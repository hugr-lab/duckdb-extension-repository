import { useLayoutEffect, useMemo, useRef, useState } from 'react'
import { Button, ErrorBox, Loading, Mono, when } from '../components/ui'
import { useLoad, useTenant } from '../lib/context'
import { seg } from '../lib/http'
import { type Feed } from './Feeds'
import { type ReportTheme, downloadable, reportFile, reportHTML, reportPageLabel, reportSrcdoc, rowLines } from '../lib/report'
import reportCss from '../report.css?raw'
import reportCssURL from '../report.css?url'
import { every, save } from './Reports'
import type { Attachment } from './types'

// A report's page (spec 0045). The console takes html from the document it downloads and shows it
// in a frame with an empty sandbox (no script, no form, no navigation of the top, a unique origin),
// under kista's one report stylesheet: the frame's document is a fixed prefix followed by the html,
// which is never parsed here. The border names the issuer from the record, not from the document.

/** The absolute URL of kista's report stylesheet: the only thing the frame may load. */
export const reportStylesheet = () => new URL(reportCssURL, document.baseURI).href

/** The element that names the console's theme for a node: the document's root (standalone), or the
 * host element of the shadow root the console is mounted in (the micro-frontend). */
function themeHost(node: Node): HTMLElement {
  const root = node.getRootNode()
  return root instanceof ShadowRoot ? (root.host as HTMLElement) : document.documentElement
}

/** The console's theme where a node is, followed as it changes: the frame cannot see it, so its
 * prefix carries it. null until the node is in the document: the theme is read where the node is
 * (the shadow host's when embedded), before the first paint, so the frame is never built for the
 * wrong one. */
function useTheme(ref: React.RefObject<HTMLElement | null>): ReportTheme | null {
  const [theme, setTheme] = useState<ReportTheme | null>(null)
  useLayoutEffect(() => {
    if (!ref.current) return
    const host = themeHost(ref.current)
    const read = () => setTheme(host.dataset.kistaTheme === 'dark' ? 'dark' : 'light')
    read()
    const seen = new MutationObserver(read)
    seen.observe(host, { attributes: true, attributeFilter: ['data-kista-theme'] })
    return () => seen.disconnect()
  }, [ref])
  return theme
}

/** The frame: an empty sandbox, the prefixed document, a border labelled with who the page is from.
 * A link in the page that navigates the frame is refused by the console's policy and leaves the
 * browser's blocked page in it: "Show the page again" builds the frame anew. */
export function ReportFrame({ a, html }: { a: Pick<Attachment, 'origin' | 'issuer' | 'attached_by' | 'source_origin'>; html: string }) {
  const label = reportPageLabel(a)
  const box = useRef<HTMLElement>(null)
  const theme = useTheme(box)
  const [shown, setShown] = useState(0)
  const doc = useMemo(() => (theme ? reportSrcdoc(html, reportStylesheet(), theme) : null), [html, theme])
  return (
    <figure ref={box} className="m-0 overflow-hidden rounded-md border-2 border-line" role="group" aria-label={label}>
      <figcaption className="flex flex-wrap items-center gap-2 border-b border-line bg-surface-soft px-3 py-1.5 text-[12px] font-semibold text-ink-muted">
        <span>{label}: the issuer's own words, shown as it wrote them</span>
        <span className="ml-auto"><Button tone="plain" onClick={() => setShown(shown + 1)}>Show the page again</Button></span>
      </figcaption>
      {doc === ''
        ? <p className="m-0 p-3 text-[13px] text-danger">The page cannot be shown here: download it instead.</p>
        : doc !== null && <iframe key={shown} title={label} sandbox="" srcDoc={doc} referrerPolicy="no-referrer" className="block h-[70vh] w-full border-0" />}
    </figure>
  )
}

/** A report's page, read from its document: the frame, and the download (the print path). */
export function ReportPage({ a, base, onClose }: { a: Attachment; base: string; onClose: () => void }) {
  const { client } = useTenant()
  const page = useLoad(async () => {
    const html = reportHTML(await (await client.blob(`${base}/${seg(a.id)}/content`)).text())
    if (html === undefined) throw new Error('The document holds no page.')
    return html
  }, [base, a.id])
  return (
    <div className="mt-4 flex flex-col gap-2" role="region" aria-label={`${a.title} page`}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-[13px] font-semibold text-ink">{a.title}</span>
        <span className="ml-auto flex gap-1.5">
          {page.data !== undefined && downloadable(page.data) && (
            <Button tone="outline" onClick={() => {
              const file = reportFile(page.data!, reportCss)
              if (file !== undefined) save(new Blob([file]), `${a.id}.html`)
            }}>Download the page</Button>
          )}
          <Button onClick={onClose}>Close</Button>
        </span>
      </div>
      {page.error != null ? <ErrorBox error={page.error} /> : page.data === undefined ? <Loading /> : <ReportFrame a={a} html={page.data} />}
      {page.data !== undefined && (downloadable(page.data)
        ? <p className="m-0 text-[12px] text-ink-muted">To print it, download the page and open the file: a framed page cannot print itself.</p>
        : <p className="m-0 text-[12px] text-ink-muted">This page cannot be downloaded: it carries an <Mono>http-equiv</Mono> instruction, which a
          downloaded file would follow. It is shown here only.</p>)}
    </div>
  )
}

/** When the feeds that take reports last succeeded, for "No report held"; '' when not known. */
export function feedsLastSuccess(feeds: Pick<Feed, 'kinds' | 'last_success_at'>[] | undefined) {
  if (!feeds) return ''
  const taking = feeds.filter((f) => f.kinds.length === 0 || f.kinds.includes('report'))
  if (taking.length === 0) return 'no feed takes reports'
  const last = taking.map((f) => f.last_success_at ?? '').sort().pop()
  return last ? `its feeds last succeeded ${when(last)}` : 'its feeds have not succeeded yet'
}

/** The lines on the row of a version that waits for a decision (spec 0045), from one read: the
 * chain heads held about that source's version. No line at all for a caller who cannot read
 * documents; "No report held" only after a successful empty read. */
export function ReportLines({ name, source, version, feeds, builds }: { name: string; source: string; version: string; feeds?: Feed[]; builds?: number }) {
  const { client, api, perms } = useTenant()
  const may = perms.extensionAdmin(name) || perms.canAudit
  const path = `${api}/extensions/${seg(name)}/attachments?${new URLSearchParams({ source, version, head: 'true' })}`
  const held = useLoad(() => (may ? every<Attachment>(client, path, 'attachments') : Promise.resolve(undefined)), [path, may])
  const [open, setOpen] = useState(false)
  if (!may || held.error != null || held.data === undefined) return null
  const lines = rowLines(held.data, when, builds)
  const last = feedsLastSuccess(feeds)
  return (
    <div className="mt-1 flex flex-col gap-0.5 text-[12px] text-ink-muted" role="group" aria-label={`${name} ${version} reports`}>
      {lines.reports.length === 0
        ? <span>No report held{last ? ` (${last})` : ''}.</span>
        : lines.reports.slice(0, open ? undefined : 3).map((l) => <span key={l.key}><b className="font-semibold text-ink">{l.behind}</b>: {l.text}</span>)}
      {lines.reports.length > 3 && !open && <Button tone="plain" onClick={() => setOpen(true)}>{`${lines.reports.length - 3} more`}</Button>}
      {lines.vulnerabilities.map((l) => <span key={l.key}><b className="font-semibold text-ink">{l.behind}</b>: {l.text}</span>)}
      {lines.reports.length > 0 && <span>The issuer's page of <Mono>{version}</Mono> is under "Report &amp; licence".</span>}
    </div>
  )
}
