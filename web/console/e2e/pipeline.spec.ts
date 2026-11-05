import { expect, test, type Page } from '@playwright/test'

// The channels' order and the pipeline (spec 0043). e2e/run.sh set up staging beside nightly and
// prod, and the tenant's relay and beacon 1.0 placed into nightly, staging's manual rules for them
// with nothing yet. A server administrator opening the tenant (who may promote) sets the order
// nightly → staging → prod, sees both wait for staging, promotes relay after its Report & licence
// panel, and skips beacon.

const pw = (name: string) => process.env[`E2E_PW_${name.toUpperCase()}`] ?? ''

async function signIn(page: Page, path: string, user: string) {
  await page.goto(path)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await page.locator('#username').fill(user)
  await page.locator('#password').fill(pw(user))
  await page.locator('#kc-login').click()
}

const dialog = (page: Page) => page.getByRole('dialog')

test('the pipeline: the order set, a version waiting for staging promoted after its report and licence, another skipped', async ({ page }) => {
  await signIn(page, '/ui/server/', 'root')
  await page.getByRole('link', { name: 'acme', exact: true }).click()
  await page.getByRole('navigation').getByRole('link', { name: 'Channels', exact: true }).click()
  // no order yet: the list, and Set the order…
  await expect(page.getByRole('button', { name: 'Pipeline' })).toHaveCount(0)
  await page.getByRole('button', { name: 'Set the order…' }).click()
  await dialog(page).getByLabel('next of nightly').selectOption('staging')
  await dialog(page).getByLabel('next of staging').selectOption('prod')
  await dialog(page).getByRole('button', { name: 'Save', exact: true }).click()
  await page.getByRole('button', { name: 'Pipeline' }).click()
  await expect(page.getByRole('group', { name: /^stage / })).toHaveText([/nightly/, /staging/, /prod/])
  await expect(page.getByText('2 versions wait for staging')).toBeVisible()

  // relay 1.0: its Report & licence panel, then Promote from nightly, filled in
  const relay = page.getByRole('row').filter({ hasText: 'relay' }).filter({ has: page.getByRole('button', { name: 'Report & licence' }) })
  await relay.getByRole('button', { name: 'Report & licence' }).click()
  const panel = page.getByRole('region', { name: 'relay report and licence' })
  await expect(panel.getByText(/is nightly's current on v2.0.0 linux_amd64/)).toBeVisible()
  await expect(panel.getByText('No licence in this entry.')).toBeVisible()
  await panel.getByRole('button', { name: 'Promote…' }).click()
  await expect(dialog(page).getByText('Promote relay from nightly')).toBeVisible()
  await expect(dialog(page).getByLabel('To channel')).toHaveValue('staging')
  await expect(dialog(page).getByLabel('Version', { exact: true })).toHaveValue('1.0')
  await dialog(page).getByRole('button', { name: 'Promote', exact: true }).click()
  await expect(page.getByRole('status').getByText('1.0 promoted to staging')).toBeVisible()
  await expect(page.getByText('1 version waits for staging')).toBeVisible()

  // beacon 1.0 skipped, from the panel: nothing waits for staging any more
  const beacon = page.getByRole('row').filter({ hasText: 'beacon' }).filter({ has: page.getByRole('button', { name: 'Report & licence' }) })
  await beacon.getByRole('button', { name: 'Report & licence' }).click()
  await page.getByRole('region', { name: 'beacon report and licence' }).getByRole('button', { name: 'Skip', exact: true }).click()
  await dialog(page).getByRole('button', { name: 'Skip', exact: true }).click()
  await expect(page.getByRole('status').getByText('beacon 1.0 skipped in staging')).toBeVisible()
  await expect(page.getByText('Nothing waits for staging')).toBeVisible()

  // the catalogue's columns follow the order, then the other channels (earlier tests' beta, …) by name
  await page.getByRole('navigation').getByRole('link', { name: 'Catalogue', exact: true }).click()
  const heads = page.getByRole('row').first().getByRole('link')
  await expect(heads.first()).toHaveText('nightly')
  await expect(heads.nth(1)).toHaveText('staging')
  await expect(heads.nth(2)).toHaveText('prod')
})
