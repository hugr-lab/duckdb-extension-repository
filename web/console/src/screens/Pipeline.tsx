import { Fragment, useEffect, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { Badge, Button, Confirm, Empty, ErrorBox, Loading, Mono, Note } from '../components/ui'
import { useAction } from '../lib/action'
import { useLoad, useTenant } from '../lib/context'
import { useHome } from '../lib/home'
import { type Client, seg } from '../lib/http'
import { type IndexRelease, type OrderEdge, type PurgedRelease, type StageWait, cellWaits, chainsOf, orderBody, previousOf, slotOf, stageWait } from '../lib/order'
import { type ChannelReads, type Decision, type HomeReads, actsManual } from '../lib/queue'
import { SourceLicence, modeWords, removedSource } from './Catalogue'
import { type Promoting, PromoteDialog, promote } from './Promote'
import { Reports } from './Reports'
import { holdWords } from './Sources'

// The pipeline (spec 0043, the owner's mockup Catalogue-Pipeline): the channels' order drawn as
// stages, each with what it follows, its rules by mode, its DuckDB versions and hold, and its names;
// then one matrix per chain, a row per name and a column per stage, so a name's progress reads in
// one row: its rule in each stage, its current version, and what waits from the previous stage, with
// the Report & licence panel before Promote of the previous stage's current, and Skip. A tenant
// administrator's view, from the home's reads and each stage's index of a name. The order is for
// display only: a promotion names its source each time, and the API decides.

/** A name's wait in a stage, from its previous stage: the cells, before the decisions. */
interface Waiting {
  from: string
  source: string
  cells: ReturnType<typeof cellWaits>
  /** the previous stage's releases of the name, for the version's builds' reports */
  prev: IndexRelease[]
}

type Waits = Map<string, StageWait & Waiting>

const waitKey = (stage: string, name: string) => `${stage}|${name}`

/** A channel's index of a name: its releases in the full view of a tenant administrator, and those
 * it purged (an administrator's). */
const indexOf = (client: Client, api: string, channel: string, name: string) =>
  client.get<{ releases: IndexRelease[]; purged?: PurgedRelease[] }>(`${api}/channels/${seg(channel)}/extensions/${seg(name)}`)
    .then((r) => ({ releases: r.data.releases, purged: r.data.purged ?? [] }))

/** The stages' DuckDB versions, and each name's wait: for a stage S with a previous stage P, a name
 * S's rule takes from a source P's flat path serves it from, with a current in P. */
export async function readPipeline(client: Client, api: string, reads: HomeReads) {
  const names = reads.order.flatMap((e) => [e.channel, e.next]).filter((c, i, a) => a.indexOf(c) === i)
  const versions = new Map(await Promise.all(names.map(async (c) => {
    const d = await client.get<{ duckdb_versions?: { duckdb_version: string }[] }>(`${api}/channels/${seg(c)}`)
    return [c, (d.data.duckdb_versions ?? []).map((v) => v.duckdb_version)] as const
  })))
  const index = new Map<string, ReturnType<typeof indexOf>>()
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
      const cells = cellWaits(prev.releases, here.releases, versions.get(stage) ?? [], here.purged)
      if (cells.length > 0) waits.set(waitKey(stage, r.name), { from, source: r.source, cells, prev: prev.releases })
    }))
  }))
  return { versions, waits }
}

/** What waits in a stage, less the skips: name → its wait. */
export function stageWaits(stage: string, waits: Map<string, Waiting>, ds: Decision[]): Waits {
  const out: Waits = new Map()
  for (const [k, w] of waits) {
    const [s, name] = k.split('|')
    if (s !== stage) continue
    const left = stageWait(w.cells, ds, stage, w.source, name)
    if (left) out.set(name, { ...w, ...left })
  }
  return out
}

/** A stage's rules counted by mode, as the console words a mode ("as it lands" for a rule an arrival
 * wrote). */
export function rulesByMode(ch: ChannelReads): string {
  const modes = new Map<string, number>()
  for (const r of ch.sources) if (r.source) modes.set(modeWords(r.mode), (modes.get(modeWords(r.mode)) ?? 0) + 1)
  return modes.size === 0 ? 'none' : [...modes].sort().map(([m, n]) => `${n} ${m}`).join(', ')
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
  // undefined while reading (or when the read failed: then the error says so)
  const waitsOf = (c: string) => (load.data ? stageWaits(c, load.data.waits, reads.decisions) : undefined)
  return (
    <div className="flex flex-col gap-6">
      {load.error != null && <ErrorBox error={load.error} />}
      {chains.map((chain) => (
        <section key={chain[0]} aria-label={`pipeline ${chain.join(' → ')}`} className="flex flex-col gap-4">
          <div className="flex flex-wrap items-stretch gap-2">
            {chain.map((c, i) => (
              <Fragment key={c}>
                {i > 0 && <span aria-hidden className="self-center text-ink-muted">→</span>}
                <StageCard ch={reads.channels.find((x) => x.name === c)!} versions={load.data?.versions.get(c)} first={i === 0}
                  waits={waitsOf(c)} failed={load.error != null} />
              </Fragment>
            ))}
          </div>
          <ChainMatrix chain={chain.map((c) => reads.channels.find((x) => x.name === c)!)} waitsOf={waitsOf} failed={load.error != null}
            order={reads.order} onChanged={home.reload} />
        </section>
      ))}
      <span><Button onClick={onEdit}>Edit the order…</Button></span>
    </div>
  )
}

/** What waits for a stage, in words: reading, failed, or how many versions. */
function waitWords(stage: string, waits: Waits | undefined, failed: boolean) {
  if (failed) return 'What waits could not be read'
  if (!waits) return 'Reading what waits…'
  const n = waits.size
  return n === 0 ? `Nothing waits for ${stage}` : n === 1 ? `1 version waits for ${stage}` : `${n} versions wait for ${stage}`
}

/** A stage's card: its follows, its rules by mode, its DuckDB versions, its hold and its names, and
 * (after the first stage) what waits for it. */
function StageCard({ ch, versions, first, waits, failed }: { ch: ChannelReads; versions?: string[]; first: boolean; waits?: Waits; failed: boolean }) {
  const { base } = useTenant()
  const rules = ch.sources.filter((r) => r.source)
  return (
    <div role="group" aria-label={`stage ${ch.name}`} className="flex min-w-[14rem] flex-col gap-1 rounded-md border border-line bg-surface p-4 text-[13px]">
      <Link className="font-mono text-[15px] font-semibold no-underline" to={`${base}/channels/${seg(ch.name)}`}>{ch.name}</Link>
      <span className="text-ink-muted">{ch.follows.length > 0 ? `follows ${ch.follows.map((f) => f.source).join(', ')}` : 'follows nothing'}</span>
      <span>rules: {rulesByMode(ch)}</span>
      <span>DuckDB: {versions === undefined ? '…' : versions.length === 0 ? 'none' : <Mono>{versions.join(' ')}</Mono>}</span>
      <span>hold {holdWords(ch.hold)} · {rules.length} {rules.length === 1 ? 'name' : 'names'}</span>
      {!first && (
        <span className={waits?.size ? 'chip self-start bg-warning-soft text-warning' : failed ? 'text-danger' : 'text-ink-muted'}>
          {waitWords(ch.name, waits, failed)}
        </span>
      )}
    </div>
  )
}

/** A chain's matrix: a row per name of any of its stages, a column per stage (arrows between them),
 * each cell the name's rule there, its current version and what waits from the previous stage. */
function ChainMatrix({ chain, waitsOf, failed, order, onChanged }: {
  chain: ChannelReads[]; waitsOf: (c: string) => Waits | undefined; failed: boolean; order: OrderEdge[]; onChanged: () => void
}) {
  const [open, setOpen] = useState<{ stage: string; name: string } | null>(null)
  const [said, setSaid] = useState('')
  // the panel closed: focus goes back to the button that opened it (when it is still there)
  const [refocus, setRefocus] = useState('')
  useEffect(() => {
    if (!refocus) return
    document.getElementById(refocus)?.focus()
    setRefocus('')
  }, [refocus])
  const close = () => {
    if (open) setRefocus(panelIds(open.stage, open.name).button)
    setOpen(null)
  }
  const names = [...new Set(chain.flatMap((c) => c.sources.filter((r) => r.source).map((r) => r.name)))].sort()
  const openWait = open ? waitsOf(open.stage)?.get(open.name) : undefined
  const stage = chain.find((c) => c.name === open?.stage)
  if (names.length === 0) return <Empty>No name in these stages.</Empty>
  return (
    <div className="flex flex-col gap-3">
      {said && <p role="status" className="m-0 text-[13px] text-ink-muted">{said}</p>}
      <div className="overflow-x-auto rounded-md border border-line">
        <table className="w-full border-collapse">
          <thead className="bg-surface-soft">
            <tr>
              <th className="th pl-4">Extension</th>
              {chain.map((c, i) => (
                <th key={c.name} className="th">
                  <span className="flex items-center gap-2">
                    {i > 0 && <span aria-hidden className="font-normal text-ink-muted">→</span>}
                    <Mono>{c.name}</Mono>
                  </span>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {names.map((name) => (
              <tr key={name} className="border-t border-line hover:bg-row">
                <td className="td pl-4"><Mono>{name}</Mono></td>
                {chain.map((c, i) => (
                  <td key={c.name} className="td">
                    <StageCell ch={c} prev={i > 0 ? chain[i - 1] : undefined} name={name} waits={i > 0 ? waitsOf(c.name) : undefined}
                      reading={i > 0 && !failed && !waitsOf(c.name)} expanded={open?.stage === c.name && open.name === name}
                      onToggle={() => {
                        setSaid('')
                        if (open?.stage === c.name && open.name === name) close()
                        else setOpen({ stage: c.name, name })
                      }} />
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {open && openWait && stage && previousOf(order, stage.name) === openWait.from && (
        <Panel key={`${open.stage}|${open.name}`} stage={stage} name={open.name} wait={openWait} onChanged={onChanged}
          onClose={close} onDone={(what) => { close(); setSaid(what); onChanged() }} />
      )}
    </div>
  )
}

/** The ids of a (stage, name)'s Report & licence button and of the panel it opens (channel and
 * extension names are path segments: letters, digits, _ and -). */
const panelIds = (stage: string, name: string) => ({ button: `rl-open-${stage}-${name}`, panel: `rl-panel-${stage}-${name}` })

/** A name's cell in a stage: its rule (mode, current version; "serves it from …" when it names
 * another source than the previous stage's), "no rule", and what waits from the previous stage. */
function StageCell({ ch, prev, name, waits, reading, expanded, onToggle }: {
  ch: ChannelReads; prev?: ChannelReads; name: string; waits?: Waits; reading: boolean; expanded: boolean; onToggle: () => void
}) {
  const r = ch.sources.find((s) => s.name === name && s.source)
  if (!r) return <span className="text-[13px] text-ink-muted">no rule</span>
  const ps = prev?.sources.find((s) => s.name === name && s.source)
  const w = waits?.get(name)
  return (
    <span className="flex flex-col items-start gap-0.5">
      <span className="flex items-center gap-1"><Badge>{modeWords(r.mode)}</Badge>{r.current && <Mono>{r.current}</Mono>}</span>
      {ps && ps.source !== r.source && <span className="text-[12px] text-ink-muted">serves it from <Mono>{r.source}</Mono></span>}
      {reading && <span className="text-[12px] text-ink-muted">…</span>}
      {w && (
        <>
          <span className="chip bg-warning-soft text-warning">→ waits&nbsp;<Mono>{w.version}</Mono>&nbsp;from {w.from}</span>
          {/* a row's cells of several stages may each wait: the stage names the button too */}
          <Button tone="plain" expanded={expanded} onClick={onToggle} label={`Report & licence of ${name} for ${ch.name}`}
            id={panelIds(ch.name, name).button} controls={panelIds(ch.name, name).panel}>Report & licence</Button>
        </>
      )}
    </span>
  )
}

/** The Report & licence panel of a waiting version, before Promote; Skip under a manual rule. */
function Panel({ stage, name, wait, onChanged, onClose, onDone }: {
  stage: ChannelReads; name: string; wait: StageWait & Waiting; onChanged: () => void; onClose: () => void; onDone: (what: string) => void
}) {
  const { client, api, base, perms } = useTenant()
  const [promoting, setPromoting] = useState(false)
  const [skipping, setSkipping] = useState(false)
  const [p, setP] = useState<Promoting>({ target: stage.name, version: wait.version, narrow: false, notCurrent: false })
  const { busy, failure, act } = useAction('It', onChanged)
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => ref.current?.focus(), [])
  const rule = stage.sources.find((r) => r.name === name && r.source)
  const missing = [
    !perms.mayPromote(stage.name, name) && `promote on ${name} in ${stage.name}`,
    !perms.mayPublish(wait.from, name) && `publish on ${name} in ${wait.from}`,
  ].filter((x): x is string => !!x)
  const cells = wait.cells.filter((c) => c.version === wait.version)
  // the version's builds current in the waiting cells of the previous stage, each (platform, slot)
  // once, with a known body
  const builds = wait.prev
    .filter((r) => r.version === wait.version && r.body_hash && cells.some((c) => c.platform === r.platform && r.current_for.includes(c.duckdb)))
    .map((r) => ({ body: r.body_hash!, what: `${r.platform} · ${slotOf(r)}` }))
    .filter((b, i, a) => a.findIndex((x) => x.what === b.what) === i)
    .sort((a, b) => a.what.localeCompare(b.what))
  return (
    <div ref={ref} id={panelIds(stage.name, name).panel} tabIndex={-1} className="flex flex-col gap-3 rounded-md border border-line p-4 outline-none"
      role="region" aria-label={`${name} report and licence`}>
      <p className="m-0 text-[13px]"><Mono>{name} {wait.version}</Mono> from <Mono>{wait.source}</Mono> is {wait.from}'s current on{' '}
        {cells.map((c) => `${c.duckdb} ${c.platform}`).join(', ')}, and newer than what {stage.name} has or had there.</p>
      <SourceLicence source={wait.source} name={name} />
      {builds.map((b) => (
        <section key={b.what} aria-label={`reports of ${b.what}`} className="flex flex-col gap-1">
          <h4 className="m-0 text-[13px] font-semibold"><Mono>{b.what}</Mono></h4>
          <Reports ext={name} body={b.body} />
        </section>
      ))}
      <Link className="text-[13px]" to={`${base}/catalogue/${seg(name)}`}>Every report of {name}, on its page</Link>
      <span className="flex flex-wrap items-center gap-2">
        {missing.length === 0 && <Button tone="brand" disabled={busy} onClick={() => setPromoting(true)}>Promote…</Button>}
        {rule && actsManual(stage, rule) && <Button disabled={busy} onClick={() => setSkipping(true)}>Skip</Button>}
        <Button tone="plain" onClick={onClose}>Not now</Button>
      </span>
      {missing.length > 0 && <Note>Promote needs {missing.join(' and ')}, which you do not hold.</Note>}
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
 * names its next, or none. The whole order is sent, at the ETag read when the dialog opened (read
 * again when it changed meanwhile); the edges of channels it does not list are kept. The API refuses
 * a cycle, two channels sharing a next, and a channel its own next. */
export function OrderDialog({ open, channels, onClose, onSaved }: { open: boolean; channels: string[]; onClose: () => void; onSaved: () => void }) {
  const { client, api } = useTenant()
  const [read, setRead] = useState<{ etag: string; edges: OrderEdge[]; next: Record<string, string> } | null>(null)
  const [reads, setReads] = useState(0)
  const { busy, failure, act, setFailure } = useAction('The order', () => setReads((n) => n + 1))
  useEffect(() => {
    if (!open) return
    setRead(null)
    if (reads === 0) setFailure(null)
    let alive = true
    client.get<{ order: OrderEdge[] }>(`${api}/channel-order`).then(
      (r) => alive && setRead({ etag: r.etag, edges: r.data.order, next: Object.fromEntries(r.data.order.map((e) => [e.channel, e.next])) }),
      (e) => alive && setFailure(e))
    return () => {
      alive = false
    }
  }, [open, api, client, setFailure, reads])
  useEffect(() => {
    if (!open) setReads(0)
  }, [open])
  const save = () => {
    if (!read || channels.length === 0) return
    const order = orderBody(read.edges, channels, read.next)
    act(() => client.put(`${api}/channel-order`, { body: { order }, ifMatch: read.etag })).then((ok) => {
      if (ok) {
        onClose()
        onSaved()
      }
    })
  }
  return (
    <Confirm open={open} title="The channels' order" confirm="Save" disabled={busy || !read || channels.length === 0} onConfirm={save} onClose={onClose}>
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
