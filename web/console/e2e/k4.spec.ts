import { expect, test, type Page } from '@playwright/test'

// Console gaps (K4): a publisher's hold set in place; a build added to the catalogue, its footer read
// in the browser; the server's reserved names and credentials; a tenant's navigation in sections and
// the scope switch. The promotion dialog is distribution.spec.ts's (it needs beta's key).

const pw = (name: string) => process.env[`E2E_PW_${name.toUpperCase()}`] ?? ''

async function signIn(page: Page, path: string, user: string) {
  await page.goto(path)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await page.locator('#username').fill(user)
  await page.locator('#password').fill(pw(user))
  await page.locator('#kc-login').click()
}

const nav = (page: Page, name: string) => page.getByRole('navigation').getByRole('link', { name, exact: true })
const confirm = (page: Page, name: string) => page.getByRole('dialog').getByRole('button', { name, exact: true }).click()

test("a publisher's hold set in place; the tenant's sections and the switch", async ({ page }) => {
  await signIn(page, '/ui/t/acme/', 'tom')
  const main = page.getByRole('navigation', { name: 'Main' })
  await expect(main.getByText('Organisation', { exact: true })).toBeVisible()
  await expect(main.getByText('Audit', { exact: true })).toBeVisible()
  await expect(main.getByRole('link', { name: 'Switch' })).toHaveAttribute('href', '/ui/')
  await nav(page, 'Publishers').click()
  await page.getByRole('link', { name: 'plain-ci', exact: true }).click()
  const hold = page.getByRole('group', { name: 'publisher hold' })
  await expect(hold.getByText('none', { exact: true })).toBeVisible()
  await hold.getByLabel('new hold').fill('2d')
  await hold.getByRole('button', { name: 'Set hold' }).click()
  await expect(hold.getByText('48h', { exact: true })).toBeVisible()
  await hold.getByLabel('new hold').fill('0')
  await hold.getByRole('button', { name: 'Set hold' }).click()
  await expect(hold.getByText('none', { exact: true })).toBeVisible()
})

test("a server administrator: reserved names, credentials, a build added to the catalogue", async ({ page }) => {
  await signIn(page, '/ui/server/', 'root')
  await nav(page, 'Reserved names').click()
  await page.getByLabel('Names').fill('k4_reserved')
  await page.getByRole('button', { name: 'Reserve', exact: true }).click()
  await expect(page.getByRole('status').getByText('Reserved: k4_reserved')).toBeVisible()
  await page.getByLabel('Find a reserved name').fill('k4_')
  await expect(page.getByRole('cell', { name: 'k4_reserved' })).toBeVisible()
  await page.getByRole('button', { name: 'Remove…' }).click()
  await confirm(page, 'Remove')
  await expect(page.getByText('Nothing matches.')).toBeVisible()

  await nav(page, 'Credentials').click()
  await expect(page.getByRole('heading', { name: 'Upstream credentials' })).toBeVisible()
  await expect(page.getByText('No credentials configured.')).toBeVisible()

  // the footer fills the version and platform, the file name the name
  await nav(page, 'Tenants').click()
  await page.getByRole('link', { name: 'acme', exact: true }).click()
  await nav(page, 'Catalogue').click()
  await page.getByRole('button', { name: 'Add a build…' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Extension file').setInputFiles(process.env.E2E_EXT ?? '')
  await expect(dialog.getByLabel('Name')).toHaveValue('k4_tool')
  await expect(dialog.getByLabel('Version')).toHaveValue('0.4')
  await expect(dialog.getByLabel('Platform')).toHaveValue('linux_amd64')
  await confirm(page, 'Add')
  await expect(page.getByRole('status').getByText('k4_tool 0.4 (linux_amd64) added')).toBeVisible()
  await expect(page.getByRole('link', { name: 'k4_tool' })).toBeVisible()
})
