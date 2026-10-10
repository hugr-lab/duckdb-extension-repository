import { useState } from 'react'
import { Badge, Button, Card, Empty, ErrorBox, Loading, Mono, Table, when } from '../components/ui'
import type { Client } from '../lib/http'
import { useLoad } from '../lib/context'
import type { Event } from './types'

/** Events (spec 0010) under an API path ("/tenants/acme/events" or "/events"), with the API's
 * filters; everything is rendered as text. */
export function Events({ client, path, title }: { client: Client; path: string; title: string }) {
  const [filter, setFilter] = useState({ kind: '', actor: '', subject: '', outcome: '' })
  const [applied, setApplied] = useState(filter)
  const [pages, setPages] = useState<string[]>([''])
  const cursor = pages[pages.length - 1]
  const kinds = useLoad(() => client.get<{ kinds: { kind: string }[] }>('/event-kinds', { anonymous: true }), [])
  const { data, error, loading, reload } = useLoad(() => {
    const q = new URLSearchParams({ limit: '50' })
    for (const [k, v] of Object.entries(applied)) if (v) q.set(k, v)
    if (cursor) q.set('cursor', cursor)
    return client.get<{ events: Event[]; next?: string }>(`${path}?${q}`)
  }, [path, applied, cursor])
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
        {input('subject', 'subject prefix')}
        <select aria-label="outcome" className="rounded-full border border-line bg-surface px-3 py-1" value={filter.outcome} onChange={(e) => setFilter({ ...filter, outcome: e.target.value })}>
          <option value="">any outcome</option>
          <option value="ok">ok</option>
          <option value="refused">refused</option>
          <option value="failed">failed</option>
        </select>
        <Button type="submit" tone="brand">Filter</Button>
      </form>
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
