import { expect, test, type Page } from '@playwright/test'

// Licences and usage in the console (spec 0012 phase 1d). e2e/run.sh trusts the licence vectors'
// test-only key, signed a 20-day licence for tresor (E2E_LICENSE), and serves another repository's
// .well-known (E2E_REPO_URL, its key E2E_REPO_KEY).

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

test('a licence: imported from a file, its claims read, made public, removed', async ({ page }) => {
  await signIn(page, '/ui/t/acme/', 'tom')
  await nav(page, 'Licences').click()
  await expect(page.getByText('No licences.')).toBeVisible()
  await expect(page.getByText('No upstream relays licences.')).toBeVisible()

  await page.getByLabel('licence file').setInputFiles(process.env.E2E_LICENSE ?? '')
  await expect(page.getByText('plan pro, held by Acme Test')).toBeVisible()
  await page.getByRole('button', { name: 'Import', exact: true }).click()
  const row = page.getByRole('row', { name: /tresor/ })
  await expect(row.getByText('expires in 20 days')).toBeVisible()
  await expect(row.getByText('valid', { exact: true })).toBeVisible()

  // opened on import: its claims and limits
  await expect(page.getByRole('cell', { name: 'max_attached_sources' })).toBeVisible()
  await expect(page.getByText('lic-e2e')).toBeVisible()
  await expect(page.getByText(/accepts a licence only when its SDK trusts the key/)).toBeVisible()

  // the same file again replaces it, after a confirmation
  await page.getByLabel('licence file').setInputFiles(process.env.E2E_LICENSE ?? '')
  await expect(page.getByText('It replaces the licence stored for tresor (imported).')).toBeVisible()
  await page.getByRole('button', { name: 'Replace…' }).click()
  await confirm(page, 'Replace')
  await expect(page.getByLabel('licence file')).toHaveValue('')
  await expect(page.getByRole('button', { name: 'Import', exact: true })).toBeDisabled()

  await page.getByRole('button', { name: 'Make public' }).click()
  await confirm(page, 'Make public')
  await expect(row.getByText('public', { exact: true })).toBeVisible()
  await expect(page.getByText(/and can copy it/)).toBeVisible()

  await page.getByRole('button', { name: 'Remove…' }).click()
  await confirm(page, 'Remove')
  await expect(page.getByText('No licences.')).toBeVisible()
})

test('an upstream stops and resumes forwarding usage; the Usage tab', async ({ page }) => {
  await signIn(page, '/ui/t/acme/', 'tom')
  await nav(page, 'Upstreams').click()
  await page.getByRole('button', { name: 'New upstream' }).click()
  await page.getByRole('group', { name: 'source' }).getByRole('button', { name: 'Another repository' }).click()
  await page.getByLabel('Name', { exact: true }).fill('org')
  await page.getByLabel('Into channel').selectOption('prod')
  await page.getByLabel('Mode').selectOption('pull-through')
  await page.getByLabel('Repository prefix').fill(process.env.E2E_REPO_URL ?? '')
  await page.getByLabel('Pinned keys').fill(process.env.E2E_REPO_KEY ?? '')
  await page.getByLabel('Extensions').fill('hello_org')
  await page.getByRole('button', { name: 'Add', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'org' })).toBeVisible()

  const usage = page.locator('section', { has: page.getByRole('heading', { name: 'Usage forwarding' }) })
  await expect(usage.getByText(/how the versions it supplied were used/)).toBeVisible()
  await expect(usage.getByText('Only an upstream with a credential gets usage.')).toBeVisible()
  await expect(usage.getByText('Never forwarded.')).toBeVisible()
  await usage.getByRole('button', { name: 'Stop forwarding…' }).click()
  await confirm(page, 'Stop')
  await expect(usage.getByText('off', { exact: true })).toBeVisible()
  await usage.getByRole('button', { name: 'Forward…' }).click()
  await confirm(page, 'Forward')
  await expect(usage.getByText('forward', { exact: true })).toBeVisible()

  await page.getByRole('group', { name: 'upstream actions' }).getByRole('button', { name: 'Remove…' }).click()
  await confirm(page, 'Remove')
  await expect(page.getByRole('heading', { name: 'Upstreams' })).toBeVisible()

  await nav(page, 'Statistics').click()
  await page.getByRole('group', { name: 'statistics' }).getByRole('button', { name: 'Usage' }).click()
  await expect(page.getByText('No usage reported in this range.')).toBeVisible()
  await expect(page.getByText(/Without a licence: reported with any status but valid or grace/)).toBeVisible()
})
