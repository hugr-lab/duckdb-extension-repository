import { expect, test, type Page } from '@playwright/test'

// Licences and usage in the console (spec 0012 phase 1d). e2e/run.sh published vault with a key
// block declaring the licence vectors' test-only key, as a marked publisher (spec 0021), and vault
// 1.1 with the same key into the catalogue as a distributor's publisher, vault-dist (spec 0035: two
// entries declaring it), signed a 20-day licence for vault (E2E_LICENSE), and serves another
// repository's .well-known (E2E_REPO_URL, its key E2E_REPO_KEY). plain-ci is the tenant's own
// publisher, its builds declaring no key. A distributor's publisher's entry (vault-dist,
// enterest-ci) is its licence manager's and the server administrators' to write: tom reads it only
// (spec 0037); run.sh stored the licence in vault-dist's entry.

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

/** Picks the licence file and, among the entries its key is live for, one. */
async function pickInto(page: Page, entry: string) {
  await page.getByLabel('licence file').setInputFiles(process.env.E2E_LICENSE ?? '')
  await page.getByRole('radiogroup', { name: 'entry' }).getByRole('radio', { name: entry }).check()
}

test('a licence: imported from a file into an entry, its claims read, made public, removed', async ({ page }) => {
  await signIn(page, '/ui/t/acme/', 'tom')
  await nav(page, 'Licences').click()
  await expect(page.getByText('No upstream relays licences.')).toBeVisible()
  // spec 0037: the distributor's licence, read-only to tom
  const inEntry = (entry: string) => page.getByRole('row').filter({ has: page.getByRole('cell', { name: entry, exact: true }) })
  const dist = inEntry('publisher:vault-dist')
  await expect(page.getByRole('row', { name: /^vault pro/ })).toHaveCount(1)
  await dist.getByRole('button', { name: 'vault' }).click()
  await expect(page.getByText("A distributor's licence: enterest's licence manager writes it, or a server administrator.")).toBeVisible()
  await expect(page.getByRole('button', { name: 'Remove…' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: /^Make (public|private)$/ })).toHaveCount(0)
  await dist.getByRole('button', { name: 'vault' }).click() // closed again
  // spec 0035: the key, live for two entries
  const keys = page.locator('section', { has: page.getByRole('heading', { name: 'Licence keys' }) })
  const vaultKey = keys.getByRole('row', { name: /vault/ })
  await expect(vaultKey.getByText(/publisher vault-ci \(1 build\)/)).toBeVisible()
  await expect(vaultKey.getByText(/publisher vault-dist \(1 build, distributor enterest\)/)).toBeVisible()

  // the dialog lists both entries, each with its kind, and preselects neither
  await page.getByLabel('licence file').setInputFiles(process.env.E2E_LICENSE ?? '')
  await expect(page.getByText('plan pro, held by Acme Test')).toBeVisible()
  const entries = page.getByRole('radiogroup', { name: 'entry' })
  await expect(entries.getByText('several declare its key, choose one')).toBeVisible()
  await expect(entries.getByText("enterest's publisher (a distributor)")).toBeVisible()
  await expect(entries.getByRole('radio', { name: 'publisher:vault-ci' })).not.toBeChecked()
  await expect(entries.getByRole('radio', { name: 'publisher:vault-dist' })).not.toBeChecked()
  // spec 0037: the distributor's entry is listed, not tom's to choose
  await expect(entries.getByRole('radio', { name: 'publisher:vault-dist' })).toBeDisabled()
  await expect(entries.getByText("a distributor's: its licence manager writes it")).toBeVisible()
  await expect(page.getByRole('button', { name: 'Import', exact: true })).toBeDisabled()
  // an entry whose builds do not declare the key: sent, and kista's refusal shown
  await entries.getByRole('radio', { name: 'another entry' }).check()
  await entries.getByRole('textbox', { name: 'other entry' }).fill('publisher:plain-ci')
  const sent = page.waitForRequest((r) => r.method() === 'POST' && r.url().includes('/licenses?source=publisher%3Aplain-ci'))
  await page.getByRole('button', { name: 'Import', exact: true }).click()
  await sent
  await expect(page.getByRole('alert')).toContainText('kid_untrusted')
  await expect(page.getByRole('row', { name: /^vault pro/ })).toHaveCount(1)
  // spec 0037: a distributor's entry named by hand: kista refuses tom, saying why
  await entries.getByRole('textbox', { name: 'other entry' }).fill('publisher:enterest-ci')
  await page.getByRole('button', { name: 'Import', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText("a distributor's entry: its licence manager or a server administrator writes it")
  await expect(page.getByRole('row', { name: /^vault pro/ })).toHaveCount(1)

  // into vault-ci's entry: ?source= sent
  await entries.getByRole('radio', { name: 'publisher:vault-ci' }).check()
  const into = page.waitForRequest((r) => r.method() === 'POST' && r.url().includes('/licenses?source=publisher%3Avault-ci'))
  await page.getByRole('button', { name: 'Import', exact: true }).click()
  await into
  const row = inEntry('publisher:vault-ci')
  await expect(row.getByText('expires in 20 days')).toBeVisible()
  await expect(row.getByText('valid', { exact: true })).toBeVisible()
  await expect(row.getByRole('cell', { name: 'publisher:vault-ci', exact: true })).toBeVisible()
  await expect(page.getByRole('columnheader', { name: 'Entry' })).toBeVisible()
  await expect(page.getByRole('columnheader', { name: 'Of' })).toHaveCount(0)
  await expect(row.getByText('no live build of its entry declares its key')).toHaveCount(0)
  // spec 0035 phase 2: vault-ci's vault is public in prod: the import went through, with a warning,
  // shown until dismissed
  const warnings = page.getByRole('status', { name: 'Warnings' })
  await expect(warnings).toContainText('Nothing was refused')
  await expect(warnings).toContainText('publisher:vault-ci holds a licence for vault and has a public release in channel prod')
  await warnings.getByRole('button', { name: 'Dismiss' }).click()
  await expect(warnings).toHaveText('') // the region stays, empty
  await expect(page.locator('main h1')).toBeFocused() // the page's heading

  // opened on import: its claims and limits
  await expect(page.getByRole('cell', { name: 'max_attached_sources' })).toBeVisible()
  await expect(page.getByText('lic-e2e')).toBeVisible()
  await expect(page.getByText(/accepts a licence only when its own key block holds the key/)).toBeVisible()

  // the same file again, into the same entry, replaces it after a confirmation
  await pickInto(page, 'publisher:vault-ci')
  await expect(page.getByText('It replaces the licence stored for vault in publisher:vault-ci (imported).')).toBeVisible()
  await page.getByRole('button', { name: 'Replace…' }).click()
  await confirm(page, 'Replace')
  await expect(page.getByLabel('licence file')).toHaveValue('')
  await expect(page.getByRole('button', { name: 'Import', exact: true })).toBeDisabled()

  // no entry lists the key of a licence for tresor: named by hand, sent, and kista's refusal shown
  await page.getByLabel('licence file').setInputFiles(process.env.E2E_LICENSE_NOKEY ?? '')
  await expect(page.getByText("No entry's live builds declare this licence's key")).toBeVisible()
  await page.getByLabel('Into the entry').fill('publisher:vault-ci')
  await page.getByRole('button', { name: 'Import', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText('kid_untrusted')
  await page.getByLabel('licence file').setInputFiles([])

  // spec 0037: it is the distributor's licence copied: tom does not make it public
  await page.getByRole('button', { name: 'Make public' }).click()
  await confirm(page, 'Make public')
  const view = page.locator('section', { has: page.getByRole('heading', { name: 'vault', exact: true }) })
  await expect(view.getByRole('alert')).toContainText("a copy of a distributor's licence (publisher:vault-dist)")
  await expect(row.getByText('private', { exact: true })).toBeVisible()

  await page.getByRole('button', { name: 'Remove…' }).click()
  await confirm(page, 'Remove')
  await expect(row).toHaveCount(0)
  await expect(page.getByRole('row', { name: /^vault pro/ })).toHaveCount(1) // the distributor's stays
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

// Spec 0028: a server administrator gives a new tenant an expected holder, and sets and clears
// acme's; another holder than a stored licence's is refused, kista naming the licence.
test("a tenant's expected holder: given at creation, refused over another holder's licence, set and cleared", async ({ page }) => {
  await signIn(page, '/ui/server/', 'root')
  await page.getByRole('button', { name: 'New tenant' }).click()
  await page.getByLabel('Name', { exact: true }).fill('held')
  await page.getByLabel('Expected holder').fill('a'.repeat(129))
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByRole('alert')).toContainText('1 to 128 bytes')
  await page.getByLabel('Expected holder').fill('org-held')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByRole('heading', { name: 'held' })).toBeVisible()
  await expect(page.getByRole('group', { name: 'expected holder' }).getByText('org-held', { exact: true })).toBeVisible()
  await nav(page, 'Tenants').click()
  await expect(page.getByRole('cell', { name: 'org-held', exact: true })).toBeVisible()

  // acme holds a licence of holder acme
  await page.getByRole('link', { name: 'acme', exact: true }).click()
  const holder = page.getByRole('group', { name: 'expected holder' })
  await expect(holder.getByText('none: licences of any holder are stored')).toBeVisible()
  await nav(page, 'Licences').click()
  await pickInto(page, 'publisher:vault-ci')
  await page.getByRole('button', { name: 'Import', exact: true }).click()
  const ci = page.getByRole('row').filter({ has: page.getByRole('cell', { name: 'publisher:vault-ci', exact: true }) })
  await expect(ci).toBeVisible()

  await holder.getByRole('button', { name: 'Set holder…' }).click()
  await page.getByRole('dialog').getByLabel('holder id').fill('org-other')
  await confirm(page, 'Set')
  await expect(page.getByRole('alert')).toContainText('remove them first: vault (lic-e2e, holder "acme")')
  await expect(holder.getByText('none: licences of any holder are stored')).toBeVisible()

  await holder.getByRole('button', { name: 'Set holder…' }).click()
  await page.getByRole('dialog').getByLabel('holder id').fill('acme')
  await confirm(page, 'Set')
  await expect(holder.getByText('acme', { exact: true })).toBeVisible()
  await expect(page.getByRole('alert')).toHaveCount(0)

  await holder.getByRole('button', { name: 'Clear holder…' }).click()
  await confirm(page, 'Clear')
  await expect(holder.getByText('none: licences of any holder are stored')).toBeVisible()

  // the licence removed again (the distributor's stays)
  await page.getByRole('button', { name: 'Remove…' }).click()
  await confirm(page, 'Remove')
  await expect(ci).toHaveCount(0)
})
