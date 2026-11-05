import { expect, test, type Page } from '@playwright/test'

// A channel's retention (spec 0042). e2e/run.sh set up vintage with dial 1.0, 1.1 and 1.2, its rule
// keep 2 for 720h, and ran the pass (dial 1.0 retired, in its grace), then added dial 2.0, a C API
// build, newest and current: 1.1 is retired next. A tenant administrator opens its Retention tab,
// pins 1.1, keeps 1 (1.2 retired next), restores 1.0, retires the line, restores a release from its
// page, and sets the tenant's default, which the pipeline's stage cards show (the order is set here
// when no earlier test set it).

const pw = (name: string) => process.env[`E2E_PW_${name.toUpperCase()}`] ?? ''

async function signIn(page: Page, path: string, user: string) {
  await page.goto(path)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await page.locator('#username').fill(user)
  await page.locator('#password').fill(pw(user))
  await page.locator('#kc-login').click()
}

const nav = (page: Page, name: string) => page.getByRole('navigation').getByRole('link', { name, exact: true })
const card = (page: Page, title: string) => page.locator('section', { has: page.getByRole('heading', { name: title, exact: true }) })
const openDialog = (page: Page) => page.getByRole('dialog')

test("the Retention tab: a pin, the rule changed, a restore, the line retired; the stage cards' retention", async ({ page }) => {
  await signIn(page, '/ui/t/acme/', 'tom')
  await nav(page, 'Channels').click()
  await page.getByRole('link', { name: 'vintage', exact: true }).click()
  await page.getByRole('group', { name: 'channel sections' }).getByRole('button', { name: 'Retention' }).click()
  await expect(page.getByLabel('retention in force')).toHaveText('keep 2 · grace 720h')
  const retired = card(page, 'Retired, in the grace')
  await expect(retired.getByRole('row').filter({ hasText: 'dial 1.0' })).toBeVisible()
  await expect(card(page, 'Retire').getByRole('row').filter({ hasText: '1.1' })).toBeVisible()
  // the C API build serves v2.0.0 by the version's C API maxima: the line's current
  const lines = card(page, 'Versions by line').getByRole('row').filter({ hasText: 'v2.0.0' })
  await expect(lines.getByRole('cell').nth(2)).toHaveText('2.0')

  // a pin keeps 1.1 whatever the rule
  const pin = page.getByRole('group', { name: 'pin a version' })
  await pin.getByLabel('pin name').fill('dial')
  await pin.getByLabel('pin version').fill('1.1')
  await pin.getByLabel('pin reason').fill('LTS nodes run it')
  await pin.getByRole('button', { name: 'Pin' }).click()
  await expect(card(page, 'Pins').getByText('LTS nodes run it')).toBeVisible()
  await expect(card(page, 'Retire').getByText('Nothing to retire.')).toBeVisible()

  // keep 1: 1.2 is retired next (2.0 is newest and current, 1.1 pinned)
  const rule = page.getByRole('group', { name: "the channel's rule" })
  await rule.getByLabel('keep').fill('1')
  await rule.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByLabel('retention in force')).toHaveText('keep 1 · grace 720h')
  await expect(card(page, 'Retire').getByRole('row').filter({ hasText: '1.2' })).toBeVisible()

  // a restore in the grace, confirmed: 1.0 back, and retired next again (keep 1)
  await retired.getByRole('row').filter({ hasText: 'dial 1.0' }).getByRole('button', { name: 'Restore' }).click()
  await expect(openDialog(page).getByText(/until the next pass unless the rule or a pin keeps it/)).toBeVisible()
  await openDialog(page).getByRole('button', { name: 'Restore', exact: true }).click()
  await expect(retired.getByText('No retired release in its grace.')).toBeVisible()
  await expect(card(page, 'Retire').getByRole('row').filter({ hasText: '1.0' })).toBeVisible()

  // the line retired: every version, current and pinned included
  await page.getByRole('button', { name: 'Retire the line…' }).click()
  await openDialog(page).getByLabel('line to retire').fill('v2.0.0')
  await expect(openDialog(page).getByLabel("the line act's scope")).toContainText('restorable for 720h, then purged')
  await openDialog(page).getByRole('button', { name: 'Retire the line', exact: true }).click()
  await expect(page.getByLabel("the line act's outcome")).toHaveText(/^4 retired, 0 kept by another DuckDB version/)
  await expect(retired.getByRole('row').filter({ hasText: 'dial 1.1' }).getByText('never while pinned')).toBeVisible()

  // a retired release's page offers Restore
  await retired.getByRole('link', { name: 'dial 1.2' }).click()
  await expect(page.getByText('retired', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'restore', exact: true }).click()
  await openDialog(page).getByRole('button', { name: 'restore', exact: true }).click()
  await expect(page.getByText('active', { exact: true })).toBeVisible()

  // the tenant's default: what the pipeline's stage cards show for the channels without their own
  await page.goBack()
  await page.getByRole('group', { name: 'channel sections' }).getByRole('button', { name: 'Retention' }).click()
  const def = page.getByRole('group', { name: "the tenant's default rule" })
  await def.getByLabel('default keep').fill('3')
  await def.getByLabel('default grace').fill('30d')
  await def.getByRole('button', { name: 'Set the default' }).click()
  await expect(page.getByText('Tenant default: keep 3 · grace 720h')).toBeVisible()
  await nav(page, 'Channels').click()
  const setOrder = page.getByRole('button', { name: 'Set the order…' })
  const view = page.getByRole('group', { name: 'Channels view' })
  await expect(setOrder.or(view)).toBeVisible()
  if (await setOrder.isVisible()) { // no earlier test set the order: nightly → staging → prod
    await setOrder.click()
    await openDialog(page).getByLabel('next of nightly').selectOption('staging')
    await openDialog(page).getByLabel('next of staging').selectOption('prod')
    await openDialog(page).getByRole('button', { name: 'Save', exact: true }).click()
  }
  await view.getByRole('button', { name: 'Pipeline' }).click()
  await expect(page.getByRole('group', { name: 'stage staging' }).getByLabel('retention')).toHaveText('retention: keep 3 · grace 720h')
})
