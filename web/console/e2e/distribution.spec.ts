import { expect, test, type Page } from '@playwright/test'

// Distribution forms (spec 0015 phase 2b): upstreams, blocks, channels and their DuckDB versions,
// keys, tenants and DuckDB versions. e2e/run.sh made the key file b.pem for a server administrator
// to add.

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

test('an upstream: added, an allowlist entry, a dry run requested, removed', async ({ page }) => {
  await signIn(page, '/ui/t/acme/', 'tom')
  await nav(page, 'Upstreams').click()
  await page.getByRole('button', { name: 'New upstream' }).click()
  await page.getByLabel('Name', { exact: true }).fill('core')
  await page.getByLabel('Into channel').selectOption('prod')
  await page.getByLabel('Extensions').fill('json')
  await page.getByRole('button', { name: 'Add', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'core' })).toBeVisible()
  await expect(page.getByRole('cell', { name: 'json', exact: true })).toBeVisible()

  await page.getByLabel('Extension', { exact: true }).fill('parquet')
  await page.getByRole('button', { name: 'Add or replace' }).click()
  await expect(page.getByRole('cell', { name: 'parquet', exact: true })).toBeVisible()
  await page.getByLabel('Find an entry').fill('par')
  await expect(page.getByRole('cell', { name: 'json', exact: true })).toHaveCount(0)

  await page.getByRole('button', { name: 'Dry run' }).click()
  await confirm(page, 'Run')
  await expect(page.getByText(/a dry run is requested|running|dry run/).first()).toBeVisible()

  await page.getByRole('button', { name: 'Remove…' }).first().click()
  await confirm(page, 'remove')
  await expect(page.getByRole('heading', { name: 'Upstreams' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'core', exact: true })).toHaveCount(0)
})

test('a body blocked and unblocked; a channel created with a DuckDB version', async ({ page }) => {
  await signIn(page, '/ui/t/acme/', 'tom')
  await nav(page, 'Blocks').click()
  const hash = 'ab'.repeat(32)
  await page.getByLabel('Body hash').fill(hash)
  await page.getByLabel('Reason').fill('e2e')
  await page.getByRole('button', { name: 'Block…' }).click()
  await confirm(page, 'Block')
  await expect(page.getByRole('cell', { name: hash })).toBeVisible()
  await page.getByRole('button', { name: 'Unblock…' }).click()
  await confirm(page, 'Unblock')
  await expect(page.getByText('Nothing blocked.')).toBeVisible()

  await nav(page, 'Channels').click()
  await page.getByRole('button', { name: 'New channel' }).click()
  await page.getByLabel('Name').fill('beta')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByRole('heading', { name: 'beta' })).toBeVisible()
  await page.getByLabel('DuckDB version to serve').selectOption('v2.0.0')
  await page.getByRole('button', { name: 'Add', exact: true }).click()
  await expect(page.getByRole('button', { name: 'stop serving v2.0.0' })).toBeVisible()
})

test('a server administrator: a tenant created, suspended and resumed; a version; a key added and activated', async ({ page }) => {
  await signIn(page, '/ui/server/', 'root')
  await page.getByRole('button', { name: 'New tenant' }).click()
  await page.getByLabel('Name', { exact: true }).fill('gamma')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByRole('heading', { name: 'gamma' })).toBeVisible()
  await page.getByRole('button', { name: 'Suspend…' }).click()
  await confirm(page, 'Suspend')
  await expect(page.getByText('suspended', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Resume…' }).click()
  await confirm(page, 'Resume')
  await expect(page.getByText('active', { exact: true }).first()).toBeVisible()

  await nav(page, 'DuckDB versions').click()
  await page.getByLabel('Version').fill('v2.1.0')
  await page.getByLabel('Maximum C APIs').fill('v1.5.6')
  await page.getByRole('button', { name: 'Add', exact: true }).click()
  await expect(page.getByRole('cell', { name: 'v2.1.0' })).toBeVisible()

  // the channel beta the previous test made: a key added by signer reference, then activated (moved
  // to by links: a reload would drop the session, which lives in memory only)
  await nav(page, 'Tenants').click()
  await page.getByRole('link', { name: 'acme', exact: true }).click()
  await page.getByRole('link', { name: 'beta', exact: true }).click()
  await page.getByRole('button', { name: 'Keys' }).click()
  await page.getByLabel('Add a key').fill('file:b.pem')
  await page.getByRole('button', { name: 'Add key' }).click()
  await expect(page.getByText('trusted', { exact: true })).toBeVisible()
  // a key trusted for less than a week is refused, as the API words it; a server administrator forces
  await page.getByRole('button', { name: 'Activate…' }).click()
  await confirm(page, 'activate')
  await expect(page.getByText(/too soon/)).toBeVisible()
  await page.getByRole('button', { name: 'Activate…' }).click()
  await page.getByRole('dialog').getByLabel(/force/).check()
  await confirm(page, 'activate')
  await expect(page.getByRole('cell', { name: 'active', exact: true })).toBeVisible()
})
