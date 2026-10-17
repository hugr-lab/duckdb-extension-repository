import { expect, test, type Page } from '@playwright/test'
import { randomBytes } from 'node:crypto'
import { writeFileSync } from 'node:fs'

// A bundle uploaded from the console (spec 0014 phase 2): a server administrator's, on tenant
// acme's Upstreams page. e2e/run.sh set chunks of 1 MiB. The upload is stopped while a chunk hangs,
// the page reloaded, and the upload resumes with the chunks the server lacks; the file is no bundle, so its import fails, as
// the page then says.

const pw = (name: string) => process.env[`E2E_PW_${name.toUpperCase()}`] ?? ''

async function signIn(page: Page, user: string) {
  await page.goto('/ui/server/')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await page.locator('#username').fill(user)
  await page.locator('#password').fill(pw(user))
  await page.locator('#kc-login').click()
}

async function upstreamsOfAcme(page: Page) {
  await signIn(page, 'root')
  await page.getByRole('link', { name: 'acme', exact: true }).click()
  await page.getByRole('navigation').getByRole('link', { name: 'Upstreams', exact: true }).click()
  await expect(page.getByText('Upload a bundle')).toBeVisible()
}

test('a bundle uploaded in chunks, stopped, resumed after a reload, its import shown', async ({ page }, info) => {
  const path = info.outputPath('acme.kbundle')
  writeFileSync(path, randomBytes(3.5 * 1024 * 1024)) // chunks 0..3
  await upstreamsOfAcme(page)

  // chunk 2 never answers: the others are received, then the page goes
  const puts: string[] = []
  page.on('request', (r) => {
    if (r.method() === 'PUT') puts.push(new URL(r.url()).pathname.split('/').pop()!)
  })
  await page.route('**/chunks/2', () => new Promise(() => {}))
  await page.getByLabel('Bundle file').setInputFiles(path)
  await page.getByRole('button', { name: 'Upload', exact: true }).click()
  await expect.poll(() => new Set(puts).size, { timeout: 30_000 }).toBe(4)
  const card = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Upload a bundle' }) })
  await expect(card.getByRole('status')).toContainText('2.5 MiB of 3.5 MiB', { timeout: 30_000 })
  // Stop ends the chunk in flight at once
  await page.getByRole('button', { name: 'Stop' }).click()
  await expect(card.getByRole('status')).toContainText('Stopped.')
  await expect(page.getByRole('button', { name: 'Resume upload' })).toBeEnabled()
  await page.unrouteAll({ behavior: 'ignoreErrors' })

  // a reload ends the session (it lives in memory); the upload is kept by the browser
  await page.reload()
  puts.length = 0
  await upstreamsOfAcme(page)
  await expect(page.getByText(/3 of 4 chunks received\) did not finish\. Pick the same file to resume it\./)).toBeVisible()
  await page.getByLabel('Bundle file').setInputFiles(path)
  await expect(page.getByText(/it resumes with the chunks missing/)).toBeVisible()
  await page.getByRole('button', { name: 'Resume upload' }).click()
  // only the chunk missing is sent; the import fails: the file is no bundle
  await expect(card.getByRole('status')).toContainText('failed', { timeout: 30_000 })
  expect(puts).toEqual(['2'])
  await expect(page.getByText(/did not finish/)).toHaveCount(0)
  await expect(page.getByRole('cell', { name: /^failed/ }).first()).toBeVisible()

  // a failed upload is aborted from the list
  await page.getByRole('button', { name: 'Abort…' }).first().click()
  await page.getByRole('dialog').getByRole('button', { name: 'Abort', exact: true }).click()
  await expect(page.getByRole('cell', { name: 'aborted' }).first()).toBeVisible()
})
