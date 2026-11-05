import { useState } from 'react'
import { Badge, Button, Card, Empty, ErrorBox, Loading, Mono, Table, when } from '../components/ui'
import type { Client } from '../lib/http'
import { useLoad } from '../lib/context'
import type { Event } from './types'

/** The families of the event catalogue's kinds ("release.*"), sorted. */
export function families(kinds: string[]) {
  return [...new Set(kinds.filter((k) => k.includes('.')).map((k) => `${k.slice(0, k.indexOf('.'))}.*`))].sort()
}

/** The kind parameter (spec 0040: a list of kinds and families): the families chosen, and the kind
 * chosen unless one of them takes it in. */
export function kindParam(chosen: string[], kind: string) {
  const covered = kind && chosen.some((f) => kind.startsWith(f.slice(0, -1)))
  return [...chosen, ...(kind && !covered ? [kind] : [])].join(',')
}

/** Events (spec 0010) under an API path ("/tenants/acme/events" or "/events"), with the API's
 * filters, the kinds' families as chips (spec 0041); everything is rendered as text. A fixed subject
 * (a channel's administrators read their channel's events) is sent with every read. */
export function Events({ client, path, title, subject }: { client: Client; path: string; title: string; subject?: string }) {
  const [filter, setFilter] = useState({ kind: '', actor: '', subject: '', outcome: '' })
  const [applied, setApplied] = useState(filter)
  const [chosen, setChosen] = useState<string[]>([])
  const [pages, setPages] = useState<string[]>([''])
  const cursor = pages[pages.length - 1]
  const kinds = useLoad(() => client.get<{ kinds: { kind: string }[] }>('/event-kinds', { anonymous: true }), [])
  const { data, error, loading, reload } = useLoad(() => {
    const q = new URLSearchParams({ limit: '50' })
    for (const [k, v] of Object.entries(applied)) if (v && k !== 'kind') q.set(k, v)
    const kind = kindParam(chosen, applied.kind)
    if (kind) q.set('kind', kind)
    if (subject) q.set('subject', applied.subject.startsWith(`${subject}/`) ? applied.subject : subject)
    if (cursor) q.set('cursor', cursor)
    return client.get<{ events: Event[]; next?: string }>(`${path}?${q}`)
  }, [path, applied, cursor, chosen.join(','), subject])
  const [open, setOpen] = useState<Event | null>(null)
  const input = (k: keyof typeof filter, label: string) => (
    <input aria-label={label} placeholder={label} className="rounded-full border border-line bg-surface px-3 py-1" value={filter[k]}
      onChange={(e) => setFilter({ ...filter, [k]: e.target.value })} />
  )
  return (
    <Card title={title} actions={<Button onClick={reload}>Refresh</Button>}>
      <form
        className="mb-4 flex flex-wrap gap-2 text-sm"
        onSubmit={(e) => {
          e.preventDefault()
          setApplied(filter)
          setPages([''])
        }}
      >
        <select aria-label="kind" className="rounded-full border border-line bg-surface px-3 py-1" value={filter.kind} onChange={(e) => setFilter({ ...filter, kind: e.target.value })}>
          <option value="">any kind</option>
          {(kinds.data?.data.kinds ?? []).map((k) => (
            <option key={k.kind} value={k.kind}>{k.kind}</option>
          ))}
        </select>
        {input('actor', 'actor id')}
        {input('subject', subject ? `subject under ${subject}/` : 'subject prefix')}
        <select aria-label="outcome" className="rounded-full border border-line bg-surface px-3 py-1" value={filter.outcome} onChange={(e) => setFilter({ ...filter, outcome: e.target.value })}>
          <option value="">any outcome</option>
          <option value="ok">ok</option>
          <option value="refused">refused</option>
          <option value="failed">failed</option>
        </select>
        <Button type="submit" tone="brand">Filter</Button>
      </form>
      <div role="group" aria-label="event families" className="mb-4 flex flex-wrap gap-1.5">
        {families((kinds.data?.data.kinds ?? []).map((k) => k.kind)).map((f) => {
          const on = chosen.includes(f)
          return (
            <button key={f} type="button" aria-pressed={on} className={`chip font-mono ${on ? 'bg-brand-strong text-brand-on' : 'border border-line text-ink'}`}
              onClick={() => { setChosen(on ? chosen.filter((x) => x !== f) : [...chosen, f].sort()); setPages(['']) }}>{f}</button>
          )
        })}
      </div>
      {loading ? <Loading /> : error ? <ErrorBox error={error} /> : data!.data.events.length === 0 ? <Empty>No events.</Empty> : (
        <>
          <Table
            rows={data!.data.events}
            rowKey={(e) => e.id}
            columns={[
              ['At', (e) => <button className="text-left text-brand-strong hover:underline" onClick={() => setOpen(e)}>{when(e.at)}</button>],
              ['Kind', (e) => <Mono>{e.kind}</Mono>],
              ['Outcome', (e) => <Badge>{e.outcome}</Badge>],
              ['Actor', (e) => <Mono>{e.actor_name || e.actor}</Mono>],
              ['Subject', (e) => <Mono>{e.subject}</Mono>],
            ]}
          />
          <div className="mt-4 flex justify-end gap-2">
            {pages.length > 1 && <Button onClick={() => setPages(pages.slice(0, -1))}>Newer</Button>}
            {data!.data.next && <Button onClick={() => setPages([...pages, data!.data.next!])}>Older</Button>}
          </div>
        </>
      )}
      {open && (
        <div className="mt-4 rounded-xl border border-line p-4">
          <div className="mb-2 flex items-center justify-between">
            <h3 className="font-bold">{open.kind}</h3>
            <Button onClick={() => setOpen(null)}>Close</Button>
          </div>
          <pre className="overflow-x-auto rounded-xl bg-row p-3 font-mono text-xs">{JSON.stringify(open, null, 2)}</pre>
        </div>
      )}
    </Card>
  )
}
