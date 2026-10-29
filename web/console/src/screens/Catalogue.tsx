import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, SearchBox, Table, when } from '../components/ui'
import { useAction } from '../lib/action'
import { useLoad, useTenant } from '../lib/context'
import { ApiError, seg, sourceSeg } from '../lib/http'
import { bytesOf } from '../lib/upload'

// The catalogue (spec 0033): every build that arrives in a tenant, mirrored or published, lands
// there as a build of its source (an upstream, a publisher, the tenant), names side by side; a
// channel takes from it by its rules, and its administrators place versions by hand. Held, revoked
// and withdrawn versions are the server's states: the console only shows them.

export interface CatalogueEntry {
  source: string
  name: string
  versions: string[] // newest first
  rules: Record<string, string> // the channels whose rule takes the source, with its mode
  held?: string[]
  revoked?: string[]
  withdrawn?: string[]
  /** a distributor's publisher's id: answered to a server administrator (spec 0033 phase B) */
  publisher_id?: string
  /** a distributor's publisher's entry's distributor (spec 0036): its metadata is the distributor's */
  distributor?: string
  /** the entry's metadata (spec 0036): a line of plain text, and an icon by its hash */
  summary?: string
  icon?: EntryIconRef
}

/** An entry's icon as the API answers it (spec 0036). */
export interface EntryIconRef {
  sha256: string
  width: number
  height: number
}

/** The one type an icon is shown as: a fixed allow-list, never the response's header (spec 0036). */
const iconTypes = ['image/png'] as const

/** An icon's bytes as an image the page shows, of a type from the allow-list whatever the bytes
 * came with: shown from a blob: URL, the console's API calls carrying a Bearer token <img src>
 * cannot send. */
export function iconBlob(b: Blob): Blob {
  return new Blob([b], { type: iconTypes[0] })
}

/** Bytes as standard base64. */
export function base64Of(bytes: Uint8Array): string {
  let bin = ''
  for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000))
  return btoa(bin)
}

/** What an Edit sends for the icon: a new file's bytes, the stored one kept, or none. */
export type IconChoice = { file: Uint8Array } | { keep: string } | null

/** The body of a metadata write (PUT …/meta): the whole metadata, so the icon not changed is sent
 * back by its hash and a summary-only edit uploads nothing. */
export function metaBody(summary: string, icon: IconChoice) {
  const s = summary.trim()
  let i: unknown = null
  if (icon && 'file' in icon) i = { content_type: iconTypes[0], data: base64Of(icon.file) }
  else if (icon) i = { sha256: icon.keep }
  return { summary: s === '' ? null : s, icon: i }
}

/** Whether the console offers to edit an entry's metadata: the tenant's administrators, on the
 * tenant's own sources only (a distributor's entry's metadata is the distributor's; the API decides
 * anyway). */
export function mayEditMeta(e: Pick<CatalogueEntry, 'distributor' | 'source'> | undefined, tenantAdmin: boolean) {
  return !!e && tenantAdmin && !e.distributor && !removedSource(e.source)
}

/** An entry's summary as the server counts it: code points. */
export const codePoints = (s: string) => [...s.trim().normalize('NFC')].length

export interface CatalogueBuild {
  id: string
  platform: string
  slot: string
  abi: string
  duckdb_version?: string
  c_api?: string
  body_hash: string
  visibility: string
  origin: string
  checked: boolean
  created_at: string
  state: 'held' | 'available' | 'withdrawn'
  available_at?: string
  revoked_at?: string
  withdrawn_at?: string
  withdrawn_reason?: string
  placed?: { channel: string; state: string }[]
}

interface CatalogueVersion {
  version: string
  builds: CatalogueBuild[]
}

type VersionState = 'withdrawn' | 'revoked' | 'held' | 'available'

/** A version's state in an entry, the one that matters most first: withdrawn (served nowhere),
 * revoked (placed automatically nowhere), held, available. */
export function versionState(e: Pick<CatalogueEntry, 'held' | 'revoked' | 'withdrawn'>, v: string): VersionState {
  if (e.withdrawn?.includes(v)) return 'withdrawn'
  if (e.revoked?.includes(v)) return 'revoked'
  if (e.held?.includes(v)) return 'held'
  return 'available'
}

/** A version's state from its builds, as the entry's lists would give it: withdrawn, revoked, held
 * (a build of it held), available. */
export function buildsState(builds: Pick<CatalogueBuild, 'state' | 'revoked_at'>[]): VersionState {
  if (builds.some((b) => b.state === 'withdrawn')) return 'withdrawn'
  if (builds.some((b) => b.revoked_at)) return 'revoked'
  if (builds.some((b) => b.state === 'held')) return 'held'
  return 'available'
}

/** Whether a source is a removed one, as answers name it ("upstream:(removed)"): its entries are not
 * read by name. */
export const removedSource = (s: string) => s.endsWith('(removed)')

/** Orders versions as the server does (store.CompareVersions): digit runs as numbers ("1.9" before
 * "1.10"), the rest bytewise, a shorter rest first. */
export function compareVersions(a: string, b: string): number {
  const digit = (c: string) => c >= '0' && c <= '9'
  const numbers = (x: string, y: string) => {
    x = x.replace(/^0+(?=.)/, '')
    y = y.replace(/^0+(?=.)/, '')
    if (x.length !== y.length) return x.length < y.length ? -1 : 1
    return x < y ? -1 : x > y ? 1 : 0
  }
  let i = 0
  let j = 0
  while (i < a.length && j < b.length) {
    if (digit(a[i]) && digit(b[j])) {
      let ei = i
      let ej = j
      while (ei < a.length && digit(a[ei])) ei++
      while (ej < b.length && digit(b[ej])) ej++
      const c = numbers(a.slice(i, ei), b.slice(j, ej))
      if (c !== 0) return c
      i = ei
      j = ej
      continue
    }
    if (a[i] !== b[j]) return a[i] < b[j] ? -1 : 1
    i++
    j++
  }
  if (a.length - i !== b.length - j) return a.length - i < b.length - j ? -1 : 1
  return a < b ? -1 : a > b ? 1 : 0
}

/** A reason's length as the server counts it: bytes of UTF-8. */
export const utf8Bytes = (s: string) => new TextEncoder().encode(s).length

/** Whether a build is offered for a purge: released nowhere and not withdrawn. The API also refuses
 * one released once and purged since. */
export function purgeable(b: Pick<CatalogueBuild, 'state' | 'placed'>) {
  return b.state !== 'withdrawn' && (b.placed ?? []).length === 0
}

/** A rule's mode as words: a rule without one is an arrival's, the channel serving what lands in it. */
export function modeWords(mode?: string) {
  return mode || 'as it lands'
}

export interface Placing {
  channel: string
  version: string
  narrow: boolean
  notCurrent: boolean
  early: boolean
}

/** A version as a path segment: its "+" (spec 0008's grammar allows it) as is, since the API refuses
 * an escaped path. */
export const versionSeg = (v: string) => seg(v).replace(/%2B/gi, '+')

/** The body of a placement by hand (POST …/releases/place). */
export function placeBody(source: string, p: Placing) {
  const out: Record<string, unknown> = { source, version: p.version }
  if (p.narrow) out.visibility = 'private'
  if (p.notCurrent) out.current = false
  if (p.early) out.early = true
  return out
}

const chipTone: Record<VersionState, string> = {
  available: 'bg-surface-soft text-ink',
  held: 'bg-warning-soft text-warning',
  revoked: 'bg-danger-soft text-danger',
  withdrawn: 'bg-danger-soft text-danger',
}

const shownVersions = 8

/** An entry's versions, newest first, each with its state. */
function Versions({ e }: { e: CatalogueEntry }) {
  const more = e.versions.length - shownVersions
  return (
    <span className="flex flex-wrap gap-1">
      {e.versions.slice(0, shownVersions).map((v) => {
        const s = versionState(e, v)
        return (
          <span key={v} className={`chip ${chipTone[s]}`}>
            <Mono>{v}</Mono>{s !== 'available' && <>&nbsp;{s}</>}
          </span>
        )
      })}
      {more > 0 && <span className="chip bg-surface-soft text-ink-muted">+{more} older</span>}
    </span>
  )
}

/** The channels whose rule takes an entry's source for its name, with the rule's mode. */
function Rules({ rules }: { rules: Record<string, string> }) {
  const chs = Object.keys(rules).sort()
  if (chs.length === 0) return <span className="text-ink-muted">no channel</span>
  return (
    <span className="flex flex-wrap gap-1">
      {chs.map((c) => <span key={c} className="chip border border-line text-ink"><Mono>{c}</Mono>&nbsp;· {modeWords(rules[c])}</span>)}
    </span>
  )
}

/** An entry's path in the API: …/catalogue/{source}/extensions/{name}. */
export const entryPath = (api: string, source: string, name: string) => `${api}/catalogue/${sourceSeg(source)}/extensions/${seg(name)}`

/** An entry's icon at a size: fetched with the console's client (the token), shown from a blob: URL
 * of the allow-listed type; a box of the size while there is none. No cache, no batching. */
export function EntryIcon({ path, icon, size }: { path: string; icon?: EntryIconRef; size: number }) {
  const { client } = useTenant()
  const [url, setUrl] = useState('')
  const sha = icon?.sha256 ?? ''
  useEffect(() => {
    setUrl('')
    if (!sha) return
    let gone = false
    let made = ''
    client.blob(`${path}/icon`).then((b) => {
      if (gone) return
      made = URL.createObjectURL(iconBlob(b))
      setUrl(made)
    }).catch(() => {}) // an icon that cannot be read is not shown
    return () => {
      gone = true
      if (made) URL.revokeObjectURL(made)
    }
  }, [client, path, sha])
  const box = { width: size, height: size }
  if (!url) return <span aria-hidden className="inline-block shrink-0 rounded bg-surface-soft" style={box} />
  return <img src={url} alt="" width={size} height={size} className="shrink-0 rounded" style={box} />
}

/** An entry's summary: plain text, never markup or a link, its direction isolated. */
export function EntrySummary({ summary }: { summary?: string }) {
  if (!summary) return null
  // isolated: a right-to-left summary does not reorder the text around it
  return <bdi className="text-[13px] text-ink-muted">{summary}</bdi>
}

/** Editing an entry's metadata (spec 0036): the summary, a PNG file for the icon, or clearing it.
 * The icon not changed is sent back by its hash, so a summary-only edit uploads nothing. */
export function MetaDialog(props: {
  open: boolean
  name: string
  summary?: string
  icon?: EntryIconRef
  onSave: (body: ReturnType<typeof metaBody>) => void
  onClose: () => void
}) {
  const [summary, setSummary] = useState('')
  const [file, setFile] = useState<File | null>(null)
  const [clear, setClear] = useState(false)
  const [failure, setFailure] = useState('')
  const [opened, setOpened] = useState(false)
  if (props.open !== opened) {
    // each opening starts from the entry's metadata
    setOpened(props.open)
    if (props.open) {
      setSummary(props.summary ?? '')
      setFile(null)
      setClear(false)
      setFailure('')
    }
  }
  const long = codePoints(summary) > 160
  const save = async () => {
    let icon: IconChoice = null
    if (file) {
      if (file.size > 256 << 10) {
        setFailure('The icon is at most 256 KiB.')
        return
      }
      icon = { file: new Uint8Array(await bytesOf(file)) }
    } else if (!clear && props.icon) icon = { keep: props.icon.sha256 }
    props.onSave(metaBody(summary, icon))
  }
  return (
    <Confirm open={props.open} title={`Edit ${props.name}`} confirm="Save" disabled={long} onConfirm={() => void save()} onClose={props.onClose}>
      <Field label="Summary" hint={long ? 'At most 160 characters.' : 'One line of plain text; empty for none.'}>
        <input className="field" value={summary} onChange={(e) => setSummary(e.target.value)} data-autofocus />
      </Field>
      <Field label="Icon" hint="A square PNG, 64 to 512 pixels a side, at most 256 KiB, without text or colour-profile chunks.">
        <input className="field" type="file" accept="image/png" onChange={(e) => { setFile(e.target.files?.[0] ?? null); setFailure('') }} />
      </Field>
      {props.icon && (
        <label className="flex items-center gap-2 text-[13px]">
          <input type="checkbox" checked={clear && !file} disabled={!!file} onChange={(e) => setClear(e.target.checked)} /> remove the icon
        </label>
      )}
      {failure && <p className="text-[13px] text-danger">{failure}</p>}
    </Confirm>
  )
}

/** The tenant's catalogue: every (source, name), filtered by a name prefix and a source. The API
 * answers it whole, so the filters are applied here. */
export function Catalogue() {
  const { client, api, base } = useTenant()
  const [prefix, setPrefix] = useState('')
  const [source, setSource] = useState('')
  const list = useLoad(() => client.get<{ entries: CatalogueEntry[] }>(`${api}/catalogue`), [api])
  // keyed by position too: two removed sources of one name answer the same label
  const all = (list.data?.data.entries ?? []).map((e, i) => ({ ...e, key: `${e.source} ${e.name} ${i}` }))
  const sources = [...new Set(all.map((e) => e.source))].sort()
  const rows = all.filter((e) => e.name.startsWith(prefix) && (!source || e.source === source))
  return (
    <>
      <PageTitle title="Catalogue">
        <span className="pb-1 text-ink-muted">Every build that arrived in the tenant, by its source. Channels take from it by their rules.</span>
      </PageTitle>
      <div role="search" className="flex flex-wrap items-center gap-2">
        <SearchBox label="Find an extension in the catalogue" value={prefix} onChange={setPrefix} />
        <select aria-label="source" className="field py-1 text-[13px]" value={source} onChange={(e) => setSource(e.target.value)}>
          <option value="">Every source</option>
          {sources.map((s) => <option key={s}>{s}</option>)}
        </select>
        <span className="ml-auto"><Button onClick={list.reload}>Refresh</Button></span>
      </div>
      {list.error ? <ErrorBox error={list.error} /> : !list.data ? <Loading /> : rows.length === 0 ? (
        <Empty>{prefix || source ? 'Nothing matches.' : 'The catalogue is empty.'}</Empty>
      ) : (
        <Table
          rows={rows}
          rowKey={(e) => e.key}
          columns={[
            ['Extension', (e) => (
              <span className="flex items-start gap-2">
                {removedSource(e.source) ? <EntryIcon path="" size={24} /> : <EntryIcon path={entryPath(api, e.source, e.name)} icon={e.icon} size={24} />}
                <span className="flex flex-col">
                  {removedSource(e.source)
                    ? <span className="font-mono" title="a removed source's entries are not read by name; its releases stay where they are">{e.name}</span>
                    : <Link className="font-mono font-medium no-underline" to={`${base}/catalogue/${seg(e.source)}/${seg(e.name)}`}>{e.name}</Link>}
                  <EntrySummary summary={e.summary} />
                </span>
              </span>
            )],
            ['Source', (e) => <Mono>{e.source}</Mono>],
            ['Versions', (e) => <Versions e={e} />],
            ['Taken by', (e) => <Rules rules={e.rules} />],
          ]}
        />
      )}
      {rows.some((e) => removedSource(e.source)) && (
        <Note>A removed source's entries are listed without a page: they are not read by name, and their releases stay where they are.</Note>
      )}
    </>
  )
}

interface ChannelRow {
  name: string
  kind: string
  package_type?: string
}

/** An entry: its versions with their builds, where each is placed; placing a version, withdrawing
 * a distributor's version (a server administrator), purging a build placed nowhere. */
export function CatalogueEntryPage() {
  const { source = '', name = '' } = useParams()
  const { client, api, base, perms } = useTenant()
  const entry = useLoad(() => client.get<{ entries: CatalogueEntry[] }>(`${api}/catalogue?${new URLSearchParams({ source, name })}`), [api, source, name])
  const path = entryPath(api, source, name)
  const versions = useLoad(() => client.get<{ versions: CatalogueVersion[] }>(path), [path])
  // the signed channels of the catalogue's type (the default) where the caller may place the name
  const channels = useLoad(() => client.get<{ channels: ChannelRow[] }>(`${api}/channels`), [api])
  const targets = (channels.data?.data.channels ?? []).filter((c) => c.kind === 'signed' && !c.package_type && perms.mayPromote(c.name, name)).map((c) => c.name)
  const reload = () => {
    entry.reload()
    versions.reload()
  }
  const { busy, failure, act } = useAction('The entry', reload)
  const [placing, setPlacing] = useState<{ version: string; held: boolean } | null>(null)
  const [withdrawing, setWithdrawing] = useState('')
  const [reason, setReason] = useState('')
  const [purging, setPurging] = useState<{ b: CatalogueBuild; version: string } | null>(null)
  const [editing, setEditing] = useState(false)
  const e = entry.data?.data.entries.find((x) => x.source === source && x.name === name)
  const done = (ok: boolean) => ok && reload()
  return (
    <>
      <PageTitle eyebrow={<><Link to={`${base}/catalogue`} className="no-underline">Catalogue</Link> / <span className="font-mono">{source}</span></>}
        title={<span className="flex items-center gap-3"><EntryIcon path={path} icon={e?.icon} size={64} /><span className="font-mono">{name}</span></span>}>
        {e && <span className="flex items-center gap-2 pb-1 text-ink-muted">Taken by <Rules rules={e.rules} /></span>}
        {mayEditMeta(e, perms.tenantAdmin) && <span className="pb-1"><Button disabled={busy} onClick={() => setEditing(true)}>Edit…</Button></span>}
      </PageTitle>
      {e?.summary && <p className="m-0"><EntrySummary summary={e.summary} /></p>}
      {failure != null && <ErrorBox error={failure} />}
      {entry.error != null && <ErrorBox error={entry.error} />}
      {channels.error != null && <ErrorBox error={channels.error} />}
      {versions.error instanceof ApiError && versions.error.status === 404 ? <Empty>No build of it from this source in the catalogue.</Empty>
        : versions.error ? <ErrorBox error={versions.error} /> : !versions.data ? <Loading /> : versions.data.data.versions.length === 0 ? <Empty>No builds.</Empty> : (
        versions.data.data.versions.map((v) => {
          const st = buildsState(v.builds)
          const held = st === 'held' || st === 'revoked'
          return (
            <Card key={v.version} title={<span className="flex items-center gap-2"><span className="font-mono">{v.version}</span> <Badge>{st}</Badge></span>}
              actions={(
                <span role="group" aria-label={`${v.version} actions`} className="flex gap-1.5">
                  {st !== 'withdrawn' && targets.length > 0 && <Button disabled={busy} onClick={() => setPlacing({ version: v.version, held })}>Place…</Button>}
                  {st !== 'withdrawn' && perms.server && e?.publisher_id && (
                    <Button tone="danger-outline" disabled={busy} onClick={() => { setReason(''); setWithdrawing(v.version) }}>Withdraw…</Button>
                  )}
                </span>
              )}>
              <Table
                rows={v.builds}
                rowKey={(b) => b.id}
                columns={[
                  ['Platform', (b) => <Mono>{b.platform}</Mono>],
                  ['Build', (b) => <Mono>{b.abi} {b.duckdb_version ?? (b.c_api ? `C API ${b.c_api}` : '')}</Mono>],
                  ['State', (b) => <BuildState b={b} />],
                  ['Visibility', (b) => <Badge>{b.visibility}</Badge>],
                  ['Placed in', (b) => ((b.placed ?? []).length === 0 ? <span className="text-ink-muted">nowhere</span> : (
                    <span className="flex flex-wrap gap-1">
                      {b.placed!.map((p) => <span key={p.channel} className="chip border border-line text-ink"><Mono>{p.channel}</Mono>&nbsp;{p.state}</span>)}
                    </span>
                  ))],
                  ['Arrived', (b) => <span className="text-[13px] text-ink-muted">{when(b.created_at)}</span>],
                  ['', (b) => (perms.tenantAdmin && purgeable(b) ? <Button tone="danger-outline" disabled={busy} onClick={() => setPurging({ b, version: v.version })}>Purge…</Button> : '')],
                ]}
              />
            </Card>
          )
        })
      )}
      <PlaceDialog open={placing !== null} name={name} source={source} version={placing?.version} held={placing?.held ?? false} channels={targets}
        onClose={() => setPlacing(null)}
        onPlace={(p) => {
          setPlacing(null)
          act(() => client.post(`${api}/channels/${seg(p.channel)}/extensions/${seg(name)}/releases/place`, { body: placeBody(source, p) })).then(done)
        }} />
      <Confirm open={withdrawing !== ''} title={`Withdraw ${name} ${withdrawing}?`} confirm="Withdraw" danger
        disabled={busy || reason.trim() === '' || utf8Bytes(reason.trim()) > 400}
        onConfirm={() => {
          const v = withdrawing
          setWithdrawing('')
          act(() => client.post(`${path}/versions/${versionSeg(v)}/withdraw`, { body: { reason: reason.trim() } })).then(done)
        }}
        onClose={() => setWithdrawing('')}>
        <p>From now on the version is served in none of the tenant's channels, and no build of it arrives from <Mono>{source}</Mono> again.
          The tenant's administrators are told, with the reason. It cannot be undone.</p>
        <Field label="Reason" hint={utf8Bytes(reason.trim()) > 400 ? 'At most 400 bytes of UTF-8.' : undefined}>
          <input className="field" value={reason} onChange={(ev) => setReason(ev.target.value)} data-autofocus />
        </Field>
      </Confirm>
      <MetaDialog open={editing} name={name} summary={e?.summary} icon={e?.icon} onClose={() => setEditing(false)}
        onSave={(body) => {
          setEditing(false)
          act(() => client.put(`${path}/meta`, { body })).then(done)
        }} />
      <Confirm open={purging !== null} title="Purge this build?" confirm="Purge" danger
        onConfirm={() => {
          const { b } = purging!
          setPurging(null)
          act(() => client.del(`${api}/catalogue/builds/${seg(b.id)}`)).then(done)
        }}
        onClose={() => setPurging(null)}>
        <p><Mono>{name} {purging?.version}</Mono> for <Mono>{purging?.b.platform}</Mono>, body <Mono>{purging?.b.body_hash}</Mono></p>
        <p>It was never released, so no one could have installed it: it is deleted, and its slot is free again. A mirror that brought it fetches
          it again at its next run.</p>
      </Confirm>
    </>
  )
}

/** A build's state: held until when, withdrawn and why, revoked. */
function BuildState({ b }: { b: CatalogueBuild }) {
  return (
    <span className="flex flex-col gap-0.5">
      <span className="flex flex-wrap gap-1">
        <Badge>{b.state}</Badge>
        {b.revoked_at && <Badge>revoked</Badge>}
      </span>
      {b.state === 'held' && b.available_at && <span className="text-[12px] text-ink-muted">until {when(b.available_at)}</span>}
      {b.state === 'withdrawn' && <span className="text-[12px]">{b.withdrawn_reason}{b.withdrawn_at && <span className="text-ink-muted"> · {when(b.withdrawn_at)}</span>}</span>}
      {b.revoked_at && <span className="text-[12px] text-ink-muted">its key revoked {when(b.revoked_at)}: placed automatically nowhere</span>}
    </span>
  )
}

/** Placing a version of a source by hand (spec 0033): into a channel the caller may place it in,
 * every build of the version the channel serves, all or none. version and channel are asked for
 * unless given; early is offered for a held version (or always, when held is not known). */
export function PlaceDialog(props: {
  open: boolean
  name: string
  source: string
  version?: string
  /** a version to start the field with, when version is not given */
  suggest?: string
  channel?: string
  channels?: string[]
  held: boolean | 'unknown'
  onPlace: (p: Placing) => void
  onClose: () => void
}) {
  const { perms } = useTenant()
  const [p, setP] = useState<Placing>({ channel: '', version: '', narrow: false, notCurrent: false, early: false })
  const [opened, setOpened] = useState(false)
  if (props.open !== opened) {
    // each opening starts from what it is given
    setOpened(props.open)
    if (props.open) setP({ channel: props.channel ?? props.channels?.[0] ?? '', version: props.version ?? props.suggest ?? '', narrow: false, notCurrent: false, early: false })
  }
  const admin = p.channel !== '' && perms.channelAdmin(p.channel)
  const ready = p.channel !== '' && p.version.trim() !== ''
  const submit = () => ready && props.onPlace({ ...p, version: p.version.trim(), early: p.early && admin })
  return (
    <Confirm open={props.open} title={`Place ${props.name}${props.version ? ` ${props.version}` : ''}`} confirm="Place" disabled={!ready} onConfirm={submit} onClose={props.onClose}>
      <p>Every build of the version from <Mono>{props.source}</Mono> that the channel serves is released there, signed with its key, all or none.
        Builds of it that arrive later for its other DuckDB versions or platforms follow.</p>
      {!props.channel && (
        <Field label="Channel">
          <select className="field" value={p.channel} onChange={(e) => setP({ ...p, channel: e.target.value })}>
            {(props.channels ?? []).map((c) => <option key={c}>{c}</option>)}
          </select>
        </Field>
      )}
      {!props.version && (
        <Field label="Version"><input className="field font-mono" value={p.version} onChange={(e) => setP({ ...p, version: e.target.value })} data-autofocus
          onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); submit() } }} /></Field>
      )}
      <label className="flex items-center gap-2 text-[13px]">
        <input type="checkbox" checked={p.narrow} onChange={(e) => setP({ ...p, narrow: e.target.checked })} /> private: only callers granted install
      </label>
      <label className="flex items-center gap-2 text-[13px]">
        <input type="checkbox" checked={p.notCurrent} onChange={(e) => setP({ ...p, notCurrent: e.target.checked })} /> not current: leave the current version as it is
      </label>
      {props.held !== false && (
        <label className="flex items-center gap-2 text-[13px]">
          <input type="checkbox" checked={p.early && admin} disabled={!admin} onChange={(e) => setP({ ...p, early: e.target.checked })} /> early: place a held
          version now
        </label>
      )}
      {props.held !== false && <Note>A held version is placed only early, by a channel administrator: the choice is recorded with the placement.</Note>}
    </Confirm>
  )
}
