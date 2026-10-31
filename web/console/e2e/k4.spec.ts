import { expect, test, type Page } from '@playwright/test'

// Console gaps (K4): a publisher's and an eager mirror's holds set in place; a build added to the
// catalogue, its footer read in the browser; the server's reserved names (forced over a tenant's
// releases) and credentials (run.sh configures one); a new channel's first key, active at once; a
// tenant's navigation in sections and the scope switch. The promotion dialog is distribution.spec.ts's
// (it needs beta's key).

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
  await expect(main.getByRole('group', { name: 'Organisation' }).getByRole('link', { name: 'Publishers' })).toBeVisible()
  await expect(main.getByRole('group', { name: 'Audit' }).getByRole('link', { name: 'Events' })).toBeVisible()
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

  // a mirror into prod, a signed channel, fetching eagerly: its hold is set on its page with its ETag
  await nav(page, 'Upstreams').click()
  await page.getByRole('button', { name: 'New upstream' }).click()
  await page.getByLabel('Name', { exact: true }).fill('k4mirror')
  await page.getByLabel('Into channel').selectOption('prod')
  await page.getByLabel('Extensions').fill('json')
  await page.getByRole('button', { name: 'Add', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'k4mirror' })).toBeVisible()
  const mirror = page.getByRole('group', { name: 'upstream hold' })
  await expect(mirror.getByText('none', { exact: true })).toBeVisible()
  await mirror.getByLabel('new hold').fill('2h')
  await mirror.getByRole('button', { name: 'Set hold' }).click()
  await expect(mirror.getByText('2h', { exact: true })).toBeVisible()
  await page.getByRole('group', { name: 'upstream actions' }).getByRole('button', { name: 'Remove…' }).click()
  await confirm(page, 'Remove')
  await expect(page.getByRole('heading', { name: 'Upstreams' })).toBeVisible()
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

  // hello_acme has acme's releases: refused, then forced (the releases stay), then removed again
  await page.getByLabel('Names').fill('hello_acme')
  await page.getByRole('button', { name: 'Reserve', exact: true }).click()
  const force = page.getByRole('checkbox', { name: /force/ })
  await force.check()
  await page.getByRole('button', { name: 'Reserve', exact: true }).click()
  await expect(page.getByRole('status').getByText(/tenants keeping releases: hello_acme: acme/)).toBeVisible()
  await expect(force).toHaveCount(0)
  await page.getByLabel('Find a reserved name').fill('hello_')
  await page.getByRole('button', { name: 'Remove…' }).click()
  await confirm(page, 'Remove')
  await expect(page.getByText('Nothing matches.')).toBeVisible()

  await nav(page, 'Credentials').click()
  await expect(page.getByRole('heading', { name: 'Upstream credentials' })).toBeVisible()
  const cred = page.getByRole('row').filter({ has: page.getByRole('cell', { name: 'k4-cred', exact: true }) })
  await expect(cred.getByText('token_file')).toBeVisible()
  await expect(cred.getByText('every tenant')).toBeVisible()
  await expect(cred.getByText('https://ext.example/acme/')).toBeVisible()
  await expect(page.getByText('e2e-only')).toHaveCount(0)

  // a new channel's first key, active at once by default
  await nav(page, 'Tenants').click()
  await page.getByRole('link', { name: 'acme', exact: true }).click()
  await page.getByRole('link', { name: 'Channels' }).first().click()
  await page.getByRole('button', { name: 'New channel' }).click()
  await page.getByLabel('Name', { exact: true }).fill('k4keys')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByRole('heading', { name: 'k4keys' })).toBeVisible()
  await page.getByRole('button', { name: 'Keys' }).click()
  await page.getByLabel("Add the channel's first key").fill('file:k.pem')
  await expect(page.getByLabel('active at once')).toBeChecked()
  await page.getByRole('button', { name: 'Add key' }).click()
  await expect(page.getByRole('cell', { name: 'active', exact: true })).toBeVisible()
  await expect(page.getByText('Active: none')).toHaveCount(0)

  // the footer fills the version and platform, the file name the name
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
