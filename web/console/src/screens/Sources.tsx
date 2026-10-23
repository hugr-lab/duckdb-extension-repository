import { useState } from 'react'
import { Button, Confirm, Empty, ErrorBox, Loading, Mono, Note, SearchBox, Table, when } from '../components/ui'
import { useAction } from '../lib/action'
import { useLoad, useTenant } from '../lib/context'
import { seg } from '../lib/http'

// A name's source in a channel (spec 0031): the channel serves each extension name from one source
// (an upstream, a publisher, the tenant), set by the name's first release and switched here by the
// channel's administrators. Other sources' releases stay stored and are not served.

export interface SourceRow {
  name: string
  source: string
  set_by?: string
  set_at?: string
  available: Record<string, number>
}

/** The sources a switch may name: those with releases of the name here, the current one, the tenant. */
export function sourceChoices(r: SourceRow): string[] {
  const all = new Set([...Object.keys(r.available), 'tenant'])
  if (r.source) all.add(r.source)
  return [...all].filter((s) => !s.endsWith('(removed)')).sort()
}

export function Sources({ channel }: { channel: string }) {
  const { client, api } = useTenant()
  const [prefix, setPrefix] = useState('')
  const [switching, setSwitching] = useState<SourceRow | null>(null)
  const [to, setTo] = useState('')
  const [clearing, setClearing] = useState('')
  const path = `${api}/channels/${seg(channel)}/sources`
  const list = useLoad(() => client.get<{ sources: SourceRow[] }>(path), [path])
  const { busy, failure, act } = useAction('The source', list.reload)
  // the API answers every name of the channel at once (no pages): the prefix filters them here
  const rows = (list.data?.data.sources ?? []).filter((r) => r.name.startsWith(prefix))
  const done = (ok: boolean) => {
    if (ok) list.reload()
  }
  const doSwitch = () => {
    const r = switching
    setSwitching(null)
    if (r && to) act(() => client.put(`${path}/${seg(r.name)}`, { body: { source: to } })).then(done)
  }
  return (
    <>
      <Note>
        The channel serves each extension from one source: an upstream, a publisher, or the tenant. The first release of a name
        sets it; another source's releases are refused, or stay stored and unserved after a switch. Switching serves the other
        source's releases at once (a mirror fills in at its next run); clearing serves nothing until the next release sets it.
      </Note>
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
            ['Source', (r) => (r.source ? <Mono>{r.source}</Mono> : <span className="text-ink-muted">none: nothing is served</span>)],
            ['Releases by source', (r) => (
              <span className="flex flex-wrap gap-1">
                {Object.entries(r.available).sort().map(([s, n]) => (
                  <span key={s} className={`chip ${s === r.source ? 'bg-success-soft text-ink' : 'bg-surface-soft text-ink-muted'}`}>
                    <Mono>{s}</Mono>&nbsp;{n}
                  </span>
                ))}
              </span>
            )],
            ['Set', (r) => (r.source ? <span>{when(r.set_at)} by <Mono>{r.set_by}</Mono></span> : '')],
            ['', (r) => (
              <span role="group" aria-label={`${r.name} source actions`} className="flex gap-1.5">
                <Button disabled={busy} onClick={() => { setSwitching(r); setTo(sourceChoices(r).find((s) => s !== r.source) ?? 'tenant') }}>Switch…</Button>
                {r.source && <Button disabled={busy} onClick={() => setClearing(r.name)}>Clear…</Button>}
              </span>
            )],
          ]}
        />
      )}
      <Confirm open={switching !== null} title={`The source of ${switching?.name ?? ''}`} confirm="Switch" disabled={busy || !to}
        onConfirm={doSwitch}
        onClose={() => setSwitching(null)}>
        <p>Only the chosen source's releases of <Mono>{switching?.name}</Mono> are served in <Mono>{channel}</Mono>. A source of the
          tenant without releases here yet is named as <Mono>upstream:&lt;name&gt;</Mono> or <Mono>publisher:&lt;name&gt;</Mono>.</p>
        <label className="flex flex-col gap-1 text-[13px]">
          Source
          <input className="field font-mono" list="source-choices" value={to} onChange={(e) => setTo(e.target.value.trim())} data-autofocus
            onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); doSwitch() } }} />
          <datalist id="source-choices">
            {switching && sourceChoices(switching).map((s) => <option key={s} value={s} />)}
          </datalist>
        </label>
      </Confirm>
      <Confirm open={clearing !== ''} title={`Clear the source of ${clearing}?`} confirm="Clear" danger
        onConfirm={() => {
          const n = clearing
          setClearing('')
          act(() => client.del(`${path}/${seg(n)}`)).then(done)
        }}
        onClose={() => setClearing('')}>
        <p>Nothing serves <Mono>{clearing}</Mono> in <Mono>{channel}</Mono> until its next release lands, which sets the source
          again, whoever brings it. To serve a given source, switch to it instead.</p>
      </Confirm>
    </>
  )
}
