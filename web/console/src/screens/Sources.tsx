import { useState } from 'react'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, SearchBox, Table, when } from '../components/ui'
import { useAction } from '../lib/action'
import { useLoad, useTenant } from '../lib/context'
import { seg, sourceSeg } from '../lib/http'
import { PlaceDialog, compareVersions, modeWords, placeBody, removedSource } from './Catalogue'

// A channel's rules (specs 0031, 0033): the channel serves each extension name from one source (an
// upstream, a publisher, the tenant), and takes that source's versions from the tenant's catalogue
// by the rule's mode: auto, each new version once its holds have passed; manual, the versions placed
// by hand; follow, a rule a follow of the whole source wrote. A rule an arrival wrote has no mode:
// the channel serves what lands in it. A signed channel may follow whole sources and hold its
// automatic placements; a passthrough channel's rules take no mode.

export interface SourceRow {
  name: string
  source: string
  set_by?: string
  set_at?: string
  available: Record<string, number>
  mode?: string
  current?: string
  newest?: string
  outcome?: string
  outcome_at?: string
}

interface FollowRow {
  source: string
  set_by: string
  set_at: string
  skips: Record<string, string>
}

/** The sources a switch may name: those with releases of the name here, the current one, the tenant. */
export function sourceChoices(r: SourceRow): string[] {
  const all = new Set([...Object.keys(r.available), 'tenant'])
  if (r.source) all.add(r.source)
  return [...all].filter((s) => !s.endsWith('(removed)')).sort()
}

/** Whether the catalogue has a version of the rule's source newer than the channel holds (by the
 * server's version order: the one an auto rule would place next). */
export function newer(r: Pick<SourceRow, 'current' | 'newest'>) {
  return !!r.newest && (!r.current || compareVersions(r.newest, r.current) > 0)
}

/** The source a Change… starts from: the rule's, unless a removed one's (not a source to name). */
export function changeTo(r: SourceRow) {
  return r.source && !removedSource(r.source) ? r.source : (sourceChoices(r).find((s) => s !== 'tenant') ?? 'tenant')
}

/** What a change without a mode leaves the rule with, as the server decides it (SetSource): a new rule
 * has none, a follow's rule moved to another source and an auto rule moved to the tenant become
 * manual, any other keeps its mode. */
export function keptMode(r: Pick<SourceRow, 'source' | 'mode'>, to: string) {
  if (!r.source) return 'none (as it lands)'
  if (to === 'tenant' && r.mode === 'auto') return 'manual (the tenant is never automatic)'
  if (r.mode === 'follow' && to !== r.source) return "manual (a follow's rule moved to another source)"
  return `keep it (${modeWords(r.mode)})`
}

/** A hold as the API answers it (a Go duration) in words: "24h0m0s" is 24h, "0s" none. */
export function holdWords(h?: string) {
  if (!h || /^0[a-z]*$/.test(h)) return 'none'
  return h.replace(/([hm])0s$/, '$1').replace(/h0m$/, 'h')
}

/** A hold as typed, for the API: days ("2d", as the CLI takes them) as hours, the rest as is. */
export function holdInput(s: string) {
  const t = s.trim()
  const d = /^(\d+)d$/.exec(t)
  return d ? `${Number(d[1]) * 24}h` : t
}

export function Sources({ channel, kind }: { channel: string; kind?: string }) {
  const { client, api, perms } = useTenant()
  const signed = kind === 'signed'
  const [prefix, setPrefix] = useState('')
  const [changing, setChanging] = useState<SourceRow | null>(null)
  const [to, setTo] = useState('')
  const [mode, setMode] = useState('')
  const [clearing, setClearing] = useState('')
  const [placing, setPlacing] = useState<SourceRow | null>(null)
  const path = `${api}/channels/${seg(channel)}/sources`
  const list = useLoad(() => client.get<{ sources: SourceRow[] }>(path), [path])
  const { busy, failure, act } = useAction('The rule', list.reload)
  // the API answers every name of the channel at once (no pages): the prefix filters them here
  const all = list.data?.data.sources ?? []
  const rows = all.filter((r) => r.name.startsWith(prefix))
  const done = (ok: boolean) => {
    if (ok) list.reload()
  }
  const doChange = () => {
    const r = changing
    setChanging(null)
    if (r && to) act(() => client.put(`${path}/${seg(r.name)}`, { body: mode ? { source: to, mode } : { source: to } })).then(done)
  }
  // the sources the channel's rules and releases name, offered to follow
  const known = [...new Set(all.flatMap((r) => [r.source, ...Object.keys(r.available)]))].filter((s) => s && s !== 'tenant' && !s.endsWith('(removed)')).sort()
  return (
    <>
      <Note>
        The channel serves each extension from one source: an upstream, a publisher, or the tenant. Its rule takes that source's versions
        from the catalogue: <b>auto</b> each new version once its holds have passed, <b>manual</b> the versions placed by hand (and their later
        builds), <b>follow</b> as auto while the channel follows the source. A rule without a mode was written by the name's first release: the
        channel serves what lands in it. Other sources' releases stay stored and unserved; switching serves the new source's at once.
      </Note>
      {signed && <ChannelHold channel={channel} />}
      <div role="search" className="flex flex-wrap items-center gap-2">
        <SearchBox label="Find an extension's source" value={prefix} onChange={setPrefix} />
        <span className="ml-auto"><Button onClick={list.reload}>Refresh</Button></span>
      </div>
      {failure != null && <ErrorBox error={failure} />}
      {list.error ? <ErrorBox error={list.error} /> : !list.data ? <Loading /> : rows.length === 0 ? (
        <Empty>{prefix ? `No extension starts with “${prefix}”.` : 'No extensions in this channel yet.'}</Empty>
      ) : (
        <Table
          rows={rows}
          rowKey={(r) => r.name}
          columns={[
            ['Extension', (r) => <Mono>{r.name}</Mono>],
            ['Source', (r) => (r.source ? <span className="whitespace-nowrap"><Mono>{r.source}</Mono></span> : <span className="text-ink-muted">none: nothing is served</span>)],
            ...(signed ? [['Mode', (r: SourceRow) => (r.source ? <Badge>{modeWords(r.mode)}</Badge> : '')] as [string, (r: SourceRow) => React.ReactNode]] : []),
            ['Versions', (r) => (
              <span className="flex flex-wrap items-center gap-1">
                {r.current ? <Mono>{r.current}</Mono> : r.source && <span className="text-ink-muted">none placed</span>}
                {newer(r) && <span className="chip bg-warning-soft text-warning" title="the newest available in the catalogue">newest&nbsp;<Mono>{r.newest}</Mono></span>}
              </span>
            )],
            ['Last outcome', (r) => (r.outcome ? <span className="text-[13px]">{r.outcome} <span className="text-ink-muted">{when(r.outcome_at)}</span></span> : '')],
            ['Releases by source', (r) => (
              <span className="flex flex-wrap gap-1">
                {Object.entries(r.available).sort().map(([s, n]) => (
                  <span key={s} className={`chip ${s === r.source ? 'bg-success-soft text-ink' : 'bg-surface-soft text-ink-muted'}`}>
                    <Mono>{s}</Mono>&nbsp;{n}
                  </span>
                ))}
              </span>
            )],
            ['Set', (r) => (r.source ? (
              <span className="flex max-w-[16rem] flex-col text-[13px]">
                <span>{when(r.set_at)}</span>
                <span className="text-ink-muted">by <Mono>{r.set_by}</Mono></span>
              </span>
            ) : '')],
            ['', (r) => (
              <span role="group" aria-label={`${r.name} source actions`} className="flex gap-1.5">
                <Button disabled={busy} onClick={() => { setChanging(r); setMode(''); setTo(changeTo(r)) }}>Change…</Button>
                {signed && r.source && perms.mayPromote(channel, r.name) && <Button disabled={busy} onClick={() => setPlacing(r)}>Place…</Button>}
                {r.source && <Button disabled={busy} onClick={() => setClearing(r.name)}>Clear…</Button>}
              </span>
            )],
          ]}
        />
      )}
      {signed && <Follows channel={channel} known={known} onChanged={list.reload} />}
      <Confirm open={changing !== null} title={`The rule for ${changing?.name ?? ''}`} confirm="Save" disabled={busy || !to}
        onConfirm={doChange}
        onClose={() => setChanging(null)}>
        <p>Only the chosen source's releases of <Mono>{changing?.name}</Mono> are served in <Mono>{channel}</Mono>. A source of the
          tenant without releases here yet is named as <Mono>upstream:&lt;name&gt;</Mono> or <Mono>publisher:&lt;name&gt;</Mono>.</p>
        <Field label="Source">
          <input className="field font-mono" list="source-choices" value={to} onChange={(e) => { const t = e.target.value.trim(); setTo(t); if (t === 'tenant' && mode === 'auto') setMode('') }} data-autofocus
            onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); doChange() } }} />
        </Field>
        <datalist id="source-choices">
          {changing && sourceChoices(changing).map((s) => <option key={s} value={s} />)}
        </datalist>
        {signed && (
          <Field label="Mode" hint="the tenant's own source is never automatic: it is everyone who may publish in the tenant">
            <select className="field" value={mode} onChange={(e) => setMode(e.target.value)}>
              <option value="">{changing ? keptMode(changing, to) : ''}</option>
              <option value="auto" disabled={to === 'tenant'}>auto</option>
              <option value="manual">manual</option>
            </select>
          </Field>
        )}
      </Confirm>
      <Confirm open={clearing !== ''} title={`Clear the source of ${clearing}?`} confirm="Clear" danger
        onConfirm={() => {
          const n = clearing
          setClearing('')
          act(() => client.del(`${path}/${seg(n)}`)).then(done)
        }}
        onClose={() => setClearing('')}>
        <p>Nothing serves <Mono>{clearing}</Mono> in <Mono>{channel}</Mono> until its next release lands, which sets the source
          again, whoever brings it, or a follow takes it. To serve a given source, change the rule instead.</p>
      </Confirm>
      <PlaceDialog open={placing !== null} name={placing?.name ?? ''} source={placing?.source ?? ''} suggest={placing?.newest ?? placing?.current}
        channel={channel} held="unknown" onClose={() => setPlacing(null)}
        onPlace={(p) => {
          const r = placing!
          setPlacing(null)
          act(() => client.post(`${api}/channels/${seg(channel)}/extensions/${seg(r.name)}/releases/place`, { body: placeBody(r.source, p) })).then(done)
        }} />
    </>
  )
}

/** A channel's hold (spec 0033): its automatic placements wait until a build's arrival plus it. */
function ChannelHold({ channel }: { channel: string }) {
  const { client, api } = useTenant()
  const path = `${api}/channels/${seg(channel)}/hold`
  const hold = useLoad(() => client.get<{ hold: string }>(path), [path])
  const [value, setValue] = useState('')
  const { busy, failure, act } = useAction('The hold', hold.reload)
  return (
    <div className="flex flex-col gap-2">
      <form role="group" aria-label="channel hold" className="flex flex-wrap items-center gap-2 text-[13px]"
        onSubmit={(e) => {
          e.preventDefault()
          act(() => client.put(path, { body: { hold: holdInput(value) } })).then((ok) => {
            if (ok) {
              setValue('')
              hold.reload()
            }
          })
        }}>
        <span className="text-ink-muted">Channel hold</span>
        <b>{hold.data ? holdWords(hold.data.data.hold) : '…'}</b>
        <span className="text-ink-muted">(automatic placements wait this long after a build arrives, besides its source's hold)</span>
        <input aria-label="new hold" className="field w-32 py-1 font-mono text-[13px]" placeholder="24h, 2d, 0" required value={value} onChange={(e) => setValue(e.target.value)} />
        <Button type="submit" disabled={busy}>Set hold</Button>
      </form>
      {(failure != null || hold.error != null) && <ErrorBox error={failure ?? hold.error} />}
    </div>
  )
}

/** A channel's follows (spec 0033): each a whole source taken automatically, the names it skips
 * listed with the reason at the placement pass's look (a new build of the source, or hourly). */
function Follows({ channel, known, onChanged }: { channel: string; known: string[]; onChanged: () => void }) {
  const { client, api } = useTenant()
  const path = `${api}/channels/${seg(channel)}/follows`
  const list = useLoad(() => client.get<{ follows: FollowRow[] }>(path), [path])
  const [source, setSource] = useState('')
  const [removing, setRemoving] = useState('')
  const reload = () => {
    list.reload()
    onChanged()
  }
  const { busy, failure, act } = useAction('The follow', reload)
  const follows = list.data?.data.follows ?? []
  return (
    <Card title="Follows">
      <div className="flex flex-col gap-4">
        <Note>A follow takes every name of a source the channel has no rule for, each new version once its holds have passed. It skips a name
          the tenant uses from another source, a reserved one, and one two follows offer; a rule naming the source takes such a name.</Note>
        {failure != null && <ErrorBox error={failure} />}
        {list.error ? <ErrorBox error={list.error} /> : !list.data ? <Loading /> : follows.length === 0 ? <Empty>Follows no source.</Empty> : (
          <Table rows={follows} rowKey={(f) => f.source} columns={[
            ['Source', (f) => <Mono>{f.source}</Mono>],
            ['Since', (f) => <span className="text-[13px] text-ink-muted">{when(f.set_at)} by <Mono>{f.set_by}</Mono></span>],
            ['Skipped', (f) => {
              const names = Object.keys(f.skips).sort()
              return names.length === 0 ? <span className="text-ink-muted">nothing (as of its last look)</span> : (
                <ul className="m-0 list-none p-0 text-[13px]">
                  {names.map((n) => <li key={n}><Mono>{n}</Mono>: {f.skips[n]}</li>)}
                </ul>
              )
            }],
            ['', (f) => <Button tone="danger-outline" disabled={busy} onClick={() => setRemoving(f.source)}>Unfollow…</Button>],
          ]} />
        )}
        <form className="flex items-end gap-2" onSubmit={(e) => {
          e.preventDefault()
          const s = source.trim()
          act(() => client.put(`${path}/${sourceSeg(s)}`)).then((ok) => {
            if (ok) {
              setSource('')
              reload()
            }
          })
        }}>
          <Field label="Follow a source" hint="upstream:<name> or publisher:<name>; never the tenant">
            <input className="field font-mono" list="follow-choices" required value={source} onChange={(e) => setSource(e.target.value)} />
          </Field>
          <datalist id="follow-choices">{known.map((s) => <option key={s} value={s} />)}</datalist>
          <Button type="submit" disabled={busy}>Follow</Button>
        </form>
      </div>
      <Confirm open={removing !== ''} title={`Stop following ${removing}?`} confirm="Unfollow" danger
        onConfirm={() => {
          const s = removing
          setRemoving('')
          act(() => client.del(`${path}/${sourceSeg(s)}`)).then((ok) => ok && reload())
        }}
        onClose={() => setRemoving('')}>
        <p>Nothing more is placed by it; what it placed stays, and the rules it wrote keep the versions they hold, as manual rules.</p>
      </Confirm>
    </Card>
  )
}
