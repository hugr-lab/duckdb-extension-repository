import { useState } from 'react'
import { Card, Empty, ErrorBox, Loading, Mono, Table } from '../components/ui'
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

/** Downloads (spec 0010 phase 2a), within what the caller's grants cover: per day over the range,
 * and per extension, version or platform. */
export function Stats() {
  const { client, api } = useTenant()
  const [span, setSpan] = useState(30)
  const [by, setBy] = useState<keyof typeof groups>('extension')
  const { data, error, loading } = useLoad(async () => {
    const to = new Date().toISOString().slice(0, 10)
    const from = new Date(Date.now() - (span - 1) * 86_400_000).toISOString().slice(0, 10)
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
  const select = 'rounded-full border border-line bg-surface px-3 py-1 text-sm'
  return (
    <div className="space-y-4">
      <Card
        title="Downloads"
        actions={
          <select aria-label="range" className={select} value={span} onChange={(e) => setSpan(Number(e.target.value))}>
            <option value={7}>7 days</option>
            <option value={30}>30 days</option>
            <option value={90}>90 days</option>
          </select>
        }
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
