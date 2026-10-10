import { useMemo, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Badge, Button, Card, Empty, ErrorBox, Loading, Mono, Table, when } from '../components/ui'
import { useLoad, useTenant } from '../lib/context'
import { seg } from '../lib/http'
import { Keys } from './Keys'
import type { Release } from './types'

const states = ['', 'active', 'deprecated', 'yanked'] as const

export function Channel() {
  const { c = '' } = useParams()
  const { perms } = useTenant()
  const [tab, setTab] = useState<'releases' | 'keys'>('releases')
  return (
    <div className="space-y-4">
      <div className="flex items-center gap-3">
        <h1 className="text-2xl font-bold">{c}</h1>
        {perms.channelAdmin(c) && (
          <div className="ml-auto flex gap-2">
            <Button tone={tab === 'releases' ? 'brand' : 'plain'} onClick={() => setTab('releases')}>Releases</Button>
            <Button tone={tab === 'keys' ? 'brand' : 'plain'} onClick={() => setTab('keys')}>Keys</Button>
          </div>
        )}
      </div>
      {tab === 'keys' ? <Keys channel={c} /> : <Releases channel={c} />}
    </div>
  )
}

/** A channel's releases: the whole channel for its administrators, else the extensions one
 * administers (each through its own route). Name and platform are filtered over the loaded pages. */
function Releases({ channel }: { channel: string }) {
  const { client, api, perms, base } = useTenant()
  const [state, setState] = useState<(typeof states)[number]>('')
  const [name, setName] = useState('')
  const [platform, setPlatform] = useState('')
  const whole = perms.channelAdmin(channel)
  const exts = perms.extensionsIn(channel)
  const { data, error, loading, reload } = useLoad(async () => {
    const base = `${api}/channels/${seg(channel)}`
    const paths = whole ? [`${base}/releases`] : exts.map((x) => `${base}/extensions/${seg(x)}/releases`)
    const out: Release[] = []
    let more = false
    for (const p of paths) {
      let cursor = ''
      for (let i = 0; ; i++) {
        if (i === 20) {
          more = true // 10,000 releases a list: the rest are not loaded
          break
        }
        const q = new URLSearchParams({ limit: '500' })
        if (state) q.set('state', state)
        if (cursor) q.set('cursor', cursor)
        const r = await client.get<{ releases: Release[]; next?: string }>(`${p}?${q}`)
        out.push(...r.data.releases)
        if (!r.data.next) break
        cursor = r.data.next
      }
    }
    return { rows: out.sort((a, b) => b.created_at.localeCompare(a.created_at)), more }
  }, [api, channel, state, whole, exts.join(',')])
  const rows = useMemo(
    () => (data?.rows ?? []).filter((r) => (!name || r.name.includes(name)) && (!platform || r.platform.includes(platform))),
    [data, name, platform],
  )
  return (
    <Card
      title="Releases"
      actions={<Button onClick={reload}>Refresh</Button>}
    >
      <div className="mb-4 flex flex-wrap gap-2 text-sm">
        <select aria-label="state" className="rounded-full border border-line bg-surface px-3 py-1" value={state} onChange={(e) => setState(e.target.value as typeof state)}>
          {states.map((s) => (
            <option key={s} value={s}>{s || 'any state'}</option>
          ))}
        </select>
        <input aria-label="name" placeholder="name" className="rounded-full border border-line bg-surface px-3 py-1" value={name} onChange={(e) => setName(e.target.value)} />
        <input aria-label="platform" placeholder="platform" className="rounded-full border border-line bg-surface px-3 py-1" value={platform} onChange={(e) => setPlatform(e.target.value)} />
      </div>
      {data?.more && <p className="mb-3 text-sm text-warning">Only the oldest 10,000 releases of a list are loaded: narrow it by state.</p>}
      {loading ? <Loading /> : error ? <ErrorBox error={error} /> : rows.length === 0 ? <Empty>No releases.</Empty> : (
        <Table
          rows={rows}
          rowKey={(r) => r.id}
          columns={[
            ['Extension', (r) => <Link className="font-semibold text-brand-strong hover:underline" to={`${base}/channels/${seg(channel)}/releases/${seg(r.name)}/${seg(r.id)}`}>{r.name}</Link>],
            ['Version', (r) => r.version],
            ['Platform', (r) => <Mono>{r.platform}</Mono>],
            ['Build', (r) => <Mono>{r.abi} {r.build_duckdb_version ?? r.build_c_api ?? ''}</Mono>],
            ['State', (r) => <Badge>{r.state}</Badge>],
            ['Visibility', (r) => <Badge>{r.visibility}</Badge>],
            ['Current-eligible', (r) => (r.seq > 0 ? `#${r.seq}` : '')],
            ['Created', (r) => when(r.created_at)],
          ]}
        />
      )}
    </Card>
  )
}
