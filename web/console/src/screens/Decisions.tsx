import { useState } from 'react'
import { Link } from 'react-router-dom'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Loading, Mono, PageTitle, Table, when } from '../components/ui'
import { useAction } from '../lib/action'
import { useLoad, useTenant } from '../lib/context'
import { useHome } from '../lib/home'
import { seg } from '../lib/http'
import { type Decision, type HomeReads, type Item, decisionBody, groups, itemKey, promoteFrom } from '../lib/queue'
import { type CatalogueBuild, PlaceDialog, RuleDialog, SourceLicence, compareVersions, entryPath, modeWords, placeBody, purgeable } from './Catalogue'
import { promoteBody } from './Promote'
import { Reports } from './Reports'

// Needs a decision (spec 0041): what kista leaves to a tenant administrator, computed from the home's
// reads, less what the decisions kept in kista cover. Skip and Dismiss write a decision, which every
// administrator then sees; Undo removes the ones this page wrote.

interface EntryVersions {
  versions: { version: string; builds: CatalogueBuild[] }[]
}

/** A decision this page wrote: what Undo removes, at its ETag. */
interface Written {
  id: string
  etag: string
  what: string
}

/** What an item is about, in words. */
function itemWords(i: Item) {
  switch (i.group) {
    case 'manual':
      return <><Mono>{i.waiting}</Mono> waits in <Mono>{i.channel}</Mono>{i.current ? <> (it serves <Mono>{i.current}</Mono>)</> : ''} under a {modeWords(i.mode)} rule</>
    case 'holds':
      return <>{i.version ? <><Mono>{i.version}</Mono> </> : ''}held until {when(i.until)}{i.channel ? <> for <Mono>{i.channel}</Mono></> : ''}</>
    case 'skipped':
      return <><Mono>{i.channel}</Mono>'s follow skips it: {i.reason}</>
    case 'nochannel':
      return i.taking.length === 0 ? <>no channel takes it</> : <>only <Mono>{i.taking.join(', ')}</Mono> take{i.taking.length === 1 ? 's' : ''} it</>
    case 'failed':
      return <>in <Mono>{i.channel}</Mono>: {i.outcome}</>
  }
}

const itemChannel = (i: Item) => ('channel' in i ? i.channel ?? '' : '')

export function Decisions() {
  const { client, api } = useTenant()
  const home = useHome()
  const [channel, setChannel] = useState('')
  const [written, setWritten] = useState<Written[]>([])
  const [telling, setTelling] = useState(false)
  const [open, setOpen] = useState('')
  const { busy, failure, act } = useAction('A decision', home?.reload)
  if (!home) return <Empty>The decision queue is the tenant administrators'.</Empty>
  const reads = home.reads
  // a channel's items; the tenant's (no channel) when no channel is chosen
  const items = home.items.filter((i) => (channel ? itemChannel(i) === channel : true))
  const decide = (i: Item) => {
    const body = decisionBody(i)
    if (!body) return
    act(async () => {
      const r = await client.put<Decision>(`${api}/decisions`, { body })
      const d = r.data
      setWritten((w) => [...w.filter((x) => x.id !== d.id), { id: d.id, etag: d.etag, what: `${d.kind} ${d.name}` }])
    }).then(() => home.reload())
  }
  const undo = () => {
    act(async () => {
      for (const w of [...written].reverse()) await client.del(`${api}/decisions/${seg(w.id)}`, { ifMatch: w.etag })
      setWritten([])
    }).then(() => home.reload())
  }
  return (
    <>
      <PageTitle title="Needs a decision">
        <span className="pb-1 text-ink-muted">What waits for an administrator: the same queue for every administrator of the tenant.</span>
      </PageTitle>
      <div role="search" className="flex flex-wrap items-center gap-2">
        <select aria-label="channel" className="field py-1 text-[13px]" value={channel} onChange={(e) => setChannel(e.target.value)}>
          <option value="">Every channel</option>
          {(reads?.channels ?? []).map((c) => <option key={c.name}>{c.name}</option>)}
        </select>
        <span className="ml-auto flex gap-2">
          {written.length > 0 && <Button disabled={busy} onClick={undo}>Undo ({written.length})</Button>}
          <Button onClick={home.reload}>Refresh</Button>
        </span>
      </div>
      {failure != null && <ErrorBox error={failure} />}
      {home.error ? <ErrorBox error={home.error} /> : !reads ? <Loading /> : items.length === 0 ? <Empty>Nothing waits for a decision.</Empty> : (
        groups.map(([g, title]) => {
          const rows = items.filter((i) => i.group === g)
          if (rows.length === 0) return null
          return (
            <Card key={g} title={<span className="flex items-center gap-2">{title} <Badge>{String(rows.length)}</Badge></span>}
              actions={g === 'manual' ? <Button tone="plain" onClick={() => setTelling(true)}>Tell me of these…</Button> : undefined}>
              <Table rows={rows} rowKey={itemKey} columns={[
                ['Extension', (i) => <span className="flex flex-col"><Mono>{i.name}</Mono><span className="text-[12px] text-ink-muted"><Mono>{i.source}</Mono></span></span>],
                ['What', (i) => <span className="text-[13px]">{itemWords(i)}</span>],
                ['', (i) => (
                  <span role="group" aria-label={`${i.name} ${itemChannel(i)} actions`.replace(/ +/g, ' ')} className="flex flex-wrap gap-1.5">
                    <ItemActions item={i} reads={reads} busy={busy} onDecide={() => decide(i)} onChanged={home.reload} />
                    <Button tone="plain" onClick={() => setOpen(open === itemKey(i) ? '' : itemKey(i))}>{open === itemKey(i) ? 'Close' : 'Report & licence'}</Button>
                  </span>
                )],
              ]} />
              {rows.some((i) => itemKey(i) === open) && <Drawer item={rows.find((i) => itemKey(i) === open)!} />}
            </Card>
          )
        })
      )}
      <Confirm open={telling} title="Telling of new versions" confirm="OK" onConfirm={() => setTelling(false)} onClose={() => setTelling(false)}>
        <p>When a version starts waiting under a manual rule, kista writes the event <Mono>source.waiting</Mono> once, naming the channel, the
          extension, its source and the version. kista sends it to no one itself: the tenant's administrators and auditors read it among the
          events, a channel's administrators read their channel's, and the operator's event sinks carry it to whatever alerts people.</p>
        <p>Here, the count beside <b>Needs a decision</b> is the signal.</p>
      </Confirm>
    </>
  )
}

/** An item's actions, each offered only to who may take it (the API decides anyway). */
function ItemActions({ item: i, reads, busy, onDecide, onChanged }: { item: Item; reads: HomeReads; busy: boolean; onDecide: () => void; onChanged: () => void }) {
  const { client, api, base, perms } = useTenant()
  const [ruling, setRuling] = useState<{ mode?: string } | null>(null)
  const [placing, setPlacing] = useState(false)
  const [purging, setPurging] = useState(false)
  const { busy: changing, failure, act } = useAction('The rule', onChanged)
  const entry = reads.entries.find((e) => e.source === i.source && e.name === i.name)
  const signed = reads.channels.map((c) => c.name)
  const setMode = (mode: string) => act(() => client.put(`${api}/channels/${seg(itemChannel(i))}/sources/${seg(i.name)}`, { body: { source: i.source, mode } }))
    .then((ok) => ok && onChanged())
  const placeTo = itemChannel(i) ? [itemChannel(i)].filter((c) => perms.mayPromote(c, i.name)) : signed.filter((c) => perms.mayPromote(c, i.name))
  const held = i.group === 'holds' ? (i.version ?? entry?.held?.[0]) : undefined
  return (
    <>
      {i.group === 'manual' && <Take item={i} />}
      {(i.group === 'manual' || i.group === 'skipped' || i.group === 'nochannel') && <Button disabled={busy} onClick={onDecide}>{i.group === 'manual' ? 'Skip' : 'Dismiss'}</Button>}
      {i.group === 'manual' && i.source !== 'tenant' && perms.channelAdmin(i.channel) && <Button disabled={changing} onClick={() => setMode('auto')}>Make auto</Button>}
      {i.group === 'failed' && i.mode !== 'manual' && perms.channelAdmin(i.channel) && <Button disabled={changing} onClick={() => setMode('manual')}>Make manual</Button>}
      {i.group === 'failed' && /block/i.test(i.outcome) && <Link className="text-[13px]" to={`${base}/blocks`}>Open the block</Link>}
      {(i.group === 'holds' || i.group === 'nochannel') && placeTo.length > 0 && (
        <Button disabled={busy} onClick={() => setPlacing(true)}>{i.group === 'holds' ? 'Place early…' : 'Place…'}</Button>
      )}
      {(i.group === 'holds' || i.group === 'nochannel') && perms.tenantAdmin && <Button tone="danger-outline" disabled={busy} onClick={() => setPurging(true)}>Purge…</Button>}
      {(i.group === 'skipped' || i.group === 'nochannel') && <Button disabled={busy} onClick={() => setRuling({})}>Set rule…</Button>}
      {failure != null && <ErrorBox error={failure} />}
      <RuleDialog open={ruling !== null} channel={itemChannel(i)} channels={signed.filter((c) => !entry?.rules[c] && perms.channelAdmin(c))} name={i.name}
        source={i.source} onClose={() => setRuling(null)} onSaved={onChanged} />
      <PlaceDialog open={placing} name={i.name} source={i.source} suggest={held ?? entry?.versions[0]} channel={placeTo.length === 1 ? placeTo[0] : undefined}
        channels={placeTo} held={i.group === 'holds' ? true : 'unknown'} onClose={() => setPlacing(false)}
        onPlace={(p) => {
          setPlacing(false)
          act(() => client.post(`${api}/channels/${seg(p.channel)}/extensions/${seg(i.name)}/releases/place`, { body: placeBody(i.source, p) })).then((ok) => ok && onChanged())
        }} />
      {purging && <PurgeDialog source={i.source} name={i.name} version={i.group === 'holds' ? held : undefined} onClose={() => setPurging(false)} onDone={onChanged} />}
    </>
  )
}

/** Taking a waiting version: Promote from a channel that holds it (an active or deprecated release),
 * else Place from the catalogue; neither without promote on the name there. */
function Take({ item: i }: { item: Extract<Item, { group: 'manual' }> }) {
  const { client, api, perms } = useTenant()
  const home = useHome()
  const path = entryPath(api, i.source, i.name)
  const may = perms.mayPromote(i.channel, i.name)
  const versions = useLoad(() => (may ? client.get<EntryVersions>(path).then((r) => r.data) : Promise.resolve({ versions: [] })), [path, may])
  const [open, setOpen] = useState<'promote' | 'place' | ''>('')
  const { busy, failure, act } = useAction('The placement', home?.reload)
  if (!may || !versions.data) return null
  const from = promoteFrom(versions.data.versions, i.waiting, i.channel)
  const promote = from !== '' && perms.mayPublish(from, i.name)
  const done = (ok: boolean) => ok && home?.reload()
  return (
    <>
      {promote ? <Button disabled={busy} onClick={() => setOpen('promote')}>Promote…</Button> : <Button disabled={busy} onClick={() => setOpen('place')}>Place…</Button>}
      {failure != null && <ErrorBox error={failure} />}
      <Confirm open={open === 'promote'} title={`Promote ${i.name} ${i.waiting} from ${from} to ${i.channel}?`} confirm="Promote" onClose={() => setOpen('')}
        onConfirm={() => {
          setOpen('')
          act(() => client.post(`${api}/channels/${seg(i.channel)}/extensions/${seg(i.name)}/releases/promote`, {
            body: promoteBody(from, { target: i.channel, version: i.waiting, narrow: false, notCurrent: false }),
          })).then(done)
        }}>
        <p>Every active release of the version that <Mono>{from}</Mono> serves is released in <Mono>{i.channel}</Mono>, signed with its key: all or
          none.</p>
      </Confirm>
      <PlaceDialog open={open === 'place'} name={i.name} source={i.source} version={i.waiting} channel={i.channel} held={false} onClose={() => setOpen('')}
        onPlace={(p) => {
          setOpen('')
          act(() => client.post(`${api}/channels/${seg(p.channel)}/extensions/${seg(i.name)}/releases/place`, { body: placeBody(i.source, p) })).then(done)
        }} />
    </>
  )
}

/** Purging an entry's builds placed nowhere: of one version, or of every version. */
function PurgeDialog({ source, name, version, onClose, onDone }: { source: string; name: string; version?: string; onClose: () => void; onDone: () => void }) {
  const { client, api } = useTenant()
  const versions = useLoad(() => client.get<EntryVersions>(entryPath(api, source, name)).then((r) => r.data), [api, source, name])
  const { busy, failure, act } = useAction('The entry', onDone)
  const builds = (versions.data?.versions ?? []).filter((v) => !version || v.version === version)
    .flatMap((v) => v.builds.filter(purgeable).map((b) => ({ b, version: v.version })))
    .sort((x, y) => compareVersions(y.version, x.version))
  return (
    <Confirm open title={`Purge ${name}${version ? ` ${version}` : ''}?`} confirm="Purge" danger disabled={busy || builds.length === 0} onClose={onClose}
      onConfirm={() => {
        act(async () => {
          for (const { b } of builds) await client.del(`${api}/catalogue/builds/${seg(b.id)}`)
        }).then((ok) => {
          if (ok) onClose()
          onDone()
        })
      }}>
      {versions.error != null && <ErrorBox error={versions.error} />}
      {!versions.data ? <Loading /> : builds.length === 0 ? <p>No build of it from <Mono>{source}</Mono> is placed nowhere: nothing to purge.</p> : (
        <>
          <p>These builds from <Mono>{source}</Mono> were never released, so no one could have installed them: they are deleted, and their slots are free
            again.</p>
          <ul className="m-0 pl-5 text-[13px]">{builds.map(({ b, version: v }) => <li key={b.id}><Mono>{v} {b.platform} {b.slot}</Mono></li>)}</ul>
        </>
      )}
      {failure != null && <ErrorBox error={failure} />}
    </Confirm>
  )
}

/** The Report & licence drawer: the entry's licence and the name's reports. */
function Drawer({ item: i }: { item: Item }) {
  return (
    <div className="mt-4 flex flex-col gap-3 rounded-md border border-line p-4" aria-label={`${i.name} report and licence`} role="region">
      <SourceLicence source={i.source} name={i.name} />
      <Reports ext={i.name} />
    </div>
  )
}
