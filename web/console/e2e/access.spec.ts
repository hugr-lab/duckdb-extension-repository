import { expect, test, type Page } from '@playwright/test'

// Access and identity forms (spec 0015 phase 2a): e2e/run.sh set up the extension group platform
// (tresor), administered by role:kc|platform-team (gina).

const pw = (name: string) => process.env[`E2E_PW_${name.toUpperCase()}`] ?? ''
const kc = process.env.E2E_KC_URL ?? ''

async function signIn(page: Page, user: string) {
  await page.goto('/ui/t/acme/')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await page.locator('#username').fill(user)
  await page.locator('#password').fill(pw(user))
  await page.locator('#kc-login').click()
}

const nav = (page: Page, name: string) => page.getByRole('navigation').getByRole('link', { name, exact: true })

test('a group, a grant on it, and who may use an extension through it', async ({ page }) => {
  await signIn(page, 'tom')
  await nav(page, 'Groups').click()
  await page.getByRole('link', { name: 'platform', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'platform' })).toBeVisible()

  // a member added by name
  await page.getByLabel('Add extensions by name').fill('hello_acme')
  await page.getByRole('button', { name: /^Add/ }).click()
  await expect(page.getByRole('status')).toHaveText('Added hello_acme.')
  await expect(page.getByRole('cell', { name: 'hello_acme', exact: true })).toBeVisible()

  // a grant on the group, to a role
  await page.getByRole('button', { name: 'Grant on this group' }).click()
  await page.getByLabel('Principal').fill('role:kc|analysts')
  await page.getByRole('button', { name: 'Grant', exact: true }).click()
  await expect(page.getByRole('cell', { name: /role:kc\|analysts/ })).toBeVisible()

  // the extension's page: the role reaches hello_acme through the group, revoked elsewhere
  await nav(page, 'Channels').click()
  await page.getByRole('link', { name: 'prod' }).click()
  await page.getByRole('link', { name: 'hello_acme' }).click()
  const row = page.getByRole('row', { name: /role:kc\|analysts/ })
  await expect(row.getByText('through the group')).toBeVisible()
  await expect(row.getByText('install', { exact: true })).toBeVisible()

  // Access: the principal, its grant, revoked
  await nav(page, 'Access').click()
  await page.getByRole('button', { name: /role:kc\|analysts/ }).click()
  await expect(page.getByText('a role', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Revoke…' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Revoke' }).click()
  await expect(page.getByText('Nothing granted.')).toBeVisible()
})

test("a group's administrator reads the group without its forms", async ({ page }) => {
  await signIn(page, 'gina')
  await expect(nav(page, 'Groups')).toBeVisible()
  await expect(nav(page, 'Access')).toHaveCount(0)
  await nav(page, 'Groups').click()
  await page.getByRole('link', { name: 'platform', exact: true }).click()
  await expect(page.getByRole('cell', { name: 'tresor', exact: true })).toBeVisible()
  await expect(page.getByText('Add extensions by name')).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Delete…' })).toHaveCount(0)
})

test('a publisher with an API key shown once, and an issuer record, added and removed', async ({ page }) => {
  await signIn(page, 'tom')
  await nav(page, 'Publishers').click()
  await page.getByLabel('New publisher').fill('ci-test')
  await page.getByRole('button', { name: 'Add', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'ci-test' })).toBeVisible()
  await page.getByRole('button', { name: '+ API key' }).click()
  await expect(page.getByText('The key, shown this once')).toBeVisible()
  await page.getByRole('button', { name: 'I have copied it' }).click()
  await page.getByRole('button', { name: 'Revoke…' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Revoke' }).click()
  await expect(page.getByText('No API keys.')).toBeVisible()
  await page.getByRole('button', { name: 'Remove…' }).first().click()
  await page.getByRole('dialog').getByRole('button', { name: 'Remove' }).click()
  await expect(page.getByRole('heading', { name: 'Publishers' })).toBeVisible()

  await nav(page, 'Identity').click()
  await page.getByRole('button', { name: 'Add an issuer' }).click()
  await page.getByLabel('Name').fill('ops2')
  await page.getByLabel('Issuer URL').fill(`${kc}/realms/ops`)
  await page.getByLabel('Required claims').fill('azp=kista-server-console')
  await page.getByRole('button', { name: 'Add', exact: true }).click()
  const card = page.locator('section', { has: page.getByRole('heading', { name: 'ops2' }) })
  await expect(card).toBeVisible()
  await card.getByRole('button', { name: 'Remove…' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Remove' }).click()
  await expect(page.getByRole('heading', { name: 'ops2' })).toHaveCount(0)
})
