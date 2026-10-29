import { expect, test, type Page } from '@playwright/test'

// The catalogue and a channel's rules (spec 0033 phase C). e2e/run.sh set up a distributor's
// publisher, enterest-ci, with a day's hold, which published geo_tools 0.9 and 1.0 and a hello_acme
// of its own into the catalogue; the channel nightly following it; the tenant's maps 1.0 and 1.1 in
// the catalogue only.

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
const version = (page: Page, v: string) => page.locator('section').filter({ has: page.getByRole('heading', { name: new RegExp(`^${v.replace('.', '\\.')} `) }) })

test("the catalogue: a distributor's version withdrawn, a held one placed early, a build purged", async ({ page }) => {
  // a server administrator: it withdraws a distributor's version, and places (promote)
  await signIn(page, '/ui/server/', 'root')
  await page.getByRole('link', { name: 'acme', exact: true }).click()
  await nav(page, 'Catalogue').click()
  const geo = page.getByRole('row').filter({ has: page.getByRole('link', { name: 'geo_tools', exact: true }) })
  await expect(geo.getByText('publisher:enterest-ci')).toBeVisible()
  await expect(geo.getByText('held')).toHaveCount(2)
  await geo.getByRole('link', { name: 'geo_tools' }).click()
  await expect(page.getByRole('heading', { name: 'geo_tools', exact: true })).toBeVisible()

  await page.getByRole('group', { name: '0.9 actions' }).getByRole('button', { name: 'Withdraw…' }).click()
  await page.getByRole('dialog').getByLabel('Reason').fill('CVE-2026-0001')
  await confirm(page, 'Withdraw')
  await expect(version(page, '0.9').getByText('CVE-2026-0001')).toBeVisible()
  await expect(page.getByRole('group', { name: '0.9 actions' }).getByRole('button')).toHaveCount(0)

  // held for a day: placed early into nightly
  await expect(version(page, '1.0').getByText(/^until /)).toBeVisible()
  await page.getByRole('group', { name: '1.0 actions' }).getByRole('button', { name: 'Place…' }).click()
  await page.getByRole('dialog').getByLabel('Channel').selectOption('nightly')
  await page.getByRole('dialog').getByLabel(/early/).check()
  await confirm(page, 'Place')
  await expect(version(page, '1.0').getByText('nightly', { exact: true })).toBeVisible()

  // the tenant's maps 1.1, placed nowhere: purged
  await nav(page, 'Catalogue').click()
  await page.getByLabel('source').selectOption('tenant')
  await page.getByRole('link', { name: 'maps', exact: true }).click()
  await version(page, '1.1').getByRole('button', { name: 'Purge…' }).click()
  await confirm(page, 'Purge')
  await expect(version(page, '1.1')).toHaveCount(0)
  await expect(version(page, '1.0')).toBeVisible()
})

test("a channel's rules: a mode changed, a follow and what it skips, the hold", async ({ page }) => {
  // a tenant administrator holds no promote here: it reads the catalogue, places and withdraws nothing
  await signIn(page, '/ui/t/acme/', 'tom')
  await nav(page, 'Catalogue').click()
  const channels = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/v1/tenants/acme/channels' && r.request().method() === 'GET')
  await page.getByRole('row').filter({ hasText: 'publisher:enterest-ci' }).getByRole('link', { name: 'geo_tools', exact: true }).click()
  // the entry and the channels read, what the actions are offered by
  await expect(page.getByText('Taken by')).toBeVisible()
  await channels
  await expect(version(page, '1.0')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Place…' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Withdraw…' })).toHaveCount(0)

  await nav(page, 'Channels').click()
  await page.getByRole('link', { name: 'nightly', exact: true }).click()
  await page.getByRole('button', { name: 'Sources' }).click()
  const row = page.getByRole('row').filter({ has: page.getByRole('cell', { name: 'geo_tools', exact: true }) })
  await expect(row.getByRole('cell').nth(1)).toHaveText('publisher:enterest-ci')
  await expect(row.getByText('follow', { exact: true })).toBeVisible() // the follow's rule
  await expect(row.getByText('1.0', { exact: true })).toBeVisible() // placed early (the previous test)
  await page.getByRole('group', { name: 'geo_tools source actions' }).getByRole('button', { name: 'Change…' }).click()
  await page.getByRole('dialog').getByLabel('Mode').selectOption('manual')
  await confirm(page, 'Save')
  await expect(row.getByText('manual', { exact: true })).toBeVisible()

  // the follow skips the name the tenant has from its own source
  const follows = page.locator('section', { has: page.getByRole('heading', { name: 'Follows' }) })
  const followed = follows.getByRole('row').filter({ hasText: 'publisher:enterest-ci' })
  await expect(followed.getByText('the tenant uses it from another source')).toBeVisible()
  await expect(followed.getByText('hello_acme', { exact: true })).toBeVisible()
  await followed.getByRole('button', { name: 'Unfollow…' }).click()
  await confirm(page, 'Unfollow')
  await expect(follows.getByText('Follows no source.')).toBeVisible()
  await follows.getByLabel('Follow a source').fill('publisher:enterest-ci')
  await follows.getByRole('button', { name: 'Follow', exact: true }).click()
  await expect(followed).toBeVisible()

  // the channel's hold, in days as the CLI takes them
  const hold = page.getByRole('group', { name: 'channel hold' })
  await expect(hold.getByText('none', { exact: true })).toBeVisible()
  await hold.getByLabel('new hold').fill('2d')
  await hold.getByRole('button', { name: 'Set hold' }).click()
  await expect(hold.getByText('48h', { exact: true })).toBeVisible()
  await hold.getByLabel('new hold').fill('0')
  await hold.getByRole('button', { name: 'Set hold' }).click()
  await expect(hold.getByText('none', { exact: true })).toBeVisible()
})

test("an entry's icon and summary: the distributor's shown, the tenant's own edited", async ({ page }) => {
  // e2e/run.sh: enterest-ci set geo_tools' icon and summary with a signed request (spec 0036)
  await signIn(page, '/ui/t/acme/', 'tom')
  await nav(page, 'Catalogue').click()
  const geo = page.getByRole('row').filter({ has: page.getByRole('link', { name: 'geo_tools', exact: true }) })
  await expect(geo.getByText('Geometry tools for maps')).toBeVisible()
  await expect(geo.locator('img')).toHaveAttribute('src', /^blob:/)
  await geo.getByRole('link', { name: 'geo_tools' }).click()
  const geoTitle = page.getByRole('heading', { name: 'geo_tools', exact: true })
  await expect(geoTitle).toBeVisible()
  await expect(page.getByText('Geometry tools for maps')).toBeVisible()
  await expect(geoTitle.locator('img')).toHaveAttribute('width', '64')
  await expect(geoTitle.locator('img')).toHaveAttribute('src', /^blob:/)
  // a distributor's entry: no Edit for the tenant's administrators
  await expect(page.getByRole('button', { name: 'Edit…' })).toHaveCount(0)

  // the tenant's own maps: a summary and an icon
  await nav(page, 'Catalogue').click()
  await page.getByLabel('source').selectOption('tenant')
  await page.getByRole('link', { name: 'maps', exact: true }).click()
  const mapsIcon = page.getByRole('heading', { name: 'maps', exact: true }).locator('img')
  await page.getByRole('button', { name: 'Edit…' }).click()
  await page.getByRole('dialog').getByLabel('Summary').fill('Maps <b>for</b> the tenant')
  await page.getByRole('dialog').getByLabel('Icon').setInputFiles(process.env.E2E_ICON ?? '')
  await confirm(page, 'Save')
  await expect(page.getByText('Maps <b>for</b> the tenant')).toBeVisible() // text, never markup
  await expect(mapsIcon).toHaveAttribute('src', /^blob:/)
  // the summary alone: the icon stays, sent back by its hash (nothing uploaded)
  await page.getByRole('button', { name: 'Edit…' }).click()
  await page.getByRole('dialog').getByLabel('Summary').fill('Maps for the tenant')
  const put = page.waitForRequest((r) => r.method() === 'PUT' && new URL(r.url()).pathname.endsWith('/extensions/maps/meta'))
  await confirm(page, 'Save')
  const sent = (await put).postDataJSON() as { summary: string; icon: { sha256?: string; data?: string } }
  expect(sent.summary).toBe('Maps for the tenant')
  expect(sent.icon.sha256).toMatch(/^[0-9a-f]{64}$/)
  expect(sent.icon.data).toBeUndefined()
  await expect(page.getByText('Maps for the tenant', { exact: true })).toBeVisible()
  await expect(mapsIcon).toHaveAttribute('src', /^blob:/)
})
