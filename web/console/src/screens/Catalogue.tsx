import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, SearchBox, Table, when } from '../components/ui'
import { useAction } from '../lib/action'
import { useLoad, useTenant } from '../lib/context'
import { useHome } from '../lib/home'
import { ApiError, seg, sourceSeg } from '../lib/http'
import { type Cell, type HomeReads, cellOf, groups } from '../lib/queue'
import { bytesOf } from '../lib/upload'
import { AddBuildButton } from './AddBuild'
import { PromoteButton } from './Promote'
import { Reports } from './Reports'
import { holdWords, type SourceRow } from './Sources'

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
  /** spec 0041: each held version's end of hold */
  held_until?: Record<string, string>
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
        const until = e.held_until?.[v]
        return (
          <span key={v} className={`chip ${chipTone[s]}`} title={s === 'held' && until ? `held until ${when(until)}` : undefined}>
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

/** The tenant's home (spec 0041): every (source, name) of the catalogue against the tenant's signed
 * channels of the default type, each cell what the channel's rule does with the entry; filtered by
 * a name prefix and a source. The API answers it whole, so the filters are applied here. */
export function Catalogue() {
  const { base } = useTenant()
  const home = useHome()
  const [prefix, setPrefix] = useState('')
  const [source, setSource] = useState('')
  const [ruling, setRuling] = useState<{ e: CatalogueEntry; channel: string } | null>(null)
  if (!home) return <Empty>The catalogue is its tenant administrators' view.</Empty>
  const reads = home.reads
  // keyed by position too: two removed sources of one name answer the same label
  const all = (reads?.entries ?? []).map((e, i) => ({ ...e, key: `${e.source} ${e.name} ${i}` }))
  const sources = [...new Set(all.map((e) => e.source))].sort()
  const rows = all.filter((e) => e.name.startsWith(prefix) && (!source || e.source === source))
  const waiting = home.items.length
  const counts = groups.map(([g, title]) => [title, home.items.filter((i) => i.group === g).length] as const).filter(([, n]) => n > 0)
  return (
    <>
      <PageTitle title="Catalogue">
        <span className="pb-1 text-ink-muted">Every build that arrived in the tenant, by its source, and what each channel takes of it.</span>
        <span className="ml-auto pb-1"><AddBuildButton onAdded={home.reload} /></span>
      </PageTitle>
      {waiting > 0 && (
        <Note><Link to={`${base}/decisions`}>{waiting === 1 ? '1 decision waits' : `${waiting} decisions wait`}</Link>:{' '}
          {counts.map(([title, n]) => `${title.toLowerCase()} ${n}`).join(', ')}.</Note>
      )}
      <div role="search" className="flex flex-wrap items-center gap-2">
        <SearchBox label="Find an extension in the catalogue" value={prefix} onChange={setPrefix} />
        <select aria-label="source" className="field py-1 text-[13px]" value={source} onChange={(e) => setSource(e.target.value)}>
          <option value="">Every source</option>
          {sources.map((s) => <option key={s}>{s}</option>)}
        </select>
        <span className="ml-auto"><Button onClick={home.reload}>Refresh</Button></span>
      </div>
      {home.error ? <ErrorBox error={home.error} /> : !reads ? <Loading /> : rows.length === 0 ? (
        <Empty>{prefix || source ? 'Nothing matches.' : 'The catalogue is empty.'}</Empty>
      ) : (
        <Matrix rows={rows} reads={reads} onRule={(e, channel) => setRuling({ e, channel })} />
      )}
      {rows.some((e) => removedSource(e.source)) && (
        <Note>A removed source's entries are listed without a page: they are not read by name, and their releases stay where they are.</Note>
      )}
      <RuleDialog open={ruling !== null} channel={ruling?.channel ?? ''} name={ruling?.e.name ?? ''} source={ruling?.e.source ?? ''}
        entry={ruling?.e} rule={ruling ? reads?.channels.find((c) => c.name === ruling.channel)?.sources.find((s) => s.name === ruling.e.name && s.source) : undefined}
        hold={reads?.channels.find((c) => c.name === ruling?.channel)?.hold} onClose={() => setRuling(null)} onSaved={home.reload} />
    </>
  )
}

/** The matrix: a row per entry, a column per signed channel of the default type, its header the
 * channel's follows and hold. */
function Matrix({ rows, reads, onRule }: { rows: (CatalogueEntry & { key: string })[]; reads: HomeReads; onRule: (e: CatalogueEntry, channel: string) => void }) {
  const { api, base, perms } = useTenant()
  return (
    <div className="overflow-x-auto rounded-md border border-line">
      <table className="w-full border-collapse">
        <thead className="bg-surface-soft">
          <tr>
            <th className="th pl-4">Extension</th>
            <th className="th">Versions</th>
            {reads.channels.map((c) => (
              <th key={c.name} className="th">
                <span className="flex flex-col gap-0.5">
                  <Link className="font-mono no-underline" to={`${base}/channels/${seg(c.name)}`}>{c.name}</Link>
                  <span className="text-[12px] font-normal normal-case tracking-normal text-ink-muted">
                    {c.follows.length > 0 ? `follows ${c.follows.map((f) => f.source).join(', ')}` : 'follows nothing'} · hold {holdWords(c.hold)}
                  </span>
                </span>
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((e) => (
            <tr key={e.key} className="border-t border-line hover:bg-row">
              <td className="td pl-4">
                <span className="flex items-start gap-2">
                  {removedSource(e.source) ? <EntryIcon path="" size={24} /> : <EntryIcon path={entryPath(api, e.source, e.name)} icon={e.icon} size={24} />}
                  <span className="flex flex-col">
                    {removedSource(e.source)
                      ? <span className="font-mono" title="a removed source's entries are not read by name; its releases stay where they are">{e.name}</span>
                      : <Link className="font-mono font-medium no-underline" to={`${base}/catalogue/${seg(e.name)}`}>{e.name}</Link>}
                    <span className="text-[12px]"><Mono>{e.source}</Mono></span>
                    <EntrySummary summary={e.summary} />
                  </span>
                </span>
              </td>
              <td className="td"><Versions e={e} /></td>
              {reads.channels.map((c) => (
                <td key={c.name} className="td">
                  <CellView cell={cellOf(e, c, reads.decisions)} name={e.name} channel={c.name}
                    onRule={!removedSource(e.source) && perms.channelAdmin(c.name) ? () => onRule(e, c.name) : undefined} />
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/** A channel's cell for an entry: its rule's mode and current version, what waits and when its next
 * placement is, a failed placement; another source's rule; a follow's skip; or a rule to add. */
function CellView({ cell, name, channel, onRule }: { cell: Cell; name: string; channel: string; onRule?: () => void }) {
  const button = (label: string, aria: string) => onRule && (
    <button type="button" className="text-[13px] text-brand-strong hover:underline" aria-label={aria} onClick={onRule}>{label}</button>
  )
  const add = button('+ Rule…', `Add a rule for ${name} in ${channel}`)
  const set = button('Set rule…', `Set the rule for ${name} in ${channel}`)
  switch (cell.kind) {
    case 'rule':
      return (
        <span className="flex flex-col items-start gap-0.5">
          <span className="flex items-center gap-1"><Badge>{modeWords(cell.mode)}</Badge>{cell.current && <Mono>{cell.current}</Mono>}</span>
          {cell.waiting && <span className="chip bg-warning-soft text-warning">→ waits&nbsp;<Mono>{cell.waiting}</Mono></span>}
          {cell.waitsUntil && <span className="text-[12px] text-ink-muted">next placement {when(cell.waitsUntil)}</span>}
          {cell.outcome && <span className="max-w-[14rem] truncate text-[12px] text-danger" title={cell.outcome}>failed: {cell.outcome}</span>}
          {set}
        </span>
      )
    case 'other':
      return <span className="flex flex-col items-start gap-0.5"><span className="text-[13px] text-ink-muted">serves it from <Mono>{cell.source}</Mono></span>{set}</span>
    case 'skipped':
      return <span className="flex flex-col items-start gap-0.5"><span className="text-[13px] text-warning">skipped: {cell.reason}</span>{add}</span>
  }
  return add ?? <span className="text-ink-muted">·</span>
}

/** A rule for a name in a channel, from a source (spec 0033), set from the catalogue's matrix or
 * the queue: PUT …/sources/{name}. The channel is given, or chosen among channels. The tenant's own
 * source is never automatic. */
export function RuleDialog(props: {
  open: boolean
  channel: string
  channels?: string[]
  name: string
  source: string
  mode?: string
  /** the entry, the channel's rule for the name and the channel's hold: what happens next */
  entry?: Pick<CatalogueEntry, 'versions' | 'held_until'>
  rule?: Pick<SourceRow, 'source' | 'mode' | 'current' | 'waits_until'>
  hold?: string
  onClose: () => void
  onSaved: () => void
}) {
  const { client, api } = useTenant()
  const [mode, setMode] = useState('manual')
  const [channel, setChannel] = useState('')
  const [opened, setOpened] = useState(false)
  const { busy, failure, act, setFailure } = useAction('The rule')
  if (props.open !== opened) {
    setOpened(props.open)
    if (props.open) {
      const kept = props.rule?.source === props.source && (props.rule.mode === 'auto' || props.rule.mode === 'manual') ? props.rule.mode : undefined
      setMode(props.mode ?? kept ?? (props.source === 'tenant' ? 'manual' : 'auto'))
      setChannel(props.channel || (props.channels?.[0] ?? ''))
      setFailure(null)
    }
  }
  const save = () => {
    if (!channel) return
    act(() => client.put(`${api}/channels/${seg(channel)}/sources/${seg(props.name)}`, { body: { source: props.source, mode } })).then((ok) => {
      if (ok) {
        props.onClose()
        props.onSaved()
      }
    })
  }
  return (
    <Confirm open={props.open} title={`A rule for ${props.name}${props.channel ? ` in ${props.channel}` : ''}`} confirm="Set rule" disabled={busy || !channel}
      onConfirm={save} onClose={props.onClose}>
      <p>The channel serves <Mono>{props.name}</Mono> from <Mono>{props.source}</Mono>: <b>auto</b> takes each new version once its holds have passed
        (the source's, and the channel's for automatic placements); <b>manual</b> takes the versions placed by hand.</p>
      {!props.channel && (
        <Field label="Channel">
          <select className="field" value={channel} onChange={(e) => setChannel(e.target.value)}>
            {(props.channels ?? []).map((c) => <option key={c}>{c}</option>)}
          </select>
        </Field>
      )}
      <Field label="Mode" hint="the tenant's own source is never automatic: it is everyone who may publish in the tenant">
        <select className="field" value={mode} onChange={(e) => setMode(e.target.value)} data-autofocus>
          <option value="auto" disabled={props.source === 'tenant'}>auto</option>
          <option value="manual">manual</option>
        </select>
      </Field>
      <p className="text-[13px]" aria-label="what happens"><b>What happens:</b> {whatHappens(mode, props.entry,
        props.rule?.source === props.source ? props.rule.current : undefined, props.hold, mode === props.rule?.mode ? props.rule?.waits_until : undefined)}</p>
      {failure != null && <ErrorBox error={failure} />}
    </Confirm>
  )
}

/** What a rule does next (spec 0041, the queue's rule dialog): under manual, nothing is placed by
 * itself; under auto, the newest version the channel does not have yet is placed at the placement
 * pass's look once the later of its holds has passed: its source's (held_until) and the channel's
 * (from each build's arrival), or when the rule says it places next (waits_until). */
export function whatHappens(mode: string, e: Pick<CatalogueEntry, 'versions' | 'held_until'> | undefined, current: string | undefined, hold: string | undefined,
  waitsUntil?: string): string {
  const newest = e?.versions[0]
  const next = newest && (!current || compareVersions(newest, current) > 0) ? newest : ''
  if (mode !== 'auto') {
    return next ? `nothing is placed by itself: ${next} waits for a placement by hand.` : 'nothing is placed by itself; a newer version waits for a placement by hand.'
  }
  if (!next) return `nothing newer than ${current} yet: each new version is placed once its holds have passed.`
  if (waitsUntil) return `${next} is placed at the placement pass's first look after ${when(waitsUntil)}.`
  const holds = []
  const until = e?.held_until?.[next]
  if (until) holds.push(`its source's hold ends ${when(until)}`)
  if (holdWords(hold) !== 'none') holds.push(`the channel's hold, ${holdWords(hold)} after each build's arrival, has passed`)
  if (holds.length === 0) return `${next} is placed at the placement pass's next look (within a minute).`
  return `${next} is placed at the placement pass's look once ${holds.join(' and ')}${holds.length > 1 ? ' (the later of the two)' : ''}.`
}

interface ChannelRow {
  name: string
  kind: string
  package_type?: string
}

/** An entry's page (spec 0041): a name with every source of it side by side, each channel's rule for
 * it edited in place, and each source's versions with their builds (placing, withdrawing, purging),
 * its licence; the name's reports. The route with a source names the same page. */
export function CatalogueNamePage() {
  const { name = '' } = useParams()
  const { client, api, base, perms } = useTenant()
  const home = useHome()
  const entries = useLoad(() => client.get<{ entries: CatalogueEntry[] }>(`${api}/catalogue?${new URLSearchParams({ name })}`), [api, name])
  // the signed channels of the catalogue's type (the default) where the caller may place the name
  const channels = useLoad(() => client.get<{ channels: ChannelRow[] }>(`${api}/channels`), [api])
  const targets = (channels.data?.data.channels ?? []).filter((c) => c.kind === 'signed' && !c.package_type && perms.mayPromote(c.name, name)).map((c) => c.name)
  const list = (entries.data?.data.entries ?? []).filter((e) => e.name === name)
  const meta = list.find((e) => e.icon || e.summary) ?? list[0]
  const reload = () => {
    entries.reload()
    home?.reload()
  }
  return (
    <>
      <PageTitle eyebrow={<Link to={`${base}/catalogue`} className="no-underline">Catalogue</Link>}
        title={<span className="flex items-center gap-3">
          {meta && !removedSource(meta.source) ? <EntryIcon path={entryPath(api, meta.source, name)} icon={meta.icon} size={64} /> : <EntryIcon path="" size={64} />}
          <span className="font-mono">{name}</span></span>}>
        {list.length > 1 && <span className="pb-1 text-ink-muted">from {list.length} sources</span>}
      </PageTitle>
      {meta?.summary && <p className="m-0"><EntrySummary summary={meta.summary} /></p>}
      {entries.error != null && <ErrorBox error={entries.error} />}
      {channels.error != null && <ErrorBox error={channels.error} />}
      {home?.reads && list.length > 0 && <RulesCard name={name} entries={list} reads={home.reads} onSaved={reload} />}
      {!entries.data ? (entries.error ? null : <Loading />) : list.length === 0 ? <Empty>No build of it in the catalogue.</Empty> : (
        list.map((e, i) => (
          removedSource(e.source)
            ? <Note key={`${e.source} ${i}`}>From <Mono>{e.source}</Mono>: a removed source's entry is not read by name; its releases stay where they are.</Note>
            : <SourceSection key={`${e.source} ${i}`} entry={e} many={list.length > 1} targets={targets} onChanged={reload} />
        ))
      )}
      {list.length > 0 && <Reports ext={name} />}
    </>
  )
}

/** A channel's rule for the name being edited: its source and mode. */
interface Draft {
  source: string
  mode: string
}

/** What a rule's draft changes, per channel: the PUT …/sources/{name} bodies, the unchanged left out. */
export function ruleChanges(rules: Record<string, Pick<SourceRow, 'source' | 'mode'> | undefined>, drafts: Record<string, Draft>) {
  const out: { channel: string; body: { source: string; mode?: string }; from: string; to: string }[] = []
  for (const [channel, d] of Object.entries(drafts).sort()) {
    const r = rules[channel]
    if (!d.source || (r?.source === d.source && (r?.mode ?? '') === d.mode)) continue
    const body: { source: string; mode?: string } = { source: d.source }
    if (d.mode === 'auto' || d.mode === 'manual') body.mode = d.mode
    out.push({ channel, body, from: r?.source ? `${r.source} ${modeWords(r.mode)}` : 'no rule', to: `${d.source} ${modeWords(body.mode ?? r?.mode)}` })
  }
  return out
}

/** Each channel's rule for a name, edited in place: the changes listed until Save sends one PUT per
 * changed channel. */
function RulesCard({ name, entries, reads, onSaved }: { name: string; entries: CatalogueEntry[]; reads: HomeReads; onSaved: () => void }) {
  const { client, api, perms } = useTenant()
  const [drafts, setDrafts] = useState<Record<string, Draft>>({})
  const [clearing, setClearing] = useState('')
  const { busy, failure, act } = useAction('A rule', onSaved)
  // the name's downloads per channel, over the statistics' default 30 days
  const downloads = useLoad(() => client.get<{ rows: { channel?: string; count: number }[] }>(
    `${api}/stats/downloads?${new URLSearchParams({ extension: name, group: 'channel' })}`), [api, name])
  const count = (c: string) => (downloads.data?.data.rows ?? []).filter((r) => r.channel === c).reduce((n, r) => n + r.count, 0)
  const rules: Record<string, SourceRow | undefined> = {}
  for (const c of reads.channels) rules[c.name] = c.sources.find((s) => s.name === name && s.source)
  const draft = (c: string): Draft => drafts[c] ?? { source: rules[c]?.source ?? '', mode: rules[c]?.mode ?? '' }
  const sources = [...new Set(entries.map((e) => e.source).filter((s) => !removedSource(s)))].sort()
  const changes = ruleChanges(rules, drafts)
  const save = () => {
    act(async () => {
      for (const ch of changes) await client.put(`${api}/channels/${seg(ch.channel)}/sources/${seg(name)}`, { body: ch.body })
    }).then((ok) => {
      if (ok) setDrafts({})
      onSaved()
    })
  }
  return (
    <Card title="Rules" actions={<Button tone="brand" disabled={busy || changes.length === 0} onClick={save}>Save</Button>}>
      <div className="flex flex-col gap-3">
        <Table rows={reads.channels.map((c) => c.name)} rowKey={(c) => c} columns={[
          ['Channel', (c) => <Mono>{c}</Mono>],
          ['Now', (c) => {
            const r = rules[c]
            if (!r) return <span className="text-ink-muted">no rule</span>
            return (
              <span className="flex flex-wrap items-center gap-1">
                <Mono>{r.source}</Mono><Badge>{modeWords(r.mode)}</Badge>{r.current && <Mono>{r.current}</Mono>}
                {r.waiting && <span className="chip bg-warning-soft text-warning">→ waits&nbsp;<Mono>{r.waiting}</Mono></span>}
              </span>
            )
          }],
          ['Source', (c) => {
            const d = draft(c)
            const choices = [...new Set([...sources, ...(rules[c]?.source ? [rules[c]!.source] : [])])].sort()
            return (
              <select aria-label={`${c} takes it from`} className="field py-1 font-mono text-[13px]" value={d.source} disabled={!perms.channelAdmin(c)}
                onChange={(e) => setDrafts({ ...drafts, [c]: { source: e.target.value, mode: e.target.value === 'tenant' && d.mode === 'auto' ? 'manual' : d.mode } })}>
                {!rules[c] && <option value="">no rule</option>}
                {choices.map((s) => <option key={s} value={s} disabled={removedSource(s)}>{s}</option>)}
              </select>
            )
          }],
          ['Mode', (c) => {
            const d = draft(c)
            return (
              <select aria-label={`${c} rule's mode`} className="field py-1 text-[13px]" value={d.mode} disabled={!perms.channelAdmin(c) || !d.source}
                onChange={(e) => setDrafts({ ...drafts, [c]: { ...d, mode: e.target.value } })}>
                {d.mode !== 'auto' && d.mode !== 'manual' && <option value={d.mode}>{modeWords(d.mode)}</option>}
                <option value="auto" disabled={d.source === 'tenant'}>auto</option>
                <option value="manual">manual</option>
              </select>
            )
          }],
          ['Downloads', (c) => (downloads.data ? <span className="text-[13px]">{count(c)}</span> : '')],
          ['', (c) => (
            <span role="group" aria-label={`${c} rule actions`} className="flex flex-wrap gap-1.5">
              <PromoteButton channel={c} ext={name} />
              {rules[c] && perms.channelAdmin(c) && <Button disabled={busy} onClick={() => setClearing(c)}>Clear…</Button>}
            </span>
          )],
        ]} />
        {downloads.data && <span className="text-[12px] text-ink-muted">Downloads: the last 30 days.</span>}
        {downloads.error != null && <p role="alert" className="m-0 text-[12px] text-danger">The downloads could not be read: {errorText(downloads.error)}</p>}
        {changes.length > 0 && (
          <div role="status" aria-label="unsaved changes" className="text-[13px]">
            Not saved yet:
            <ul className="m-0 pl-5">{changes.map((ch) => <li key={ch.channel}><Mono>{ch.channel}</Mono>: {ch.from} → {ch.to}</li>)}</ul>
          </div>
        )}
        {failure != null && <ErrorBox error={failure} />}
      </div>
      <Confirm open={clearing !== ''} title={`Clear the rule for ${name} in ${clearing}?`} confirm="Clear" danger
        onConfirm={() => {
          const c = clearing
          setClearing('')
          act(() => client.del(`${api}/channels/${seg(c)}/sources/${seg(name)}`)).then(() => onSaved())
        }}
        onClose={() => setClearing('')}>
        <p>Nothing serves <Mono>{name}</Mono> in <Mono>{clearing}</Mono> until its next release lands, which sets the source again, or a follow
          takes it.</p>
      </Confirm>
    </Card>
  )
}

/** A source's builds of a name: its versions with their builds and where each is placed; placing a
 * version, withdrawing a distributor's version (a server administrator), purging a build placed
 * nowhere; its licence. */
function SourceSection({ entry: e, many, targets, onChanged }: { entry: CatalogueEntry; many: boolean; targets: string[]; onChanged: () => void }) {
  const { client, api, perms } = useTenant()
  const { source, name } = e
  const path = entryPath(api, source, name)
  const versions = useLoad(() => client.get<{ versions: CatalogueVersion[] }>(path), [path])
  const reload = () => {
    versions.reload()
    onChanged()
  }
  const { busy, failure, act } = useAction('The entry', reload)
  const [placing, setPlacing] = useState<{ version: string; held: boolean } | null>(null)
  const [withdrawing, setWithdrawing] = useState('')
  const [reason, setReason] = useState('')
  const [purging, setPurging] = useState<{ b: CatalogueBuild; version: string } | null>(null)
  const [editing, setEditing] = useState(false)
  const done = (ok: boolean) => ok && reload()
  const actions = (v: string) => (many ? `${source} ${v} actions` : `${v} actions`)
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-3">
        <h2 className="m-0 text-[18px] font-bold leading-[26px]"><Mono>{source}</Mono></h2>
        {e.distributor && <span className="text-[13px] text-ink-muted">{e.distributor}'s</span>}
        <span className="flex items-center gap-2 text-ink-muted">Taken by <Rules rules={e.rules} /></span>
        {mayEditMeta(e, perms.tenantAdmin) && <span className="ml-auto"><Button disabled={busy} onClick={() => setEditing(true)}>Edit…</Button></span>}
      </div>
      {perms.tenantAdmin && <SourceLicence source={source} name={name} />}
      {failure != null && <ErrorBox error={failure} />}
      {versions.error instanceof ApiError && versions.error.status === 404 ? <Empty>No build of it from this source in the catalogue.</Empty>
        : versions.error ? <ErrorBox error={versions.error} /> : !versions.data ? <Loading /> : versions.data.data.versions.length === 0 ? <Empty>No builds.</Empty> : (
        versions.data.data.versions.map((v) => {
          const st = buildsState(v.builds)
          const held = st === 'held' || st === 'revoked'
          return (
            <Card key={v.version} title={<span className="flex items-center gap-2"><span className="font-mono">{v.version}</span> <Badge>{st}</Badge></span>}
              actions={(
                <span role="group" aria-label={actions(v.version)} className="flex gap-1.5">
                  {st !== 'withdrawn' && targets.length > 0 && <Button disabled={busy} onClick={() => setPlacing({ version: v.version, held })}>Place…</Button>}
                  {st !== 'withdrawn' && perms.server && e.publisher_id && (
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
      <MetaDialog open={editing} name={name} summary={e.summary} icon={e.icon} onClose={() => setEditing(false)}
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
    </div>
  )
}

/** A source's licence for a name (spec 0035: a licence belongs to a catalogue entry), read from the
 * tenant's licences. */
export function SourceLicence({ source, name }: { source: string; name: string }) {
  const { client, api } = useTenant()
  const list = useLoad(() => client.get<{ licenses: { name: string; entry: string; plan: string; holder?: string; holder_id: string; expires_at: string; status: string; visibility: string }[] }>(`${api}/licenses`), [api])
  // the licence keys the entry's live builds declare (spec 0035): what an import into it accepts
  const keys = useLoad(() => (removedSource(source) || source === 'tenant' ? Promise.resolve(null)
    : client.get<{ names: { name: string; keys: { kid: string; sources: { builds: number }[] }[] }[] }>(
      `${api}/license-keys/${seg(name)}?${new URLSearchParams({ source })}`)), [api, name, source])
  if (list.error) return <ErrorBox error={list.error} />
  if (!list.data) return null
  const ls = list.data.data.licenses.filter((l) => l.name === name && l.entry === source)
  const kids = (keys.data?.data.names ?? []).flatMap((n) => n.keys.map((k) => ({ kid: k.kid, builds: k.sources.reduce((x, s) => x + s.builds, 0) })))
  return (
    <ul className="m-0 list-none p-0 text-[13px]">
      {ls.length === 0 && <li className="text-ink-muted">No licence in this entry.</li>}
      {ls.map((l) => (
        <li key={l.entry + l.plan}>Licence: plan <b>{l.plan}</b> for {l.holder || <Mono>{l.holder_id}</Mono>}, expires {when(l.expires_at)} <Badge>{l.status}</Badge> <Badge>{l.visibility}</Badge></li>
      ))}
      {keys.error != null && <li role="alert" className="text-danger">The licence keys could not be read: {errorText(keys.error)}</li>}
      {keys.data && (kids.length === 0 ? <li className="text-ink-muted">Its builds declare no licence key.</li>
        : <li>Licence keys its builds declare: {kids.map((k) => <span key={k.kid} className="mr-2"><Mono>{k.kid}</Mono> ({k.builds} {k.builds === 1 ? 'build' : 'builds'})</span>)}</li>)}
    </ul>
  )
}

/** A failed read's message, in a line. */
export const errorText = (e: unknown) => (e instanceof Error ? e.message : String(e))

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
