import { readFileSync } from 'node:fs'
import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'
import { fileURLToPath } from 'node:url'
import { expect, test, type Frame, type Page } from '@playwright/test'
import ts from 'typescript'

// A report's page in a browser (spec 0045): the issuer's html is hostile input, and each property
// that closes it is proved here, one test per property, standalone (under the console's own
// policy) and embedded (the micro-frontend in a host's page, under a host policy with frame-src
// 'none'). Nothing is asked of kista or of an identity provider: three small servers here stand for
// kista's console, a host and the rest of the world, so the tests also run alone (npx playwright test e2e/report-frame.spec.ts). The frame is built with
// the console's own prefix (src/lib/report.ts) and stylesheet (src/report.css), in an iframe with the
// attributes ReportFrame gives it (src/lib/report.test.tsx holds the component to them).

// a Chromium already on this machine, for a run without a download (E2E_CHROMIUM: its executable)
if (process.env.E2E_CHROMIUM) test.use({ launchOptions: { executablePath: process.env.E2E_CHROMIUM } })

const src = (name: string) => readFileSync(fileURLToPath(new URL(`../src/${name}`, import.meta.url)), 'utf8')
const reportCss = src('report.css')
const reportJs = ts.transpileModule(src('lib/report.ts'), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } }).outputText

// three origins on this machine: kista (the console and its stylesheet), a host that embeds the
// console, and everything an issuer's page might reach for
let kista = ''
let host = ''
let evil = ''
let sheet = ''
let seen: string[] = [] // what reached the third origin
let pageHTML = ''
const servers: Server[] = []

const pixel = 'data:image/gif;base64,R0lGODlhAQABAAAAACwAAAAAAQABAAACAkQBADs='

// the console's own policy (web/console/console.go, held by its test), for a tenant without an IdP
const consolePolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; font-src 'self'; connect-src 'self'; " +
  "object-src 'none'; frame-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"
// a host that embeds the console: kista's module and stylesheet allowed, and no frame navigated
const hostPolicy = () => `default-src 'self'; script-src 'self' ${kista}; style-src 'self' ${kista}; img-src 'self' data: blob:; connect-src 'self' ${kista}; ` +
  "object-src 'none'; frame-src 'none'; base-uri 'self'"

// an issuer's page that tries everything
const hostile = (refresh = true) => `<h1 id="title">Report</h1>
<p id="plain">A paragraph.</p>
<table><tr><th id="th">Finding</th></tr><tr><td>none</td></tr></table>
<script>document.documentElement.setAttribute('data-script-ran', '1'); fetch('${evil}/script')</script>
<img id="pixel" src="${pixel}" onload="document.documentElement.setAttribute('data-handler-ran', '1'); new Image().src = '${evil}/handler'">
<form id="form" action="${evil}/form" method="get"><input name="q" value="1"><button id="submit" type="submit">Send</button></form>
${refresh ? `<meta http-equiv="refresh" content="0;url=${evil}/refresh">` : ''}
<img src="${evil}/image.png"><link rel="stylesheet" href="${evil}/style.css">
<style>@font-face { font-family: x; src: url(${evil}/font.woff2) } #block { color: rgb(255, 0, 0); background: url(${evil}/background.png) } body { font-family: x }</style>
<video src="${evil}/video.mp4"></video><audio src="${evil}/audio.mp3"></audio><iframe src="${evil}/frame"></iframe><object data="${evil}/object"></object>
<p id="inline" style="color: rgb(255, 0, 0); background-image: url(${evil}/inline.png)">Inline.</p><p id="block">Block.</p>
<a id="link" href="${evil}/link">a link</a> <a id="top" href="${evil}/top" target="_top">to the top</a> <a id="blank" href="${evil}/blank" target="_blank">a new window</a>
<base href="${evil}/base/">`

const moduleJs = `${reportJs}
export function show(root, html, stylesheet) {
  const theme = () => (document.documentElement.dataset.kistaTheme === 'dark' ? 'dark' : 'light')
  const frame = document.createElement('iframe')
  frame.setAttribute('sandbox', '')
  frame.setAttribute('referrerpolicy', 'no-referrer')
  frame.title = 'page'
  frame.id = 'report'
  frame.width = '900'
  frame.height = '600'
  frame.srcdoc = reportSrcdoc(html, stylesheet, theme())
  root.appendChild(frame)
  // as ReportFrame does: the prefix carries the console's theme, and follows it
  new MutationObserver(() => { frame.srcdoc = reportSrcdoc(html, stylesheet, theme()) })
    .observe(document.documentElement, { attributes: true, attributeFilter: ['data-kista-theme'] })
}
`

const theme = ":root { color-scheme: light } :root[data-kista-theme='dark'] { color-scheme: dark; background: #061c28 }"

function serve(handler: (path: string) => { type: string; body: string; headers?: Record<string, string> } | undefined): Promise<string> {
  return new Promise((resolve) => {
    const s = createServer((req, res) => {
      const a = handler(req.url ?? '/')
      if (!a) {
        res.writeHead(404).end('not found')
        return
      }
      res.writeHead(200, { 'Content-Type': a.type, 'Access-Control-Allow-Origin': '*', 'Cache-Control': 'no-store', ...a.headers }).end(a.body)
    })
    servers.push(s)
    s.listen(0, '127.0.0.1', () => resolve(`http://127.0.0.1:${(s.address() as AddressInfo).port}`))
  })
}

test.beforeAll(async () => {
  evil = await serve((path) => {
    seen.push(path)
    return { type: 'text/html', body: '<title>evil</title><p id="evil">reached</p>' }
  })
  const shell = (policy: string, css: string, js: string) => ({ type: 'text/html', headers: { 'Content-Security-Policy': policy },
    body: `<!doctype html><html data-kista-theme="light"><head><link rel="stylesheet" href="${css}"></head><body><div id="root"></div><script type="module" src="${js}"></script></body></html>` })
  const start = () => ({ type: 'text/javascript', body: `import { show } from '${kista}/ui/report-frame.js'\nshow(document.getElementById('root'), ${JSON.stringify(pageHTML)}, '${sheet}')\n` })
  kista = await serve((path) => {
    switch (path) {
      case '/ui/assets/report-0045.css': return { type: 'text/css', body: reportCss }
      case '/ui/report-frame.js': return { type: 'text/javascript', body: moduleJs }
      case '/ui/console.css': return { type: 'text/css', body: theme }
      case '/ui/start.js': return start()
      case '/ui/t/acme/decisions': return shell(consolePolicy, '/ui/console.css', '/ui/start.js')
      case '/ui/make': return { type: 'text/html', body: '<!doctype html><script type="module">import { reportFile } from "/ui/report-frame.js"\nwindow.made = reportFile(await window.reportHTML(), await window.reportCSS())</script>' }
      case '/file/report.html': return { type: 'text/html; charset=utf-8', body: pageHTML }
    }
    return undefined
  })
  host = await serve((path) => {
    switch (path) {
      case '/host.css': return { type: 'text/css', body: theme }
      case '/start.js': return start()
      case '/platform': return shell(hostPolicy(), '/host.css', '/start.js')
    }
    return undefined
  })
  sheet = `${kista}/ui/assets/report-0045.css`
})

test.afterAll(() => {
  for (const s of servers) s.close()
})

test.beforeEach(() => {
  seen = []
})

interface World {
  page: Page
  frame: () => Promise<Frame>
}

/** Opens the console's page (or the host's) with the report's frame in it. */
async function world(page: Page, mode: 'standalone' | 'embedded', html = hostile()): Promise<World> {
  pageHTML = html
  await page.goto(mode === 'standalone' ? `${kista}/ui/t/acme/decisions` : `${host}/platform`)
  await expect(page.locator('#report')).toBeVisible()
  const frame = async () => {
    const el = await page.locator('#report').elementHandle()
    const f = await el!.contentFrame()
    if (!f) throw new Error('no frame')
    return f
  }
  // the page is there, as the issuer wrote it
  await expect((await frame()).locator('#title')).toHaveText('Report')
  return { page, frame }
}

/** Long enough for a refresh, a load or a navigation that was going to happen. */
const settle = (page: Page) => page.waitForTimeout(1500)

for (const mode of ['standalone', 'embedded'] as const) {
  test.describe(`a report's page, ${mode}`, () => {
    test('no script runs', async ({ page }) => {
      const w = await world(page, mode)
      await settle(page)
      const f = await w.frame()
      expect(await f.locator('html').getAttribute('data-script-ran')).toBeNull()
      expect(await f.locator('html').getAttribute('data-handler-ran')).toBeNull()
      expect(seen.filter((u) => /script|handler/.test(u))).toEqual([])
    })

    test('no form submits', async ({ page }) => {
      const w = await world(page, mode)
      await (await w.frame()).locator('#submit').click()
      await settle(page)
      expect(seen.filter((u) => u.includes('/form'))).toEqual([])
      await expect((await w.frame()).locator('#title')).toHaveText('Report') // still the report
      expect(page.url().startsWith(evil)).toBe(false)
    })

    test('no meta refresh', async ({ page }) => {
      const w = await world(page, mode)
      await settle(page)
      expect(seen.filter((u) => u.includes('/refresh'))).toEqual([])
      await expect((await w.frame()).locator('#title')).toHaveText('Report')
      expect(page.url().startsWith(evil)).toBe(false)
    })

    test('no external request: image, CSS url(), font, stylesheet, media, frame, object', async ({ page }) => {
      const w = await world(page, mode)
      await settle(page)
      expect(seen).toEqual([])
      // an image from a data: URL is the one thing the page may show
      expect(await (await w.frame()).locator('#pixel').evaluate((img: HTMLImageElement) => img.naturalWidth)).toBe(1)
    })

    test('no inline style applies', async ({ page }) => {
      const w = await world(page, mode)
      const f = await w.frame()
      const color = (id: string) => f.locator(id).evaluate((el) => getComputedStyle(el).color)
      const plain = await color('#plain')
      expect(plain).not.toBe('rgb(255, 0, 0)')
      expect(await color('#inline')).toBe(plain) // a style attribute
      expect(await color('#block')).toBe(plain) // a style element
      expect(await f.locator('#inline').evaluate((el) => getComputedStyle(el).backgroundImage)).toBe('none')
    })

    test("kista's stylesheet applies, in both themes", async ({ page }) => {
      const w = await world(page, mode)
      const look = async () => {
        const f = await w.frame()
        return f.locator('html').evaluate((el) => {
          const th = el.ownerDocument.getElementById('th')!
          return { background: getComputedStyle(el).backgroundColor, ink: getComputedStyle(el).color, th: getComputedStyle(th).backgroundColor }
        })
      }
      expect(await look()).toEqual({ background: 'rgb(255, 255, 255)', ink: 'rgb(11, 31, 42)', th: 'rgb(242, 247, 247)' })
      // the console's theme is named on its root; the frame's prefix carries it
      await page.evaluate(() => { document.documentElement.dataset.kistaTheme = 'dark' })
      await expect.poll(look).toEqual({ background: 'rgb(6, 28, 40)', ink: 'rgb(232, 241, 242)', th: 'rgb(10, 49, 70)' })
    })

    test('a link does not navigate: the frame, the top, a new window', async ({ page, context }) => {
      for (const id of ['#link', '#top', '#blank']) {
        const w = await world(page, mode)
        const at = page.url()
        await (await w.frame()).locator(id).click()
        await settle(page)
        // its target is never asked for, and is shown nowhere: the frame keeps the report, or (a
        // navigation of the frame itself, refused by frame-src) the browser's own blocked page
        expect(seen.filter((u) => /\/(link|top|blank)/.test(u)), id).toEqual([])
        expect(await (await w.frame()).locator('#evil').count(), id).toBe(0)
        expect((await w.frame()).url().startsWith(evil), id).toBe(false)
        expect(page.url(), id).toBe(at)
        expect(context.pages(), id).toHaveLength(1)
      }
    })
  })
}

/** The file the console saves for a page (reportFile), made by the console's own code. */
async function download(page: Page, html: string) {
  await page.exposeFunction('reportHTML', () => html)
  await page.exposeFunction('reportCSS', () => reportCss)
  await page.goto(`${kista}/ui/make`)
  await page.waitForFunction(() => typeof (window as unknown as { made?: string }).made === 'string')
  const file = await page.evaluate(() => (window as unknown as { made: string }).made)
  expect(file.startsWith('<!doctype html><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src \'none\'; style-src \'unsafe-inline\';')).toBe(true)
  expect(file.endsWith(html)).toBe(true)
  return file
}

test('the download opens styled and loads nothing', async ({ page }) => {
  // opened outside the console: a document of its own, under its own meta policy only
  pageHTML = await download(page, hostile(false))
  seen = []
  const opened = await page.context().newPage()
  await opened.goto(`${kista}/file/report.html`)
  await expect(opened.locator('#title')).toHaveText('Report')
  await opened.waitForTimeout(1500)
  // styled by kista's stylesheet, inlined
  expect(await opened.locator('#th').evaluate((el) => getComputedStyle(el).backgroundColor)).toBe('rgb(242, 247, 247)')
  expect(await opened.locator('html').evaluate((el) => getComputedStyle(el).backgroundColor)).toBe('rgb(255, 255, 255)')
  // it loads nothing, runs nothing, and its form goes nowhere
  expect(seen).toEqual([])
  expect(await opened.locator('html').getAttribute('data-script-ran')).toBeNull()
  expect(await opened.locator('html').getAttribute('data-handler-ran')).toBeNull()
  await opened.locator('#submit').click({ noWaitAfter: true })
  await opened.waitForTimeout(1000)
  expect(seen).toEqual([])
  expect(opened.url()).toBe(`${kista}/file/report.html`)
})

// A limit of the download, held here so the docs stay true: the file is a document of its own, with
// no sandbox around it (a policy in a meta element cannot carry one), so a meta refresh in the
// issuer's html takes the reader's browser to its target when the file is opened. Nothing else of
// the page loads or runs. Spec 0045 lists meta refresh as closed by the frame's sandbox, which the
// download does not have.
test('the download does not stop a meta refresh in the page (a limit)', async ({ page }) => {
  pageHTML = await download(page, hostile(true))
  seen = []
  const opened = await page.context().newPage()
  await opened.goto(`${kista}/file/report.html`)
  await opened.waitForURL(`${evil}/refresh`)
  expect(seen).toEqual(['/refresh'])
})
