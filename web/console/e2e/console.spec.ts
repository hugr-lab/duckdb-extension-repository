import { expect, test, type Page } from '@playwright/test'

// The console end to end (spec 0015): e2e/run.sh set up tenant acme (channel prod, releases tresor
// and hello_acme, issuer kc with a console client) and the server issuer ops.

const pw = (name: string) => process.env[`E2E_PW_${name.toUpperCase()}`] ?? ''
const kcHost = process.env.E2E_KC_HOST ?? ''
const kc = process.env.E2E_KC_URL ?? ''

/** Ends a user's Keycloak sessions and offline tokens (its refresh tokens stop working), with the admin API. */
async function endSessions(user: string) {
  const form = new URLSearchParams({ grant_type: 'password', client_id: 'admin-cli', username: 'admin', password: process.env.E2E_KC_ADMIN_PW ?? '' })
  const tok = (await (await fetch(`${kc}/realms/master/protocol/openid-connect/token`, { method: 'POST', body: form })).json()).access_token
  const h = { Authorization: `Bearer ${tok}` }
  const [u] = await (await fetch(`${kc}/admin/realms/acme/users?exact=true&username=${user}`, { headers: h })).json()
  const r = await fetch(`${kc}/admin/realms/acme/users/${u.id}/logout`, { method: 'POST', headers: h })
  expect(r.ok).toBe(true)
  // the console's refresh token is an offline one (offline_access): revoking the client's consent ends it
  const c = await fetch(`${kc}/admin/realms/acme/users/${u.id}/consents/kista-console`, { method: 'DELETE', headers: h })
  expect(c.ok).toBe(true)
}

async function login(page: Page, user: string) {
  await page.locator('#username').fill(user)
  await page.locator('#password').fill(pw(user))
  await page.locator('#kc-login').click()
}

async function signInTenant(page: Page, user: string) {
  await page.goto('/ui/t/acme/')
  await expect(page.getByText(`You will sign in at ${kcHost}`)).toBeVisible()
  await page.getByRole('button', { name: 'Sign in' }).click()
  await login(page, user)
}

test('the landing goes to a tenant; a tenant never redirects to its IdP without a click', async ({ page }) => {
  await page.goto('/ui/')
  await expect(page.getByText('Server administration')).toBeVisible()
  await page.getByLabel('tenant').fill('acme')
  await page.getByRole('button', { name: 'Continue' }).click()
  await expect(page).toHaveURL(/\/ui\/t\/acme\/$/)
  await expect(page.getByText(`You will sign in at ${kcHost}`)).toBeVisible()
  await page.waitForTimeout(500)
  expect(new URL(page.url()).host).not.toBe(kcHost)
})

test('an unknown tenant is not found', async ({ page }) => {
  await page.goto('/ui/t/nobody-here/')
  await expect(page.getByText('No such tenant.')).toBeVisible()
})

test('an extension administrator sees only its extension', async ({ page }) => {
  await signInTenant(page, 'eve')
  await expect(page.getByRole('link', { name: 'prod' })).toBeVisible()
  await page.getByRole('link', { name: 'prod' }).click()
  await expect(page.getByRole('link', { name: 'tresor' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'hello_acme' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Keys' })).toHaveCount(0)
})

test('a token without admin or audit has nothing to administer', async ({ page }) => {
  await signInTenant(page, 'nina')
  await expect(page.getByText('Nothing to administer here.')).toBeVisible()
})

test('a tenant administrator yanks and purges a release', async ({ page }) => {
  await signInTenant(page, 'tom')
  await page.getByRole('link', { name: 'prod' }).click()
  await page.getByRole('link', { name: 'tresor' }).click()
  await expect(page.getByRole('heading', { name: 'tresor 1.0' })).toBeVisible()
  await page.getByRole('button', { name: 'yank' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'yank' }).click()
  await expect(page.getByText('yanked', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'purge' }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByRole('button', { name: 'purge' })).toBeDisabled()
  await dialog.getByLabel('confirm name').fill('tresor')
  await dialog.getByRole('button', { name: 'purge' }).click()
  await expect(page.getByRole('heading', { name: 'prod' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'hello_acme' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'tresor' })).toHaveCount(0)
  // the purge and the yank are events
  await page.getByRole('link', { name: 'Events' }).click()
  await expect(page.getByRole('cell', { name: 'release.purge' })).toBeVisible()
  await expect(page.getByRole('cell', { name: 'release.yank' })).toBeVisible()
})

test('a server administrator sees the tenants and opens one', async ({ page }) => {
  await page.goto('/ui/server/')
  await expect(page.getByText(`You will sign in at ${kcHost}`)).toBeVisible()
  await page.getByRole('button', { name: 'Sign in' }).click()
  await login(page, 'root')
  await page.getByRole('link', { name: 'acme' }).click()
  await expect(page.getByRole('heading', { name: 'acme' })).toBeVisible()
  await page.getByRole('link', { name: 'prod' }).click()
  await expect(page.getByRole('link', { name: 'hello_acme' })).toBeVisible()
})

test('an ended session signs in again in a popup and keeps what was typed', async ({ page }) => {
  await signInTenant(page, 'tom')
  await page.getByRole('link', { name: 'Events' }).click()
  await page.getByLabel('actor').fill('kept while signing in again')
  await endSessions('tom')
  await page.waitForTimeout(11_000) // the token's renewal is due: its refresh token no longer works
  await page.getByRole('button', { name: 'Refresh' }).click()
  await expect(page.getByText('Your session has ended')).toBeVisible()
  const popup = page.waitForEvent('popup')
  await page.getByRole('alert').getByRole('button', { name: 'Sign in' }).click()
  const p = await popup
  await login(p, 'tom')
  await expect(page.getByText('Your session has ended')).toHaveCount(0)
  await expect(page.getByLabel('actor')).toHaveValue('kept while signing in again')
})
