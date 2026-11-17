import { readFileSync } from 'node:fs'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Perms, TenantContext } from './context'
import { Client } from './http'
import {
  attentionText, issuerHost, policyURL, reportBehind, reportFile, reportHTML, reportPageLabel, reportParts, reportPolicy, reportSrcdoc, reportText,
  rowLines, staleScanOf, vulnerabilitiesText,
} from './report'
import { feedBody, handOn, handOnWarning, feedKinds } from '../screens/Feeds'
import { ReportFrame, ReportLines, ReportPage, feedsLastSuccess } from '../screens/ReportPage'
import { summaryText } from '../screens/Reports'
import type { Attachment } from '../screens/types'

// kista's report stylesheet, as the build inlines it into a download (vitest empties a css import)
const reportCss = readFileSync('src/report.css', 'utf8')

// Spec 0045 in the console: the frame's document and the download, the row's texts, the
// vulnerabilities rules. A report's html and every member of its summary are hostile input.

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const sheet = 'https://kista.example/ui/assets/report-abc123.css'
const hostile = '<h1>Report</h1><script>alert(1)</script><style>*{display:none}</style><img src="https://evil.example/x.png">' +
  '<meta http-equiv="refresh" content="0;url=https://evil.example/"><form action="https://evil.example/"><button>go</button></form></html><!--'

describe('the report frame', () => {
  it('is a fixed prefix followed by the html, unparsed', () => {
    const doc = reportSrcdoc(hostile, sheet)
    const prefix = '<!doctype html><html data-kista-theme="light"><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src \'none\'; ' +
      `style-src ${sheet}; img-src data:; form-action 'none'; base-uri 'none'"><link rel="stylesheet" href="${sheet}">`
    expect(doc).toBe(prefix + hostile)
    expect(reportSrcdoc('', sheet)).toBe(prefix)
    // the console's theme is the only thing that varies in the prefix: one of two fixed words
    expect(reportSrcdoc(hostile, sheet, 'dark')).toBe(prefix.replace('data-kista-theme="light"', 'data-kista-theme="dark"') + hostile)
    expect(reportSrcdoc('', sheet, '"><script>' as never)).toBe(prefix)
    expect(reportPolicy(sheet)).toBe(`default-src 'none'; style-src ${sheet}; img-src data:; form-action 'none'; base-uri 'none'`)
  })
  it('is not built for a stylesheet URL that cannot be named in a policy', () => {
    for (const bad of ['', 'report.css', '/ui/report.css', 'javascript:alert(1)', "https://k.example/a.css; script-src 'unsafe-inline'", 'https://k.example/a b.css',
      'https://k.example/a.css"><script>', "https://k.example/'x'.css", 'https://k.example/a.css,https://evil.example/']) {
      expect(policyURL(bad), bad).toBe(false)
      expect(reportSrcdoc('<p>x</p>', bad), bad).toBe('')
    }
    expect(policyURL(sheet)).toBe(true)
  })
  it('has an empty sandbox and a border that names the issuer from the record', () => {
    render(<ReportFrame a={{ origin: 'feed', issuer: 'https://enterest.example/api/v1/tenants/acme' }} html={hostile} />)
    const frame = screen.getByTitle("enterest.example's page") as HTMLIFrameElement
    expect(frame.tagName).toBe('IFRAME')
    expect(frame.getAttribute('sandbox')).toBe('') // present and empty: no allow-scripts, no allow-same-origin, nothing
    expect(frame.hasAttribute('src')).toBe(false)
    expect(frame.hasAttribute('allow')).toBe(false)
    expect(frame.getAttribute('referrerpolicy')).toBe('no-referrer')
    const doc = frame.getAttribute('srcdoc') ?? ''
    expect(doc.endsWith(hostile)).toBe(true)
    expect(doc.startsWith('<!doctype html><html data-kista-theme="light"><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src \'none\'; style-src http')).toBe(true)
    expect(screen.getByRole('group', { name: "enterest.example's page" })).toBeTruthy()
    // the page itself is in no element of the console's document
    expect(document.querySelector('script')).toBeNull()
    expect(document.body.textContent).not.toContain('alert(1)')
  })
  it("follows the console's theme, which its prefix carries", async () => {
    document.documentElement.dataset.kistaTheme = 'dark'
    render(<ReportFrame a={{ origin: 'tenant' }} html="<p>x</p>" />)
    const frame = screen.getByTitle("The tenant's own report's page")
    expect(frame.getAttribute('srcdoc')).toContain('<html data-kista-theme="dark">')
    document.documentElement.dataset.kistaTheme = 'light'
    await waitFor(() => expect(frame.getAttribute('srcdoc')).toContain('<html data-kista-theme="light">'))
    expect(frame.getAttribute('sandbox')).toBe('')
    delete document.documentElement.dataset.kistaTheme
  })
  it('names the tenant on its own report', () => {
    expect(reportPageLabel({ origin: 'tenant' })).toBe("The tenant's own report's page")
    expect(issuerHost('not a url')).toBe('not a url')
  })
})

describe('the download', () => {
  it('is the same prefix with the stylesheet inlined', () => {
    const file = reportFile(hostile, 'body{color:red}')
    expect(file).toBe('<!doctype html><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src \'none\'; ' +
      "style-src 'unsafe-inline'; img-src data:; form-action 'none'; base-uri 'none'\"><style>body{color:red}</style>" + hostile)
    expect(reportFile('<p>x</p>', 'a{}</STYLE><script>x</script>')).not.toContain('<script>x')
  })
  it("uses kista's stylesheet, which loads nothing and cannot end its own element", () => {
    expect(reportCss).toContain(":root[data-kista-theme='dark']")
    expect(reportCss).toContain('prefers-color-scheme: dark') // a downloaded file follows its reader's system
    expect(reportCss).not.toMatch(/url\(|@import|<\/style/i)
    expect(reportFile('', reportCss)).toContain('prefers-color-scheme: dark')
    expect(reportFile('', reportCss)).not.toContain('<html data-kista-theme')
  })
  it('takes html only as a string member of the document', () => {
    expect(reportHTML('{"html":"<p>x</p>","envelope":"kista.report.v1"}')).toBe('<p>x</p>')
    expect(reportHTML('{"html":""}')).toBe('')
    for (const bad of ['{"html":["<p>"]}', '{"html":null}', '{"HTML":"x"}', '[{"html":"x"}]', '"x"', 'not json', '{}', 'null']) {
      expect(reportHTML(bad), bad).toBeUndefined()
    }
  })
})

const summary = {
  envelope: 'kista.report.v1', report: 'rep-1', issue: 2, issued_at: '2026-11-16T09:00:00Z', depth: 'static_v1', bodies: 2, read: true, kinds_evaluated: 12,
  attention: [{ kind: 'network', count: 3 }, { kind: 'native_code', count: 0 }, { kind: 'process', count: 1 }],
  sections: [{ name: 'imports', status: 'analysed' }, { name: 'strings', status: 'skipped' }],
}

describe("a report's row texts", () => {
  it('begins with who stands behind the report, from the record', () => {
    expect(reportBehind({ origin: 'feed', issuer: 'https://enterest.example/api/v1/tenants/acme' })).toBe('Report from enterest.example, signature verified')
    expect(reportBehind({ origin: 'tenant', attached_by: 'acme/kc|tom' })).toBe("The tenant's own report, attached by acme/kc|tom")
    expect(reportBehind({ origin: 'tenant' })).toBe("The tenant's own report")
  })
  it('says issue, date, depth, attention, open sections and whether the build was read, open strings quoted', () => {
    expect(reportText(summary)).toBe('issue 2 · 2026-11-16T09:00:00Z · depth "static_v1" · attention: "network" 3, "process" 1 · ' +
      'sections not analysed: "strings" ("skipped") · this build was read')
    expect(reportParts({ ...summary, read: false })).toContain('this build was not read')
    expect(reportParts(summary, 1, 3)).toContain('1 of 3 builds read')
    expect(summaryText('report', summary)).toBe(reportText(summary))
  })
  it('never reads no attention as "no points of attention" when nothing was evaluated', () => {
    expect(attentionText({ attention: [], kinds_evaluated: 0 })).toBe('points of attention: not evaluated')
    expect(attentionText({ attention: [] })).toBe('points of attention: not evaluated')
    expect(attentionText({ attention: [{ kind: 'network', count: 0 }] })).toBe('points of attention: not evaluated')
    expect(attentionText({ attention: [], kinds_evaluated: 7 })).toBe('no points of attention (7 kinds evaluated)')
    expect(attentionText({ attention: [{ kind: 'network', count: 2 }], kinds_evaluated: 0 })).toBe('attention: "network" 2')
    expect(attentionText({ kinds_evaluated: 7 })).toBe('') // dropped at intake: the summary is incomplete
  })
  it('says a summary is incomplete, and shows nothing of a list that was left out', () => {
    const text = reportText({ ...summary, attention: undefined, incomplete: true })
    expect(text).toContain('summary incomplete: open the report')
    expect(text).not.toContain('attention')
  })
  it('shows a summary of other shapes as nothing more than text', () => {
    expect(reportText({ issue: '<b>', depth: 7, attention: 'x', sections: [null, 'x', { name: 1 }] } as Record<string, unknown>)).toBe(
      'issue 0 · sections not analysed: "" ("") · this build was not read')
  })
})

describe('a vulnerabilities summary', () => {
  it('never says "no findings" while a component is unchecked or unmarked', () => {
    expect(vulnerabilitiesText({ findings: 0, components: 40, checked: 40, not_checked: 0 })).toBe('no findings')
    expect(vulnerabilitiesText({ findings: 0, components: 40, checked: 31, not_checked: 4 })).toBe('31 of 40 components checked; 4 marked not checked')
    expect(vulnerabilitiesText({ findings: 0, components: 40, checked: 0, not_checked: 0 })).toBe('0 of 40 components checked')
    expect(vulnerabilitiesText({ findings: 0, components: 0, checked: 0, not_checked: 0 })).toBe('0 of 0 components checked')
    expect(vulnerabilitiesText({ findings: 2, high: 1, low: 1, components: 40, checked: 39, not_checked: 0 })).toBe('2 findings: 1 high, 1 low; 39 of 40 components checked')
    expect(vulnerabilitiesText({ findings: 2, high: 1, low: 1, components: 40, checked: 40, not_checked: 0 })).toBe('2 findings: 1 high, 1 low')
    // a format without components (OpenVEX, SARIF, osv-scanner), or a summary computed before the counts
    expect(vulnerabilitiesText({ findings: 0 })).toBe('no findings')
    expect(vulnerabilitiesText({ findings: 3, critical: 1, low: 2, not_affected: 1 })).toBe('3 findings: 1 critical, 2 low; 1 not affected')
  })
  it('counts informational advisories apart, and says "at least" at the list bound', () => {
    expect(vulnerabilitiesText({ findings: 1, low: 1, informational: 5 })).toBe('1 findings: 1 low; 5 informational')
    expect(vulnerabilitiesText({ findings: 0, informational: 2, components: 3, checked: 3 })).toBe('no findings; 2 informational')
    expect(vulnerabilitiesText({ findings: 99990, unknown: 99990, not_affected: 10 })).toBe('at least 99990 findings: 99990 unknown; 10 not affected')
    expect(vulnerabilitiesText({ findings: 0, components: 100000, checked: 99999 })).toBe('99999 of at least 100000 components checked')
    expect(summaryText('vulnerabilities', { findings: 0, components: 2, checked: 1 })).toBe('1 of 2 components checked')
  })
  it('is stale after 30 days only without an unexpired valid_until', () => {
    const now = Date.parse('2026-06-01T00:00:00Z')
    const a = { kind: 'vulnerabilities', summary: { scanned_at: '2026-01-01T00:00:00Z' }, issued_at: '2026-01-01T00:00:00Z' }
    expect(staleScanOf(a, now)).toBe(true)
    expect(staleScanOf({ ...a, valid_until: '2026-07-01T00:00:00Z' }, now)).toBe(false)
    expect(staleScanOf({ ...a, valid_until: '2026-05-01T00:00:00Z' }, now)).toBe(true)
    expect(staleScanOf({ ...a, valid_until: '2026-07-01T00:00:00Z', expired: true }, now)).toBe(true)
    expect(staleScanOf({ ...a, kind: 'report' }, now)).toBe(false)
  })
})

const held = (over: Partial<Attachment>): Attachment => ({
  id: 'd1', body_hash: 'a'.repeat(64), kind: 'report', media_type: 'application/vnd.kista.report+json', title: 'Report', size: 1, summary,
  origin: 'feed', issuer: 'https://enterest.example/api/v1/tenants/acme', issued_at: '2026-11-16T09:00:00Z', head: true, ...over,
})

describe('the row of a waiting version', () => {
  it("keeps a feed's and the tenant's reports on separate lines, each report once, the highest issue first", () => {
    const lines = rowLines([
      held({ id: 'd1' }),
      held({ id: 'd2', body_hash: 'b'.repeat(64), summary: { ...summary, read: false } }), // the same report, about the version's other build
      held({ id: 'd3', summary: { ...summary, issue: 1 } }), // a head its producer did not chain
      held({ id: 'd4', origin: 'tenant', issuer: undefined, attached_by: 'acme/kc|tom', summary: { ...summary, report: 'own', issue: 7 } }),
      held({ id: 'd5', withdrawn: true, summary: { ...summary, issue: 9 } }),
      held({ id: 'd6', head: false, summary: { ...summary, issue: 8 } }),
      held({ id: 'v1', kind: 'vulnerabilities', summary: { findings: 0, components: 10, checked: 4 }, issued_at: '2026-11-15T00:00:00Z' }),
    ])
    expect(lines.reports.map((l) => l.behind)).toEqual(['Report from enterest.example, signature verified', 'Report from enterest.example, signature verified',
      "The tenant's own report, attached by acme/kc|tom"])
    expect(lines.reports[0].text).toContain('issue 2 ·')
    expect(lines.reports[0].text).toContain('1 of 2 builds read')
    expect(lines.reports[1].text).toContain('issue 1 ·')
    expect(lines.reports[2].text).toContain('issue 7 ·')
    expect(lines.vulnerabilities).toEqual([{ key: 'v1', behind: 'Vulnerabilities from enterest.example, signature verified', text: '4 of 10 components checked' }])
    expect(rowLines([]).reports).toEqual([])
  })
  it('says when the feeds that take reports last succeeded', () => {
    expect(feedsLastSuccess(undefined)).toBe('')
    expect(feedsLastSuccess([{ kinds: ['sbom'], last_success_at: '2026-11-16T09:00:00Z' }])).toBe('no feed takes reports')
    expect(feedsLastSuccess([{ kinds: [] }])).toBe('its feeds have not succeeded yet')
    expect(feedsLastSuccess([{ kinds: ['report'], last_success_at: '2026-11-15T09:00:00Z' }, { kinds: [], last_success_at: '2026-11-16T09:00:00Z' }]))
      .toMatch(/^its feeds last succeeded /)
  })
})

type Call = { path: string; search: string }

function api(answer: (c: Call) => Response) {
  const calls: Call[] = []
  vi.stubGlobal('fetch', vi.fn(async (input: string) => {
    const u = new URL(input)
    const c = { path: u.pathname, search: u.search }
    calls.push(c)
    return answer(c)
  }))
  return calls
}

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

function tenant(ui: React.ReactNode, perms = new Perms(true)) {
  const client = new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })
  return render(
    <MemoryRouter>
      <TenantContext.Provider value={{ client, tenant: 'acme', perms, api: '/tenants/acme', base: '/t/acme' }}>{ui}</TenantContext.Provider>
    </MemoryRouter>)
}

describe('the row, read', () => {
  it('reads the heads of the source\'s version once and shows the lines', async () => {
    const calls = api(() => json({ attachments: [held({})] }))
    tenant(<ReportLines name="geo" source="upstream:core" version="1.2.0" />)
    await waitFor(() => expect(screen.getByText('Report from enterest.example, signature verified')).toBeTruthy())
    expect(calls).toHaveLength(1)
    expect(calls[0].path).toBe('/api/v1/tenants/acme/extensions/geo/attachments')
    const q = new URLSearchParams(calls[0].search)
    expect([q.get('source'), q.get('version'), q.get('head')]).toEqual(['upstream:core', '1.2.0', 'true'])
  })
  it('says "No report held" only after a successful empty read, with the feeds\' last success', async () => {
    api(() => json({ attachments: [] }))
    tenant(<ReportLines name="geo" source="upstream:core" version="1.2.0" feeds={[{ kinds: [], last_success_at: '2026-11-16T09:00:00Z' } as never]} />)
    await waitFor(() => expect(screen.getByText(/^No report held \(its feeds last succeeded /)).toBeTruthy())
    cleanup()
    api(() => json({ title: 'down' }, 500))
    tenant(<ReportLines name="geo" source="upstream:core" version="1.2.0" />)
    await new Promise((r) => setTimeout(r, 20))
    expect(screen.queryByText(/No report held/)).toBeNull()
  })
  it('shows no line, and sends no request, for a caller who cannot read documents', async () => {
    const calls = api(() => json({ attachments: [held({})] }))
    const { container } = tenant(<ReportLines name="geo" source="upstream:core" version="1.2.0" />, new Perms(false, []))
    await new Promise((r) => setTimeout(r, 20))
    expect(calls).toHaveLength(0)
    expect(container.textContent).toBe('')
  })
})

describe("a report's page, read", () => {
  it('frames the html of the document it downloads', async () => {
    api(() => new Response(JSON.stringify({ envelope: 'kista.report.v1', html: hostile }), { status: 200, headers: { 'Content-Type': 'application/vnd.kista.report+json' } }))
    tenant(<ReportPage a={held({})} base="/tenants/acme/extensions/geo/attachments" onClose={() => {}} />)
    const frame = await waitFor(() => screen.getByTitle("enterest.example's page")) as HTMLIFrameElement
    expect(frame.getAttribute('sandbox')).toBe('')
    expect((frame.getAttribute('srcdoc') ?? '').endsWith(hostile)).toBe(true)
    expect(screen.getByRole('button', { name: 'Download the page' })).toBeTruthy()
  })
  it('frames nothing of a document without a string html', async () => {
    api(() => new Response(JSON.stringify({ html: ['<p>'] }), { status: 200 }))
    tenant(<ReportPage a={held({})} base="/tenants/acme/extensions/geo/attachments" onClose={() => {}} />)
    await waitFor(() => expect(screen.getByText(/holds no page/)).toBeTruthy())
    expect(document.querySelector('iframe')).toBeNull()
  })
})

describe('a feed handed on, and taken through a relay', () => {
  const form = { name: 'reports', url: 'https://enterest.example/api/v1/tenants/acme', credential: 'relay', keys: 'sha256:a', kinds: [...feedKinds], readers: false,
    offer: 'upstreams' as const, channels: [], mode: 'online' as const, export: false, via: ' https://kista.example/api/v1/tenants/acme ' }
  it('sends via at creation, never for a bundle-mode feed, and no kinds filter when every kind is taken', () => {
    expect(feedBody(form)).toEqual({ name: 'reports', url: form.url, credential: 'relay', keys: ['sha256:a'], readers: false, offer: 'upstreams',
      via: 'https://kista.example/api/v1/tenants/acme' })
    expect(feedBody({ ...form, via: '' })).not.toHaveProperty('via')
    expect(feedBody({ ...form, mode: 'bundle' })).not.toHaveProperty('via')
    expect(feedKinds).toContain('report')
  })
  it('names the source and who receives its documents before handing it on', () => {
    expect(handOn).toBe("Hand on: to bundles and to kistas that take this tenant's feed")
    const warning = handOnWarning({ name: 'reports', url: form.url })
    expect(warning).toContain(form.url)
    expect(warning).toContain('the feed reports')
    expect(warning).toContain('Every holder of feed on a channel of this tenant will receive')
  })
})
