import { useState } from 'react'
import { Card, Chips, Empty, ErrorBox, Loading, Mono, Note, Table } from '../components/ui'
import { useLoad, useTenant } from '../lib/context'

interface Row {
  day?: string
  extension?: string
  version?: string
  platform?: string
  count: number
  authenticated: number
  installers?: number
}

const groups = { extension: 'extension', version: 'extension,version', platform: 'extension,platform' } as const

/** Every day from from to to (YYYY-MM-DD), so days without downloads show as zero. */
export function days(from: string, to: string): string[] {
  const out: string[] = []
  for (let d = new Date(from + 'T00:00:00Z'); d.toISOString().slice(0, 10) <= to; d = new Date(d.getTime() + 86_400_000)) {
    out.push(d.toISOString().slice(0, 10))
  }
  return out
}

const select = 'rounded-full border border-line bg-surface px-3 py-1 text-sm'

/** The range's first and last day, ending today. */
function range(span: number) {
  const to = new Date().toISOString().slice(0, 10)
  return { from: new Date(Date.now() - (span - 1) * 86_400_000).toISOString().slice(0, 10), to }
}

function Span({ span, onChange }: { span: number; onChange: (n: number) => void }) {
  return (
    <select aria-label="range" className={select} value={span} onChange={(e) => onChange(Number(e.target.value))}>
      <option value={7}>7 days</option>
      <option value={30}>30 days</option>
      <option value={90}>90 days</option>
    </select>
  )
}

/** Statistics: downloads, and the usage the extensions' SDK reports (spec 0012). */
export function Stats() {
  const [tab, setTab] = useState<'downloads' | 'usage'>('downloads')
  const [span, setSpan] = useState(30)
  return (
    <div className="space-y-4">
      <Chips label="statistics" value={tab} onChange={setTab} options={[['downloads', 'Downloads'], ['usage', 'Usage']]} />
      {tab === 'downloads' ? <Downloads span={span} setSpan={setSpan} /> : <Usage span={span} setSpan={setSpan} />}
    </div>
  )
}

/** Downloads (spec 0010 phase 2a), within what the caller's grants cover: per day over the range,
 * and per extension, version or platform. */
function Downloads({ span, setSpan }: { span: number; setSpan: (n: number) => void }) {
  const { client, api } = useTenant()
  const [by, setBy] = useState<keyof typeof groups>('extension')
  const { data, error, loading } = useLoad(async () => {
    const { from, to } = range(span)
    const [byDay, rows] = await Promise.all([
      client.get<{ rows: Row[] }>(`${api}/stats/downloads?${new URLSearchParams({ from, to, group: 'day' })}`),
      client.get<{ rows: Row[] }>(`${api}/stats/downloads?${new URLSearchParams({ from, to, group: groups[by] })}`),
    ])
    const counts = new Map(byDay.data.rows.map((r) => [r.day, r.count]))
    return {
      byDay: days(from, to).map((day) => ({ day, count: counts.get(day) ?? 0 })),
      rows: rows.data.rows.sort((a, b) => b.count - a.count),
    }
  }, [api, span, by])
  if (loading && !data) return <Loading />
  if (error) return <ErrorBox error={error} />
  const max = Math.max(1, ...data!.byDay.map((r) => r.count))
  return (
    <div className="space-y-4">
      <Card
        title="Downloads"
        actions={<Span span={span} onChange={setSpan} />}
      >
        {data!.byDay.every((r) => r.count === 0) ? <Empty>No downloads in this range.</Empty> : (
          <div className="flex h-40 items-end gap-1" role="img" aria-label="downloads per day">
            {data!.byDay.map((r) => (
              <div key={r.day} title={`${r.day}: ${r.count}`} className="flex-1 rounded-t bg-brand" style={{ height: `${(r.count / max) * 100}%` }} />
            ))}
          </div>
        )}
      </Card>
      <Card
        title="By"
        actions={
          <select aria-label="group" className={select} value={by} onChange={(e) => setBy(e.target.value as keyof typeof groups)}>
            <option value="extension">extension</option>
            <option value="version">version</option>
            <option value="platform">platform</option>
          </select>
        }
      >
        <Table
          rows={data!.rows}
          rowKey={(r) => `${r.extension}|${r.version}|${r.platform}`}
          columns={[
            ['Extension', (r) => <Mono>{r.extension}</Mono>],
            ...(by === 'version' ? [['Version', (r: Row) => r.version] as [string, (r: Row) => React.ReactNode]] : []),
            ...(by === 'platform' ? [['Platform', (r: Row) => <Mono>{r.platform}</Mono>] as [string, (r: Row) => React.ReactNode]] : []),
            ['Downloads', (r) => r.count],
            ['Authenticated', (r) => r.authenticated],
            ['Installers', (r) => r.installers ?? ''],
          ]}
        />
      </Card>
    </div>
  )
}

// Usage (spec 0012 phases 1b, 1c): what the extensions' SDK reports, per event and counter, with the
// days' users; use without a valid licence and what downstream kistas forwarded shown apart.

export interface UsageRow {
  day?: string
  extension?: string
  version?: string
  status?: string
  event: string
  counter?: string
  value: number
}

export interface UsageUsers {
  day?: string
  extension?: string
  version?: string
  status?: string
  users: number
}

export type UsageBy = 'day' | 'extension' | 'version'

export interface UsageLine {
  key: string
  day?: string
  extension?: string
  version?: string
  event: string
  counter?: string
  licensed: number
  unlicensed: number
  received: number
}

export interface UsersLine {
  key: string
  day?: string
  extension?: string
  version?: string
  users: number
  received: number
}

const usageGroups: Record<UsageBy, string> = { day: 'day,status', extension: 'extension,status', version: 'extension,version,status' }

/** The usage answer's rows as lines per (day | extension | extension and version) and event and
 * counter: a valid licence or its grace counts as licensed, received counts apart, any other status
 * (missing, expired, invalid, …) as without a licence. Users likewise, this kista's and received. */
export function usageLines(rows: UsageRow[], users: UsageUsers[]): { events: UsageLine[]; users: UsersLine[] } {
  const dims = (r: { day?: string; extension?: string; version?: string }) => ({ day: r.day, extension: r.extension, version: r.version })
  const id = (r: { day?: string; extension?: string; version?: string }) => [r.day ?? '', r.extension ?? '', r.version ?? ''].join('|')
  const ev = new Map<string, UsageLine>()
  for (const r of rows) {
    const key = `${id(r)}|${r.event}|${r.counter ?? ''}`
    let l = ev.get(key)
    if (!l) ev.set(key, (l = { key, ...dims(r), event: r.event, counter: r.counter, licensed: 0, unlicensed: 0, received: 0 }))
    if (r.status === 'received') l.received += r.value
    else if (r.status === 'valid' || r.status === 'grace') l.licensed += r.value
    else l.unlicensed += r.value
  }
  const us = new Map<string, UsersLine>()
  for (const u of users) {
    const key = id(u)
    let l = us.get(key)
    if (!l) us.set(key, (l = { key, ...dims(u), users: 0, received: 0 }))
    if (u.status === 'received') l.received += u.users
    else l.users += u.users
  }
  const order = (a: { day?: string; extension?: string; version?: string }, b: typeof a) =>
    (a.day ?? '').localeCompare(b.day ?? '') || (a.extension ?? '').localeCompare(b.extension ?? '') || (a.version ?? '').localeCompare(b.version ?? '')
  return {
    events: [...ev.values()].sort((a, b) => order(a, b) || a.event.localeCompare(b.event) || (a.counter ?? '').localeCompare(b.counter ?? '')),
    users: [...us.values()].sort(order),
  }
}

function Usage({ span, setSpan }: { span: number; setSpan: (n: number) => void }) {
  const { client, api } = useTenant()
  const [by, setBy] = useState<UsageBy>('extension')
  const { data, error, loading } = useLoad(async () => {
    const { from, to } = range(span)
    const q = (group: string) => client.get<{ rows: UsageRow[]; users: UsageUsers[] }>(`${api}/stats/usage?${new URLSearchParams({ from, to, group })}`)
    const [byDay, rows] = await Promise.all([q('day,status'), q(usageGroups[by])])
    const perDay = usageLines(byDay.data.rows, byDay.data.users).users
    const users = new Map(perDay.map((u) => [u.day, u.users + u.received]))
    return { byDay: days(from, to).map((day) => ({ day, users: users.get(day) ?? 0 })), lines: usageLines(rows.data.rows, rows.data.users) }
  }, [api, span, by])
  if (loading && !data) return <Loading />
  if (error) return <ErrorBox error={error} />
  const max = Math.max(1, ...data!.byDay.map((r) => r.users))
  const dimCols: [string, (r: UsageLine | UsersLine) => React.ReactNode][] = by === 'day'
    ? [['Day', (r) => r.day]]
    : [['Extension', (r) => <Mono>{r.extension}</Mono>], ...(by === 'version' ? [['Version', (r: UsageLine | UsersLine) => r.version] as [string, (r: UsageLine | UsersLine) => React.ReactNode]] : [])]
  return (
    <>
      <Card title="Users" actions={<Span span={span} onChange={setSpan} />}>
        {data!.byDay.every((r) => r.users === 0) ? <Empty>No usage reported in this range.</Empty> : (
          <div className="flex h-40 items-end gap-1" role="img" aria-label="users per day">
            {data!.byDay.map((r) => (
              <div key={r.day} title={`${r.day}: ${r.users}`} className="flex-1 rounded-t bg-brand" style={{ height: `${(r.users / max) * 100}%` }} />
            ))}
          </div>
        )}
      </Card>
      <Card
        title="By"
        actions={
          <select aria-label="usage group" className={select} value={by} onChange={(e) => setBy(e.target.value as UsageBy)}>
            <option value="day">day</option>
            <option value="extension">extension</option>
            <option value="version">version</option>
          </select>
        }
      >
        <div className="flex flex-col gap-4">
          <Table rows={data!.lines.users} rowKey={(r) => r.key} columns={[...dimCols, ['Users', (r) => (r as UsersLine).users], ['Received', (r) => (r as UsersLine).received || '']]} />
          {data!.lines.events.length === 0 ? <Empty>No events reported.</Empty> : (
            <Table rows={data!.lines.events} rowKey={(r) => r.key} columns={[
              ...dimCols,
              ['Event', (r) => <Mono>{(r as UsageLine).event}</Mono>],
              ['Counter', (r) => <Mono>{(r as UsageLine).counter ?? ''}</Mono>],
              ['Licensed', (r) => (r as UsageLine).licensed],
              ['Without a licence', (r) => (r as UsageLine).unlicensed ? <span className="text-warning">{(r as UsageLine).unlicensed}</span> : ''],
              ['Received', (r) => (r as UsageLine).received || ''],
            ]} />
          )}
          <Note>Users are the distinct principals that used a version on a day, summed over the days, versions and channels not grouped by. Without a licence: reported with any status but valid or grace. Received: forwarded by downstream kistas, statuses not told apart.</Note>
        </div>
      </Card>
    </>
  )
}
