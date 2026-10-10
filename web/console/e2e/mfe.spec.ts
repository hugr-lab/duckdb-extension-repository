import { expect, test, type Page } from '@playwright/test'

// The micro-frontend in a platform's shell on another origin (spec 0015 phase 1b): e2e/run.sh serves
// the test platform (mfe-host) at E2E_HOST_URL and lists it in ui.allowed_origins. The platform's
// tokens come from Keycloak by password here, through Playwright: the host asks per audience.

const kista = process.env.E2E_KISTA_URL ?? ''
const kc = process.env.E2E_KC_URL ?? ''
const host = process.env.E2E_HOST_URL ?? ''
const pw = (name: string) => process.env[`E2E_PW_${name.toUpperCase()}`] ?? ''

// the platform's own mapping of kista's audiences to its IdP: it issues tokens only for these
const audiences: Record<string, { realm: string; client: string; user: string }> = {
  [`${kista}/acme`]: { realm: 'acme', client: 'kista-console', user: 'tom' },
  'api://kista-server': { realm: 'ops', client: 'kista-server-console', user: 'root' },
}

async function platform(page: Page) {
  const asked: string[] = []
  await page.exposeFunction('hostToken', async (audience: string, renew: boolean) => {
    asked.push(renew ? `${audience} renew` : audience)
    const a = audiences[audience]
    if (!a) throw new Error(`no token for ${audience}`)
    const form = new URLSearchParams({ grant_type: 'password', client_id: a.client, username: a.user, password: pw(a.user), scope: 'openid' })
    const r = await fetch(`${kc}/realms/${a.realm}/protocol/openid-connect/token`, { method: 'POST', body: form })
    if (!r.ok) throw new Error(`token: ${r.status}`)
    return (await r.json()).access_token as string
  })
  await page.goto(`${host}/platform/acme?kista=${encodeURIComponent(kista)}`)
  await expect(page.locator('#status')).toHaveText('kista contract 1')
  return asked
}

test('two consoles in one shell: a token per audience, navigation, title and theme', async ({ page }) => {
  const asked = await platform(page)
  const acme = page.locator('#acme')
  const server = page.locator('#server')
  await expect(acme.getByRole('link', { name: 'prod' })).toBeVisible()
  await expect(server.getByRole('link', { name: 'acme' })).toBeVisible()
  expect(asked).toContain(`${kista}/acme`)
  expect(asked).toContain('api://kista-server')
  // the acme console took the host's basePath over; the server console left the address alone
  await expect(page).toHaveURL(/\/platform\/acme\/channels$/)
  await expect(page.locator('#title-acme')).toHaveText('Channels')
  await expect(page.locator('#title-server')).toHaveText('Tenants')

  // its moves are the host's history: back returns, the other console untouched
  await acme.getByRole('link', { name: 'prod' }).click()
  await expect(page).toHaveURL(/\/platform\/acme\/channels\/prod$/)
  await expect(acme.getByRole('link', { name: 'hello_acme' })).toBeVisible()
  await page.goBack()
  await expect(page).toHaveURL(/\/platform\/acme\/channels$/)
  await expect(acme.getByRole('link', { name: 'prod' })).toBeVisible()
  await expect(server.getByRole('link', { name: 'acme' })).toBeVisible()

  // the theme is the host's, and so are the colours it sets on the element
  await page.click('#theme')
  await expect(acme).toHaveAttribute('data-kista-theme', 'dark')
  await page.click('#brand')
  const tab = acme.getByRole('link', { name: 'Channels' })
  await expect.poll(() => tab.evaluate((el) => getComputedStyle(el).backgroundColor)).toBe('rgb(200, 30, 90)')
})

test('a refused token is renewed once; refused again, the host is told', async ({ page }) => {
  const asked = await platform(page)
  const acme = page.locator('#acme')
  await expect(acme.getByRole('link', { name: 'prod' })).toBeVisible()

  // the cached token is refused: the console asks for a newly issued one and goes on
  await page.click('#expire')
  await acme.getByRole('link', { name: 'Events' }).click()
  await expect(acme.getByRole('heading', { name: 'Events' })).toBeVisible()
  await expect(acme.getByRole('cell', { name: 'release.add' }).first()).toBeVisible()
  expect(asked).toContain(`${kista}/acme renew`)
  expect(await page.evaluate(() => window.unauthorized)).toBe(0)

  // refused even renewed: onUnauthorized (kista-unauthorized), and the console says so
  await page.click('#revoke')
  await acme.getByRole('link', { name: 'Statistics' }).click()
  await expect(acme.getByText('Your session ended')).toBeVisible()
  await expect(page.locator('#status')).toHaveText('unauthorized ×1')

  // the host signs in again; Try again goes on where it was
  await page.click('#restore')
  await acme.getByRole('button', { name: 'Try again' }).click()
  await expect(acme.getByText('Your session ended')).toHaveCount(0)
  await expect(acme.getByRole('heading', { name: 'Downloads' }).first()).toBeVisible()
})

declare global {
  interface Window {
    unauthorized: number
  }
}
