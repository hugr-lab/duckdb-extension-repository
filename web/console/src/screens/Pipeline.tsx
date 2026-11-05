import { Fragment, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Loading, Mono, Note, Table } from '../components/ui'
import { useAction } from '../lib/action'
import { useLoad, useTenant } from '../lib/context'
import { useHome } from '../lib/home'
import { type Client, seg } from '../lib/http'
import { type IndexRelease, type OrderEdge, type StageWait, cellWaits, chainsOf, previousOf, stageWait } from '../lib/order'
import { type ChannelReads, type Decision, type HomeReads, actsManual } from '../lib/queue'
import { SourceLicence, modeWords, removedSource } from './Catalogue'
import { type Promoting, PromoteDialog, promote } from './Promote'
import { Reports } from './Reports'
import { holdWords } from './Sources'

// The pipeline (spec 0043): the channels' order drawn as stages, each with what it follows, its rules
// by mode, its DuckDB versions and hold, and its names; what waits for a stage from its previous one,
// Promote of the previous stage's current with the Report & licence panel before it, and Skip. A
// tenant administrator's view, from the home's reads and each stage's index of a name. The order is
// for display only: a promotion names its source each time, and the API decides.

/** A name's wait in a stage, from its previous stage: the cells, before the decisions. */
interface Waiting {
  from: string
  source: string
  cells: ReturnType<typeof cellWaits>
  /** the previous stage's releases of the name, for the version's builds' reports */
  prev: IndexRelease[]
}

const waitKey = (stage: string, name: string) => `${stage}|${name}`

/** A channel's index of a name (its releases in the full view of a tenant administrator). */
const indexOf = (client: Client, api: string, channel: string, name: string) =>
  client.get<{ releases: IndexRelease[] }>(`${api}/channels/${seg(channel)}/extensions/${seg(name)}`).then((r) => r.data.releases)

/** The stages' DuckDB versions, and each name's wait: for a stage S with a previous stage P, a name
 * S's rule takes from a source P's flat path serves it from, with a current in P. */
export async function readPipeline(client: Client, api: string, reads: HomeReads) {
  const names = reads.order.flatMap((e) => [e.channel, e.next]).filter((c, i, a) => a.indexOf(c) === i)
  const versions = new Map(await Promise.all(names.map(async (c) => {
    const d = await client.get<{ duckdb_versions?: { duckdb_version: string }[] }>(`${api}/channels/${seg(c)}`)
    return [c, (d.data.duckdb_versions ?? []).map((v) => v.duckdb_version)] as const
  })))
  const index = new Map<string, Promise<IndexRelease[]>>()
  const rows = (c: string, n: string) => {
    const k = waitKey(c, n)
    if (!index.has(k)) index.set(k, indexOf(client, api, c, n))
    return index.get(k)!
  }
  const waits = new Map<string, Waiting>()
  await Promise.all(reads.order.map(async ({ channel: from, next: stage }) => {
    const p = reads.channels.find((c) => c.name === from)
    const s = reads.channels.find((c) => c.name === stage)
    if (!p || !s) return
    await Promise.all(s.sources.map(async (r) => {
      const pr = p.sources.find((x) => x.name === r.name)
      if (!r.source || removedSource(r.source) || pr?.source !== r.source || !pr.current) return
      const [prev, here] = await Promise.all([rows(from, r.name), rows(stage, r.name)])
      const cells = cellWaits(prev, here, versions.get(stage) ?? [])
      if (cells.length > 0) waits.set(waitKey(stage, r.name), { from, source: r.source, cells, prev })
    }))
  }))
  return { versions, waits }
}

/** What waits in a stage, less the skips: name → its wait. */
export function stageWaits(stage: string, waits: Map<string, Waiting>, ds: Decision[]) {
  const out = new Map<string, StageWait & Waiting>()
  for (const [k, w] of waits) {
    const [s, name] = k.split('|')
    if (s !== stage) continue
    const left = stageWait(w.cells, ds, stage, w.source, name)
    if (left) out.set(name, { ...w, ...left })
  }
  return out
}

export function Pipeline({ onEdit }: { onEdit: () => void }) {
  const { client, api } = useTenant()
  const home = useHome()
  const reads = home?.reads
  const load = useLoad(() => (reads ? readPipeline(client, api, reads) : Promise.resolve(null)), [reads])
  if (!home) return null
  if (home.error) return <ErrorBox error={home.error} />
  if (!reads) return <Loading />
  const known = new Set(reads.channels.map((c) => c.name))
  const chains = chainsOf(reads.order).map((c) => c.filter((x) => known.has(x))).filter((c) => c.length > 0)
  if (chains.length === 0) return <Empty>No order is set.</Empty>
  const data = load.data
  const reload = home.reload
  return (
    <div className="flex flex-col gap-6">
      {load.error != null && <ErrorBox error={load.error} />}
      {chains.map((chain) => (
        <section key={chain[0]} aria-label={`pipeline ${chain.join(' → ')}`} className="flex flex-col gap-4">
          <div className="flex flex-wrap items-stretch gap-2">
            {chain.map((c, i) => (
              <Fragment key={c}>
                {i > 0 && <span aria-hidden className="self-center text-ink-muted">→</span>}
                <StageCard ch={reads.channels.find((x) => x.name === c)!} versions={data?.versions.get(c)}
                  waiting={data ? stageWaits(c, data.waits, reads.decisions).size : undefined} />
              </Fragment>
            ))}
          </div>
          {chain.map((c) => (
            <StageTable key={c} ch={reads.channels.find((x) => x.name === c)!} order={reads.order} reads={reads}
              waits={data ? stageWaits(c, data.waits, reads.decisions) : undefined} loading={load.loading} onChanged={reload} />
          ))}
        </section>
      ))}
      <span><Button onClick={onEdit}>Edit the order…</Button></span>
    </div>
  )
}

/** A stage's card: its follows, its rules by mode, its DuckDB versions, its hold and its names. */
function StageCard({ ch, versions, waiting }: { ch: ChannelReads; versions?: string[]; waiting?: number }) {
  const { base } = useTenant()
  const rules = ch.sources.filter((r) => r.source)
  const modes = new Map<string, number>()
  for (const r of rules) if (r.mode) modes.set(r.mode, (modes.get(r.mode) ?? 0) + 1)
  return (
    <div role="group" aria-label={`stage ${ch.name}`} className="flex min-w-[14rem] flex-col gap-1 rounded-md border border-line bg-surface p-4 text-[13px]">
      <Link className="font-mono text-[15px] font-semibold no-underline" to={`${base}/channels/${seg(ch.name)}`}>{ch.name}</Link>
      <span className="text-ink-muted">{ch.follows.length > 0 ? `follows ${ch.follows.map((f) => f.source).join(', ')}` : 'follows nothing'}</span>
      <span>rules: {modes.size === 0 ? 'none by mode' : [...modes].sort().map(([m, n]) => `${n} ${m}`).join(', ')}</span>
      <span>DuckDB: {versions === undefined ? '…' : versions.length === 0 ? 'none' : <Mono>{versions.join(' ')}</Mono>}</span>
      <span>hold {holdWords(ch.hold)} · {rules.length} {rules.length === 1 ? 'name' : 'names'}</span>
      {!!waiting && <span className="chip self-start bg-warning-soft text-warning">{waiting === 1 ? '1 version waits' : `${waiting} versions wait`}</span>}
    </div>
  )
}

/** A stage's names: each one's rule and current version, and what waits from the previous stage. */
function StageTable({ ch, order, reads, waits, loading, onChanged }: {
  ch: ChannelReads; order: OrderEdge[]; reads: HomeReads; waits?: Map<string, StageWait & Waiting>; loading: boolean; onChanged: () => void
}) {
  const [open, setOpen] = useState('')
  const [said, setSaid] = useState('')
  const prev = previousOf(order, ch.name)
  const p = reads.channels.find((c) => c.name === prev)
  // the stage's names, and those of the previous stage it has no rule for
  const names = [...new Set([...ch.sources.filter((r) => r.source).map((r) => r.name), ...(p?.sources.filter((r) => r.source).map((r) => r.name) ?? [])])].sort()
  const n = waits?.size ?? 0
  const title = prev ? (n === 0 ? `Nothing waits for ${ch.name}` : n === 1 ? `1 version waits for ${ch.name}` : `${n} versions wait for ${ch.name}`) : ch.name
  return (
    <Card title={<span className="flex items-center gap-2"><Mono>{ch.name}</Mono>{prev && <span className="text-[13px] font-normal text-ink-muted">after {prev}</span>}<span className="sr-only">: </span>{prev && <span className="text-[13px] font-normal">{title}</span>}</span>}>
      {said && <p role="status" className="m-0 mb-3 text-[13px] text-ink-muted">{said}</p>}
      {names.length === 0 ? <Empty>No name in this stage.</Empty> : (
        <Table rows={names} rowKey={(x) => x} columns={[
          ['Extension', (x) => <Mono>{x}</Mono>],
          ['Rule', (x) => <RuleWords ch={ch} name={x} prev={p} />],
          ['Current', (x) => <Mono>{ch.sources.find((r) => r.name === x && r.source)?.current ?? '·'}</Mono>],
          ['Waits', (x) => {
            const w = waits?.get(x)
            if (!w) return loading && !waits ? '…' : <span className="text-ink-muted">·</span>
            return (
              <span className="flex flex-col items-start gap-1">
                <span className="chip bg-warning-soft text-warning">→ waits&nbsp;<Mono>{w.version}</Mono>&nbsp;from {w.from}</span>
                <Button tone="plain" expanded={open === x} onClick={() => { setSaid(''); setOpen(open === x ? '' : x) }}>Report & licence</Button>
              </span>
            )
          }],
        ]} />
      )}
      {open && waits?.get(open) && <Panel key={open} stage={ch} name={open} wait={waits.get(open)!} onChanged={onChanged}
        onDone={(what) => { setOpen(''); setSaid(what); onChanged() }} />}
    </Card>
  )
}

/** A name's rule in a stage, as the pipeline words it: its mode, another source than the previous
 * stage's, or none. */
function RuleWords({ ch, name, prev }: { ch: ChannelReads; name: string; prev?: ChannelReads }) {
  const r = ch.sources.find((s) => s.name === name && s.source)
  if (!r) return <span className="text-ink-muted">no rule</span>
  const ps = prev?.sources.find((s) => s.name === name && s.source)
  return (
    <span className="flex flex-col">
      <span><Badge>{modeWords(r.mode)}</Badge></span>
      {ps && ps.source !== r.source && <span className="text-[12px] text-ink-muted">serves it from <Mono>{r.source}</Mono></span>}
    </span>
  )
}

/** The Report & licence panel of a waiting version, before Promote; Skip under a manual rule. */
function Panel({ stage, name, wait, onChanged, onDone }: {
  stage: ChannelReads; name: string; wait: StageWait & Waiting; onChanged: () => void; onDone: (what: string) => void
}) {
  const { client, api, perms } = useTenant()
  const [promoting, setPromoting] = useState(false)
  const [skipping, setSkipping] = useState(false)
  const [p, setP] = useState<Promoting>({ target: stage.name, version: wait.version, narrow: false, notCurrent: false })
  const { busy, failure, act } = useAction('It', onChanged)
  const rule = stage.sources.find((r) => r.name === name && r.source)
  const bodies = [...new Set(wait.prev.filter((r) => r.version === wait.version).map((r) => r.body_hash ?? ''))].filter((b) => b !== '')
  const mayPromote = perms.mayPromote(stage.name, name) && perms.mayPublish(wait.from, name)
  const cells = wait.cells.filter((c) => c.version === wait.version)
  return (
    <div className="mt-4 flex flex-col gap-3 rounded-md border border-line p-4" role="region" aria-label={`${name} report and licence`}>
      <p className="m-0 text-[13px]"><Mono>{name} {wait.version}</Mono> from <Mono>{wait.source}</Mono> is {wait.from}'s current on{' '}
        {cells.map((c) => `${c.duckdb} ${c.platform}`).join(', ')}, and newer than what {stage.name} has or had there.</p>
      <SourceLicence source={wait.source} name={name} />
      {bodies.length === 0 ? <Reports ext={name} /> : bodies.map((b) => <Reports key={b} ext={name} body={b} />)}
      <span className="flex flex-wrap items-center gap-2">
        {mayPromote && <Button tone="brand" disabled={busy} onClick={() => setPromoting(true)}>Promote…</Button>}
        {rule && actsManual(stage, rule) && perms.tenantAdmin && <Button disabled={busy} onClick={() => setSkipping(true)}>Skip</Button>}
      </span>
      {!mayPromote && <Note>Promote takes promote on {name} in {stage.name} and publish in {wait.from}: you hold neither here.</Note>}
      {failure != null && <ErrorBox error={failure} />}
      <PromoteDialog open={promoting} ext={name} from={wait.from} targets={[stage.name]} versions={[wait.version]} p={p} setP={setP}
        onClose={() => setPromoting(false)} onSubmit={(q) => {
          setPromoting(false)
          let what = ''
          act(async () => { what = await promote(client, api, name, wait.from, q) }).then((ok) => ok && onDone(what))
        }} />
      <Confirm open={skipping} title={`Skip ${name} ${wait.version} in ${stage.name}?`} confirm="Skip" onClose={() => setSkipping(false)}
        onConfirm={() => {
          setSkipping(false)
          act(() => client.put(`${api}/decisions`, { body: { kind: 'skip', channel: stage.name, source: wait.source, name, version: wait.version } }))
            .then((ok) => ok && onDone(`${name} ${wait.version} skipped in ${stage.name}`))
        }}>
        <p>The version is left out of what waits for <Mono>{stage.name}</Mono> until a newer one waits, for every administrator of the tenant. It
          changes nothing else: a promotion or a placement by hand still takes it.</p>
      </Confirm>
    </div>
  )
}

/** Setting the channels' order (tenant administrators): each signed channel of the default type
 * names its next, or none. The whole order is sent, at the ETag read when the dialog opened; the API
 * refuses a cycle, two channels sharing a next, and a channel its own next. */
export function OrderDialog({ open, channels, onClose, onSaved }: { open: boolean; channels: string[]; onClose: () => void; onSaved: () => void }) {
  const { client, api } = useTenant()
  const [read, setRead] = useState<{ etag: string; next: Record<string, string> } | null>(null)
  const { busy, failure, act, setFailure } = useAction('The order')
  useEffect(() => {
    if (!open) return
    setRead(null)
    setFailure(null)
    let alive = true
    client.get<{ order: OrderEdge[] }>(`${api}/channel-order`).then(
      (r) => alive && setRead({ etag: r.etag, next: Object.fromEntries(r.data.order.map((e) => [e.channel, e.next])) }),
      (e) => alive && setFailure(e))
    return () => {
      alive = false
    }
  }, [open, api, client, setFailure])
  const save = () => {
    if (!read) return
    const order = channels.filter((c) => read.next[c]).map((c) => ({ channel: c, next: read.next[c] }))
    act(() => client.put(`${api}/channel-order`, { body: { order }, ifMatch: read.etag })).then((ok) => {
      if (ok) {
        onClose()
        onSaved()
      }
    })
  }
  return (
    <Confirm open={open} title="The channels' order" confirm="Save" disabled={busy || !read} onConfirm={save} onClose={onClose}>
      <p>Each channel names the channel a version goes to next, as nightly → staging → prod. The order is for the console only: the pipeline,
        its columns and the source a promotion offers. kista places, promotes and serves as before.</p>
      {!read ? (failure == null && <Loading />) : (
        <div className="flex flex-col gap-2">
          {channels.map((c) => (
            <label key={c} className="flex items-center gap-2 text-[13px]">
              <Mono>{c}</Mono> →
              <select aria-label={`next of ${c}`} className="field py-1 text-[13px]" value={read.next[c] ?? ''}
                onChange={(e) => setRead({ ...read, next: { ...read.next, [c]: e.target.value } })}>
                <option value="">nothing</option>
                {channels.filter((x) => x !== c).map((x) => <option key={x}>{x}</option>)}
              </select>
            </label>
          ))}
        </div>
      )}
      {failure != null && <ErrorBox error={failure} />}
    </Confirm>
  )
}
