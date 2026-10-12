import { useRef, useState } from 'react'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, Table, when } from '../components/ui'
import { useLoad, useTenant } from '../lib/context'
import { ApiError, type Client, seg } from '../lib/http'
import type { Attachment, Release } from './types'

// The Reports block (spec 0011): the documents about an extension's bodies, as their summaries
// computed at intake, rendered as text. A document is untrusted: it is never rendered, only
// downloaded as bytes.

/** The media types each kind takes (the API's closed list). */
export const mediaTypes: Record<string, [string, string][]> = {
  sbom: [['application/vnd.cyclonedx+json', 'CycloneDX'], ['application/spdx+json', 'SPDX']],
  vulnerabilities: [
    ['application/vnd.cyclonedx+json', 'CycloneDX VEX'],
    ['application/openvex+json', 'OpenVEX'],
    ['application/sarif+json', 'SARIF'],
    ['application/json; profile=osv-scanner', 'osv-scanner'],
  ],
  review: [['application/vnd.kista.review.v1+json', 'kista review v1']],
  provenance: [['application/vnd.dev.sigstore.bundle.v0.3+json', 'Sigstore bundle'], ['application/vnd.dsse.envelope.v1+json', 'DSSE envelope']],
  notes: [['text/markdown', 'Markdown']],
}

const severities = ['critical', 'high', 'medium', 'low', 'unknown']

/** A scan older than this is flagged. */
const staleScan = 30 * 24 * 3600 * 1000

const num = (v: unknown) => (typeof v === 'number' ? v : 0)
const str = (v: unknown) => (typeof v === 'string' ? v : '')

/** A summary as one line of text, by kind. */
export function summaryText(kind: string, s: Record<string, unknown>): string {
  const counts = () => severities.filter((k) => num(s[k]) > 0).map((k) => `${num(s[k])} ${k}`)
  switch (kind) {
    case 'sbom':
      return [str(s.format), `${num(s.components)} components`, s.licences != null ? `${num(s.licences)} licences` : '',
        num(s.crypto_assets) > 0 ? `${num(s.crypto_assets)} crypto assets` : ''].filter(Boolean).join(', ')
    case 'vulnerabilities': {
      const parts = num(s.findings) === 0 ? ['no findings'] : [`${num(s.findings)} findings: ${counts().join(', ')}`]
      if (num(s.not_affected) > 0) parts.push(`${num(s.not_affected)} not affected`)
      return parts.join('; ')
    }
    case 'review':
      return [`${str(s.verdict)} by ${str(s.analyser)}`, num(s.findings) > 0 ? `${num(s.findings)} findings: ${counts().join(', ')}` : 'no findings']
        .join('; ')
    case 'provenance':
      return [str(s.builder) && `built by ${str(s.builder)}`, str(s.repository) && `from ${str(s.repository)}${str(s.ref) ? `@${str(s.ref)}` : ''}`,
        s.verified === false ? 'not verified' : ''].filter(Boolean).join(', ') || str(s.predicate_type)
    case 'notes':
      return str(s.first_line)
  }
  return Object.entries(s).map(([k, v]) => `${k}: ${String(v)}`).join(', ')
}

/** When a document is from: a scan's own time, else when it was issued. */
function dated(a: Attachment) {
  return str(a.summary.scanned_at) || str(a.summary.analysed_at) || a.issued_at
}

export function stale(a: Attachment, now = Date.now()) {
  return a.kind === 'vulnerabilities' && now - new Date(dated(a)).getTime() > staleScan
}

const origins: Record<string, string> = { tenant: 'the tenant', publisher: 'a publisher', feed: 'a feed' }

/** Who stands behind a document: the tenant, a publisher, or a feed's verified source. */
export function originText(a: Pick<Attachment, 'origin' | 'attached_by' | 'issuer' | 'source_origin'>) {
  if (a.origin === 'feed' && a.issuer) {
    const behind = a.source_origin && a.source_origin !== 'tenant' ? `, by ${a.source_origin}` : ''
    return `the feed of ${a.issuer}${behind} (signature verified)`
  }
  return `${origins[a.origin] ?? a.origin}${a.attached_by ? ` (${a.attached_by})` : ''}`
}

const fileExt = (mt: string) => (mt === 'text/markdown' ? 'md' : 'json')

/** Saves bytes as a file: never opened, never rendered. */
function save(b: Blob, name: string) {
  const url = URL.createObjectURL(new Blob([b], { type: 'application/octet-stream' }))
  const a = document.createElement('a')
  a.href = url
  a.download = name
  a.rel = 'noopener noreferrer'
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

type Pending = { a: Attachment; change: 'readers' | 'administrators' | 'feedable' | 'unfeedable' | 'remove' }

const titles: Record<Pending['change'], [string, string]> = {
  readers: ['Show to readers', 'show to readers'],
  administrators: ['Hide from readers', 'hide'],
  feedable: ['Offer in the feed', 'offer'],
  unfeedable: ['Withdraw from the feed', 'withdraw'],
  remove: ['Remove', 'remove'],
}

/** Every page of a list, following the API's cursors. */
async function every<T>(client: Client, path: string, key: string): Promise<T[]> {
  const out: T[] = []
  let cursor = ''
  do {
    const q = new URLSearchParams({ limit: '200' })
    if (cursor) q.set('cursor', cursor)
    const r = (await client.get<Record<string, unknown>>(`${path}${path.includes('?') ? '&' : '?'}${q}`)).data
    out.push(...((r[key] as T[]) ?? []))
    cursor = typeof r.next === 'string' ? r.next : ''
  } while (cursor)
  return out
}

/** Where readers would see a body's document: the channels serving a release of it, publicly (to
 * anyone) or privately (to callers granted install). */
export interface Showing {
  publicly: string[]
  privately: string[]
}

/** The Reports block of an extension, or of one of its bodies (a release page: attach there). */
export function Reports(props: { ext: string; body?: string }) {
  const { perms } = useTenant()
  // only for who may read them: no request is sent that would be refused
  if (!perms.extensionAdmin(props.ext) && !perms.canAudit) return null
  return <ReportsBlock {...props} />
}

function ReportsBlock({ ext, body }: { ext: string; body?: string }) {
  const { client, api, perms } = useTenant()
  const base = `${api}/extensions/${seg(ext)}/attachments`
  const { data, error, loading, reload } = useLoad(
    () => every<Attachment>(client, body ? `${base}?${new URLSearchParams({ body_hash: body })}` : base, 'attachments'),
    [base, body],
  )
  const [pending, setPending] = useState<Pending | null>(null)
  // undefined: being found; null: could not be found
  const [showing, setShowing] = useState<Showing | null | undefined>(undefined)
  const asked = useRef<Pending | null>(null)
  const [attaching, setAttaching] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const manage = perms.extensionAdmin(ext)
  const ask = async (p: Pending) => {
    asked.current = p
    setPending(p)
    setShowing(undefined)
    if (p.change !== 'readers') return
    const found = await channelsShowing(p.a.body_hash).catch(() => null)
    if (asked.current === p) setShowing(found) // the dialog may have moved to another document
  }
  // every release of the body in every channel, any state: the index shows its documents on each
  const channelsShowing = async (hash: string): Promise<Showing> => {
    const chs = await every<{ name: string }>(client, `${api}/channels`, 'channels')
    const out: Showing = { publicly: [], privately: [] }
    for (const c of chs) {
      const rs = await every<Release>(client, `${api}/channels/${seg(c.name)}/extensions/${seg(ext)}/releases`, 'releases')
      const of = rs.filter((r) => r.body_hash === hash)
      if (of.some((r) => r.visibility === 'public')) out.publicly.push(c.name)
      else if (of.length > 0) out.privately.push(c.name)
    }
    return out
  }
  const run = async () => {
    const p = pending!
    asked.current = null
    setPending(null)
    setFailure(null)
    try {
      if (p.change === 'remove') await client.del(`${base}/${seg(p.a.id)}`)
      else await client.post(`${base}/${seg(p.a.id)}/${p.change}`, { ifMatch: p.a.etag })
    } catch (e) {
      setFailure(e instanceof ApiError && e.status === 412 ? new Error('The document changed meanwhile: it was read again.') : e)
    }
    reload()
  }
  const download = async (a: Attachment) => {
    setFailure(null)
    try {
      save(await client.blob(`${base}/${seg(a.id)}/content`), `${a.id}.${fileExt(a.media_type)}`)
    } catch (e) {
      setFailure(e)
    }
  }
  const rows = data ?? []
  return (
    <Card title="Reports" actions={manage && body ? <Button tone="outline" onClick={() => setAttaching(true)}>Attach</Button> : undefined}>
      {failure != null && <ErrorBox error={failure} />}
      {attaching && body && (
        <AttachForm
          base={base}
          body={body}
          onDone={() => {
            setAttaching(false)
            reload()
          }}
        />
      )}
      {error ? <ErrorBox error={error} /> : loading && !data ? <Loading /> : rows.length === 0 ? <Empty>No documents.</Empty> : (
        <Table
          rows={rows}
          rowKey={(a) => a.id}
          columns={[
            ['Kind', (a) => <Mono>{a.kind}</Mono>],
            ['Title', (a) => <span title={a.media_type}>{a.title}</span>],
            ...(body ? [] : ([['Body', (a: Attachment) => <Mono>{a.body_hash.slice(0, 12)}</Mono>]] as [string, (a: Attachment) => React.ReactNode][])),
            ['Summary', (a) => <span className="whitespace-pre-wrap break-words">{summaryText(a.kind, a.summary ?? {})}</span>],
            ['Origin', (a) => <span className="break-words">{originText(a)}</span>],
            ['Dated', (a) => (
              <span>
                {when(dated(a))}
                {stale(a) && <> <Badge>stale</Badge></>}
                {a.expired && <> <Badge>expired</Badge></>}
              </span>
            )],
            ['Visibility', (a) => (
              <span className="flex flex-wrap gap-1">
                <Badge>{a.visibility ?? ''}</Badge>
                {a.head === false && <Badge>superseded</Badge>}
                {a.withdrawn && <Badge>withdrawn</Badge>}
                {a.feedable && <Badge>offered</Badge>}
              </span>
            )],
            ['', (a) => (
              <span className="flex flex-wrap gap-1.5">
                <Button onClick={() => download(a)}>Download</Button>
                {perms.tenantAdmin && (a.visibility === 'readers' || a.origin !== 'feed' || a.audience === 'readers') && (
                  <Button tone="outline" onClick={() => ask({ a, change: a.visibility === 'readers' ? 'administrators' : 'readers' })}>
                    {a.visibility === 'readers' ? 'Hide from readers' : 'Show to readers'}
                  </Button>
                )}
                {perms.tenantAdmin && a.origin !== 'feed' && (
                  <Button tone="outline" onClick={() => ask({ a, change: a.feedable ? 'unfeedable' : 'feedable' })}>
                    {a.feedable ? 'Withdraw from feed' : 'Offer in feed'}
                  </Button>
                )}
                {manage && <Button tone="danger-outline" onClick={() => ask({ a, change: 'remove' })}>Remove</Button>}
              </span>
            )],
          ]}
        />
      )}
      <Confirm
        open={pending !== null}
        title={pending ? `${titles[pending.change][0]}: ${pending.a.title}` : ''}
        confirm={pending ? titles[pending.change][1] : ''}
        danger={pending?.change === 'remove'}
        disabled={pending?.change === 'readers' && showing === undefined}
        onConfirm={run}
        onClose={() => {
          asked.current = null
          setPending(null)
        }}
      >
        {pending?.change === 'remove' && (
          <p>The document is deleted for good{pending.a.feedable ? '; the feed withdraws it from subscribers' : ''}.</p>
        )}
        {pending?.change === 'feedable' && (
          <p>Subscribers holding feed in a channel serving this body receive the document, signed with the tenant's attestation key
            {pending.a.visibility === 'readers' ? ', and may show it to their readers' : ''}. Withdrawing it later tells them so.</p>
        )}
        {pending?.change === 'unfeedable' && <p>The feed withdraws the document: subscribers mark it withdrawn.</p>}
        {pending?.change === 'administrators' && <p>Readers no longer see the document; administrators still do.</p>}
        {pending?.change === 'readers' && (
          <>
            <p>Whoever may see a release of this body sees the document, through the index, in every channel serving the body.</p>
            {showing === undefined ? <Loading /> : showing === null ? (
              <p>Where the body is served could not be found: check its channels before showing the document.</p>
            ) : (
              <>
                {showing.publicly.length > 0 && <p>Served publicly in <strong>{showing.publicly.join(', ')}</strong>: anyone there reads it.</p>}
                {showing.privately.length > 0 && <p>Served privately in <strong>{showing.privately.join(', ')}</strong>: callers granted install there read it.</p>}
                {showing.publicly.length + showing.privately.length === 0 && <p>No channel serves this body now: readers see it once one does.</p>}
              </>
            )}
          </>
        )}
      </Confirm>
    </Card>
  )
}

function AttachForm({ base, body, onDone }: { base: string; body: string; onDone: () => void }) {
  const { client } = useTenant()
  const [kind, setKind] = useState('sbom')
  const [mt, setMt] = useState(mediaTypes.sbom[0][0])
  const [title, setTitle] = useState('')
  const [file, setFile] = useState<File | null>(null)
  const [failure, setFailure] = useState<unknown>(null)
  const [busy, setBusy] = useState(false)
  const titleBytes = new TextEncoder().encode(title).length
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!file) return
    setBusy(true)
    setFailure(null)
    try {
      const q = new URLSearchParams({ body_hash: body, kind })
      await client.post(`${base}?${q}`, { raw: { body: file, type: mt }, headers: { 'Kista-Title': encodeURIComponent(title) } })
      onDone()
    } catch (err) {
      setFailure(err)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form onSubmit={submit} className="mb-4 flex flex-col gap-3">
      {failure != null && <ErrorBox error={failure} />}
      <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
        <Field label="Kind">
          <select className="field" value={kind} onChange={(e) => {
            setKind(e.target.value)
            setMt(mediaTypes[e.target.value][0][0])
          }}>
            {Object.keys(mediaTypes).map((k) => <option key={k} value={k}>{k}</option>)}
          </select>
        </Field>
        <Field label="Format">
          <select className="field" value={mt} onChange={(e) => setMt(e.target.value)}>
            {mediaTypes[kind].map(([v, label]) => <option key={v} value={v}>{label}</option>)}
          </select>
        </Field>
        <Field label="Title" hint={titleBytes > 200 ? 'At most 200 bytes of UTF-8.' : undefined}>
          <input className="field" required value={title} onChange={(e) => setTitle(e.target.value)} />
        </Field>
      </div>
      <Field label="Document" hint="Kept as is; the console shows only its summary, computed by kista.">
        <input type="file" required onChange={(e) => setFile(e.target.files?.[0] ?? null)} />
      </Field>
      <Note>An administrators-only document: show it to readers once attached, if it is meant for them.</Note>
      <div>
        <Button type="submit" tone="brand" disabled={busy || !file || !title || titleBytes > 200}>Attach</Button>
      </div>
    </form>
  )
}
