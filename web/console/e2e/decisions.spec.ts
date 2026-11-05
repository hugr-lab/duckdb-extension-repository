import { expect, test, type Page } from '@playwright/test'

// The catalogue as the tenant's home and the decision queue (spec 0041). e2e/run.sh set up nightly
// following the distributor's publisher enterest-ci with a day's hold (which skips hello_acme, the
// tenant having it from its own source), and the tenant's atlas 1.0 and 1.1 under a manual rule in
// prod: 1.1 waits there.

const pw = (name: string) => process.env[`E2E_PW_${name.toUpperCase()}`] ?? ''

async function signIn(page: Page, path: string, user: string) {
  await page.goto(path)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await page.locator('#username').fill(user)
  await page.locator('#password').fill(pw(user))
  await page.locator('#kc-login').click()
}

const nav = (page: Page, name: string | RegExp) => page.getByRole('navigation').getByRole('link', typeof name === 'string' ? { name, exact: true } : { name })
const confirm = (page: Page, name: string) => page.getByRole('dialog').getByRole('button', { name, exact: true }).click()
const queue = (page: Page) => nav(page, /^Needs a decision/)

test("the home: the catalogue against the channels, a follow's skip in its column", async ({ page }) => {
  await signIn(page, '/ui/t/acme/', 'tom')
  // a tenant administrator's home is the catalogue
  await expect(page).toHaveURL(/\/ui\/t\/acme\/catalogue$/)
  await expect(page.getByRole('heading', { name: 'Catalogue', exact: true })).toBeVisible()
  const head = page.getByRole('row').first()
  await expect(head.getByRole('link', { name: 'nightly', exact: true })).toBeVisible()
  await expect(head.getByRole('link', { name: 'prod', exact: true })).toBeVisible()
  await expect(head.getByText(/follows publisher:enterest-ci/)).toBeVisible()
  // atlas: prod's manual rule, 1.1 waiting; nightly takes nothing of it
  const atlas = page.getByRole('row').filter({ has: page.getByRole('link', { name: 'atlas', exact: true }) })
  await expect(atlas.getByText('manual', { exact: true })).toBeVisible()
  await expect(atlas.getByText('→ waits')).toBeVisible()
  await expect(atlas.getByRole('button', { name: '+ Rule…' })).toHaveCount(1)
  await expect(page.getByRole('link', { name: /decisions? waits?/ })).toBeVisible()
})

test('a skip is kept in kista: undone, made again, remembered across a reload and seen by another administrator', async ({ browser }) => {
  test.setTimeout(120_000)
  const page = await (await browser.newContext()).newPage()
  await signIn(page, '/ui/t/acme/', 'tom')
  await queue(page).click()
  const atlas = page.getByRole('group', { name: 'atlas prod actions' })
  await expect(atlas).toBeVisible()
  // a tenant administrator holds no promote here: neither Promote nor Place is offered
  await expect(atlas.getByRole('button', { name: /Promote|Place/ })).toHaveCount(0)
  await atlas.getByRole('button', { name: 'Skip' }).click()
  await expect(atlas).toHaveCount(0)
  await page.getByRole('button', { name: /^Undo/ }).click()
  await expect(atlas).toBeVisible()
  await atlas.getByRole('button', { name: 'Skip' }).click()
  await expect(atlas).toHaveCount(0)
  // a reload ends the session (it lives in memory): signed in again, the queue is read again
  await page.reload()
  await signIn(page, '/ui/t/acme/', 'tom')
  await queue(page).click()
  await expect(page.getByRole('heading', { name: 'Needs a decision' })).toBeVisible()
  await expect(page.getByRole('group', { name: /actions$/ }).first()).toBeVisible()
  await expect(atlas).toHaveCount(0)
  // the matrix leaves the skipped version out too
  await nav(page, 'Catalogue').click()
  const row = page.getByRole('row').filter({ has: page.getByRole('link', { name: 'atlas', exact: true }) })
  await expect(row.getByText('manual', { exact: true })).toBeVisible()
  await expect(row.getByText('→ waits')).toHaveCount(0)

  // a server administrator opening the tenant sees the same queue
  const other = await (await browser.newContext()).newPage()
  await signIn(other, '/ui/server/', 'root')
  await other.getByRole('link', { name: 'acme', exact: true }).click()
  await queue(other).click()
  await expect(other.getByRole('heading', { name: 'Needs a decision' })).toBeVisible()
  await expect(other.getByRole('group', { name: /actions$/ }).first()).toBeVisible()
  await expect(other.getByRole('group', { name: 'atlas prod actions' })).toHaveCount(0)
})

test("a follow's skip dismissed; a block says what its body is", async ({ page }) => {
  test.setTimeout(150_000)
  await signIn(page, '/ui/t/acme/', 'tom')
  await queue(page).click()
  // the follow's skip is listed after the placement pass's look at the follow (every minute)
  const hello = page.getByRole('group', { name: 'hello_acme nightly actions' })
  await expect(async () => {
    await page.getByRole('button', { name: 'Refresh' }).click()
    await expect(hello).toBeVisible({ timeout: 2_000 })
  }).toPass({ timeout: 110_000 })
  await hello.getByRole('button', { name: 'Dismiss' }).click()
  await expect(hello).toHaveCount(0)
  // a reload ends the session (it lives in memory): signed in again, the queue is read again
  await page.reload()
  await signIn(page, '/ui/t/acme/', 'tom')
  await queue(page).click()
  await expect(page.getByRole('heading', { name: 'Needs a decision' })).toBeVisible()
  await expect(page.getByRole('group', { name: /actions$/ }).first()).toBeVisible()
  await expect(hello).toHaveCount(0)

  // atlas 1.0's body, from its entry's read, blocked: the block names it
  const read = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/v1/tenants/acme/catalogue/tenant/extensions/atlas')
  await nav(page, 'Catalogue').click()
  await page.getByRole('link', { name: 'atlas', exact: true }).click()
  const entry = (await (await read).json()) as { versions: { version: string; builds: { body_hash: string }[] }[] }
  const hash = entry.versions.find((v) => v.version === '1.0')!.builds[0].body_hash
  await nav(page, 'Blocks').click()
  await page.getByLabel('Body hash').fill(hash)
  await page.getByLabel('Reason').fill('e2e')
  await page.getByRole('button', { name: 'Block…' }).click()
  await confirm(page, 'Block')
  const blocked = page.getByRole('row').filter({ has: page.getByRole('cell', { name: hash }) })
  await expect(blocked.getByText('atlas 1.0, linux_amd64, tenant')).toBeVisible()
  await blocked.getByRole('button', { name: 'Unblock…' }).click()
  await confirm(page, 'Unblock')
  await expect(page.getByText('Nothing blocked.')).toBeVisible()
})
