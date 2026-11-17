import type { Attachment } from '../screens/types'

// A report (spec 0045) in the console: the issuer's page in an isolated frame, and the lines on the
// row of a version that waits for a decision. A report's html, and every member of its summary, is
// the issuer's word: the page is never parsed here, only prefixed and handed to a frame with an
// empty sandbox; the summary's strings are shown as quoted text.

/** The frame's own policy, in a meta element: nothing loads but the one stylesheet named. */
export function reportPolicy(styleSrc: string) {
  return `default-src 'none'; style-src ${styleSrc}; img-src data:; form-action 'none'; base-uri 'none'`
}

/** A stylesheet URL that can be named in a policy and an attribute: absolute http(s), with none of
 * the characters that end a source expression, a directive or an attribute value. */
export function policyURL(url: string) {
  return /^https?:\/\/[^\s;,'"<>]+$/.test(url)
}

const attr = (s: string) => s.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;')

/** The console's theme, as its root names it (data-kista-theme). */
export type ReportTheme = 'light' | 'dark'

/** The frame's document: a fixed prefix (the root element with the console's theme, the policy, the
 * link to kista's report stylesheet) followed by the issuer's html, which is not parsed. Empty when
 * the stylesheet's URL cannot be named in a policy: the page is then not shown at all. */
export function reportSrcdoc(html: string, stylesheet: string, theme: ReportTheme = 'light') {
  if (!policyURL(stylesheet)) return ''
  return `<!doctype html><html data-kista-theme="${theme === 'dark' ? 'dark' : 'light'}"><meta charset="utf-8">` +
    `<meta http-equiv="Content-Security-Policy" content="${attr(reportPolicy(stylesheet))}"><link rel="stylesheet" href="${attr(stylesheet)}">` + html
}

/** The page as a file (the print path: a sandboxed frame cannot print itself): the same prefix with
 * kista's stylesheet inlined, then the issuer's html. Opened outside kista, it loads nothing. */
export function reportFile(html: string, css: string) {
  // the stylesheet is kista's own; it must not end its own element
  const style = /<\/style/i.test(css) ? '' : css
  return `<!doctype html><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="${attr(reportPolicy("'unsafe-inline'"))}">` +
    `<style>${style}</style>` + html
}

/** The html member of a report document's bytes; undefined when the document is not an object with
 * a string html. */
export function reportHTML(text: string): string | undefined {
  try {
    const doc: unknown = JSON.parse(text)
    if (doc && typeof doc === 'object' && !Array.isArray(doc)) {
      const html = (doc as Record<string, unknown>).html
      if (typeof html === 'string') return html
    }
  } catch {
    // not JSON: no page
  }
  return undefined
}

const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0)
const str = (v: unknown) => (typeof v === 'string' ? v : '')
const list = (v: unknown) => (Array.isArray(v) ? v.filter((x): x is Record<string, unknown> => !!x && typeof x === 'object') : [])
/** An open string of a document, quoted: it is the issuer's word, not kista's. */
const quoted = (s: string) => `"${s}"`

/** The host of a feed URL; the URL itself when it does not parse. */
export function issuerHost(issuer: string) {
  try {
    return new URL(issuer).host
  } catch {
    return issuer
  }
}

type Behind = Pick<Attachment, 'origin' | 'issuer' | 'attached_by' | 'source_origin'>

/** Who stands behind a feed's document at its source, when that is not the source's tenant itself
 * (a publisher there): the source's signed word, said as such. */
const sourceBehind = (a: Behind) => (a.source_origin && a.source_origin !== 'tenant' ? `, by ${a.source_origin} there` : '')

/** Who stands behind a report, from its record, never from the document: a feed's verified
 * source, a publisher of the tenant, or the tenant. */
export function reportBehind(a: Behind) {
  if (a.origin === 'feed' && a.issuer) return `Report from ${issuerHost(a.issuer)}${sourceBehind(a)}, signature verified`
  if (a.origin === 'publisher') return `A publisher's own report${a.attached_by ? `, attached by ${a.attached_by}` : ''}`
  return `The tenant's own report${a.attached_by ? `, attached by ${a.attached_by}` : ''}`
}

/** The label of the border around a report's page. */
export function reportPageLabel(a: Behind) {
  if (a.origin === 'feed' && a.issuer) return `${issuerHost(a.issuer)}'s page${sourceBehind(a)}`
  if (a.origin === 'publisher') return `The page of a publisher's own report${a.attached_by ? ` (${a.attached_by})` : ''}`
  return "The tenant's own report's page"
}

/** A report's points of attention, in words: none with kinds_evaluated 0 or absent is "not
 * evaluated", never "no points of attention". */
export function attentionText(s: Record<string, unknown>) {
  if (!Array.isArray(s.attention)) return ''
  const points = list(s.attention).filter((p) => num(p.count) > 0)
  if (points.length > 0) return `attention: ${points.map((p) => `${quoted(str(p.kind))} ${num(p.count)}`).join(', ')}`
  return num(s.kinds_evaluated) > 0 ? `no points of attention (${num(s.kinds_evaluated)} kinds evaluated)` : 'points of attention: not evaluated'
}

/** How far a report covers a row's builds: held is the builds a copy of the report is held for (one
 * document per body), read those it says it read, builds the version's builds. */
export interface Coverage {
  held: number
  read: number
  builds: number
}

/** A report's summary as the parts of a line: issue, date, depth, attention, the sections not
 * analysed, and its coverage of the row's builds. */
export function reportParts(s: Record<string, unknown>, cover: Coverage = { held: 1, read: s.read === true ? 1 : 0, builds: 1 },
  date = (iso: string) => iso): string[] {
  const parts = [`issue ${num(s.issue)}`]
  if (str(s.issued_at)) parts.push(date(str(s.issued_at)))
  if (str(s.depth)) parts.push(`depth ${quoted(str(s.depth))}`)
  const attention = attentionText(s)
  if (attention) parts.push(attention)
  const open = list(s.sections).filter((x) => str(x.status) !== 'analysed')
  if (open.length > 0) parts.push(`sections not analysed: ${open.map((x) => `${quoted(str(x.name))} (${quoted(str(x.status))})`).join(', ')}`)
  parts.push(cover.builds <= 1 && cover.held >= 1 ? (cover.read > 0 ? 'this build was read' : 'this build was not read')
    : `report held for ${cover.held} of ${cover.builds} builds; ${cover.read} read`)
  if (s.incomplete === true) parts.push('summary incomplete: open the report')
  return parts
}

/** A report's summary as one line of text. */
export const reportText = (s: Record<string, unknown>) => reportParts(s).join(' · ')

/** The bound of a document's counted lists (kista counts at most this many components, findings). */
export const listBound = 100000

/** A vulnerabilities summary as one line (spec 0045): never "no findings" while a component is
 * unchecked or unmarked, but "N of M components checked"; a count that reached the list bound says
 * "at least"; informational advisories are counted apart. */
export function vulnerabilitiesText(s: Record<string, unknown>) {
  const severities = ['critical', 'high', 'medium', 'low', 'unknown']
  const findings = num(s.findings)
  const least = (n: number) => (n >= listBound ? `at least ${n}` : String(n))
  // every entry of the document's list counts toward its bound
  const capped = findings + num(s.informational) + num(s.not_affected) >= listBound
  const counted = typeof s.components === 'number'
  const components = num(s.components)
  const checked = num(s.checked)
  // at the bound kista stopped counting: the document has components it never looked at, so it is
  // not known to be fully checked
  const unchecked = counted && (checked < components || components >= listBound)
  const coverage = counted && (unchecked || components === 0) ? `${checked} of ${least(components)} components checked` : ''
  const parts: string[] = []
  if (findings > 0) {
    parts.push(`${capped ? 'at least ' : ''}${findings} findings: ${severities.filter((k) => num(s[k]) > 0).map((k) => `${num(s[k])} ${k}`).join(', ')}`)
    if (coverage) parts.push(coverage)
  } else {
    parts.push(coverage || 'no findings')
  }
  if (num(s.not_checked) > 0) parts.push(`${num(s.not_checked)} marked not checked`)
  if (num(s.informational) > 0) parts.push(`${num(s.informational)} informational`)
  if (num(s.not_affected) > 0) parts.push(`${num(s.not_affected)} not affected`)
  return parts.join('; ')
}

type Held = Pick<Attachment, 'id' | 'kind' | 'body_hash' | 'origin' | 'issuer' | 'attached_by' | 'source_origin' | 'summary' | 'issued_at' | 'valid_until' | 'expired' |
  'withdrawn' | 'head'>

/** A document's validity, from its statement: expired, or until when; '' when it carries none. */
function validity(a: Pick<Attachment, 'valid_until' | 'expired'>, date: (iso: string) => string) {
  if (a.expired) return 'expired'
  return a.valid_until ? `valid until ${date(a.valid_until)}` : ''
}

const sourceOf = (a: Held) => (a.origin === 'feed' ? `feed ${a.issuer ?? ''} ${a.source_origin ?? ''}` : `${a.origin} ${a.attached_by ?? ''}`)

/** The documents of the Reports block with each body's report heads in the row's order: where a
 * producer did not chain its issues, the highest issue first. Nothing else moves. */
export function orderReports<T extends Pick<Attachment, 'kind' | 'body_hash' | 'head' | 'summary'>>(rows: T[]): T[] {
  const out = [...rows]
  for (let i = 0; i < out.length;) {
    let j = i + 1
    while (j < out.length && out[j].kind === out[i].kind && out[j].body_hash === out[i].body_hash) j++
    if (out[i].kind === 'report') {
      const run = out.slice(i, j).map((a, at) => ({ a, at }))
      run.sort((x, y) => Number(x.a.head === false) - Number(y.a.head === false) ||
        (x.a.head === false ? 0 : num(y.a.summary?.issue) - num(x.a.summary?.issue)) || x.at - y.at)
      out.splice(i, j - i, ...run.map((x) => x.a))
    }
    i = j
  }
  return out
}

/** One line of a waiting version's row. */
export interface RowLine {
  key: string
  behind: string
  text: string
}

/** The lines on the row of a version that waits for a decision, from the chain heads held about its
 * builds: per source (a feed's, a publisher's and the tenant's are separate lines, never merged)
 * each report once (a report covers the version's builds, one document per body), the highest issue
 * first; then the vulnerabilities. A withdrawn head is hidden. Empty: no report is held. builds is
 * the number of the version's builds, when the catalogue told it: a report held for fewer says so. */
export function rowLines(heads: Held[], date = (iso: string) => iso, builds?: number): { reports: RowLine[]; vulnerabilities: RowLine[] } {
  const live = heads.filter((a) => !a.withdrawn && a.head !== false)
  const bodies = new Set(live.map((a) => a.body_hash)).size
  const source = sourceOf
  const reports = new Map<string, { a: Held; read: number; builds: Set<string>; validity: Set<string> }>()
  for (const a of live.filter((x) => x.kind === 'report')) {
    const key = [source(a), str(a.summary?.report), num(a.summary?.issue)].join('|')
    const g = reports.get(key) ?? { a, read: 0, builds: new Set<string>(), validity: new Set<string>() }
    if (validity(a, date)) g.validity.add(validity(a, date))
    if (!g.builds.has(a.body_hash)) {
      g.builds.add(a.body_hash)
      if (a.summary?.read === true) g.read++
    }
    // a copy whose summary is incomplete flags the line
    if (a.summary?.incomplete === true) g.a = a
    reports.set(key, g)
  }
  const ordered = [...reports.entries()].sort(([ka, x], [kb, y]) => source(x.a).localeCompare(source(y.a)) || num(y.a.summary?.issue) - num(x.a.summary?.issue) || ka.localeCompare(kb))
  const vulns = live.filter((x) => x.kind === 'vulnerabilities').sort((x, y) => source(x).localeCompare(source(y)) || y.issued_at.localeCompare(x.issued_at))
  return {
    reports: ordered.map(([key, g]) => ({
      key,
      behind: reportBehind(g.a),
      text: [...reportParts(g.a.summary ?? {}, { held: g.builds.size, read: g.read, builds: Math.max(builds ?? 0, bodies, g.builds.size) }, date),
        ...[...g.validity].sort()].join(' · '),
    })),
    vulnerabilities: vulns.map((a) => ({
      key: a.id,
      behind: a.origin === 'feed' && a.issuer ? `Vulnerabilities from ${issuerHost(a.issuer)}${sourceBehind(a)}, signature verified`
        : a.origin === 'publisher' ? "A publisher's own vulnerabilities document" : "The tenant's own vulnerabilities document",
      text: [vulnerabilitiesText(a.summary ?? {}), validity(a, date)].filter(Boolean).join('; '),
    })),
  }
}

/** A scan is stale after this long, unless its statement carries a valid_until that has not passed. */
const staleScan = 30 * 24 * 3600 * 1000

/** Freshness is the statement's valid_until (spec 0045): the 30-day flag shows only when the
 * document has no unexpired one. */
export function staleScanOf(a: Pick<Attachment, 'kind' | 'summary' | 'issued_at' | 'valid_until' | 'expired'>, now = Date.now()) {
  if (a.kind !== 'vulnerabilities') return false
  if (a.valid_until && !a.expired && new Date(a.valid_until).getTime() > now) return false
  const dated = str(a.summary?.scanned_at) || a.issued_at
  return now - new Date(dated).getTime() > staleScan
}
