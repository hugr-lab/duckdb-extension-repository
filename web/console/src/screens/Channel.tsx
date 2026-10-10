import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Badge, Button, Chips, Empty, ErrorBox, Loading, Mono, PageTitle, Pager, SearchBox, Table, when } from '../components/ui'
import { useLoad, usePaged, useTenant } from '../lib/context'
import { seg } from '../lib/http'
import { Keys } from './Keys'
import type { Release } from './types'

// DuckDB's platforms (spec 0006's grammar names more; these are the ones DuckDB builds).
export const platforms = ['linux_amd64', 'linux_arm64', 'linux_amd64_musl', 'linux_arm64_musl', 'osx_amd64', 'osx_arm64',
  'windows_amd64', 'windows_arm64', 'wasm_eh', 'wasm_mvp', 'wasm_threads']

export const releaseStates: ['' | 'active' | 'deprecated' | 'yanked', string][] = [['', 'All'], ['active', 'Active'], ['deprecated', 'Deprecated'], ['yanked', 'Yanked']]

type Tab = 'extensions' | 'releases' | 'keys'

/** A channel: its extensions (the index, by name, page by page), all its releases, its keys. Its
 * administrators see all of them; an extension's administrators, their extensions. */
export function Channel() {
  const { c = '' } = useParams()
  const { client, api, perms, base } = useTenant()
  const admin = perms.channelAdmin(c)
  const [picked, setTab] = useState<{ c: string; tab: Tab }>({ c, tab: 'extensions' })
  const detail = useLoad(() => client.get<{ kind: string; duckdb_versions?: { duckdb_version: string }[] }>(`${api}/channels/${seg(c)}`), [api, c])
  const versions = detail.data?.data.duckdb_versions?.map((v) => v.duckdb_version) ?? []
  const tabs: [Tab, string][] = admin ? [['extensions', 'Extensions'], ['releases', 'All releases'], ['keys', 'Keys']] : [['extensions', 'Extensions']]
  // a tab chosen in another channel, or one this caller is not offered here, is not kept
  const tab = picked.c === c && tabs.some(([t]) => t === picked.tab) ? picked.tab : 'extensions'
  return (
    <>
      <PageTitle eyebrow={<><Link to={`${base}/channels`} className="no-underline">Channels</Link>{detail.data && ` / ${detail.data.data.kind}`}</>}
        title={<span className="font-mono">{c}</span>}>
        {versions.length > 0 && <span className="pb-1 text-ink-muted">Serves DuckDB {versions.join(', ')}</span>}
      </PageTitle>
      {tabs.length > 1 && <Chips label="channel sections" value={tab} options={tabs} onChange={(t) => setTab({ c, tab: t })} />}
      {tab === 'keys' ? <Keys channel={c} /> : tab === 'releases' ? <Releases channel={c} /> : !admin ? <Mine channel={c} /> :
        detail.error ? <ErrorBox error={detail.error} /> : !detail.data ? <Loading /> : <Extensions channel={c} versions={versions} />}
    </>
  )
}

interface IndexRow {
  name: string
  visibility: string
  current?: string
}

/** The channel's extensions from the index, by name: a prefix searched by the API, and the current
 * version for a chosen DuckDB version and platform. */
function Extensions({ channel, versions }: { channel: string; versions: string[] }) {
  const { client, api, base } = useTenant()
  const [prefix, setPrefix] = useState('')
  const [version, setVersion] = useState('')
  const [platform, setPlatform] = useState('linux_amd64')
  const v = version || versions[versions.length - 1] || ''
  const list = usePaged<IndexRow>(async (cursor, limit) => {
    const q = new URLSearchParams({ limit: String(limit) })
    if (prefix) q.set('prefix', prefix)
    if (cursor) q.set('cursor', cursor)
    if (v) {
      q.set('duckdb_version', v)
      q.set('platform', platform)
    }
    const r = await client.get<{ extensions: IndexRow[]; next?: string }>(`${api}/channels/${seg(channel)}/extensions?${q}`)
    return { items: r.data.extensions, next: r.data.next }
  }, [api, channel, prefix, v, platform])
  return (
    <>
      <div role="search" className="flex flex-wrap items-center gap-2">
        <SearchBox label="Find an extension" value={prefix} onChange={setPrefix} />
        {versions.length > 0 && (
          <label className="flex items-center gap-2 text-[13px] text-ink-muted">
            Current for
            <select aria-label="DuckDB version" className="field py-1 text-[13px]" value={v} onChange={(e) => setVersion(e.target.value)}>
              {versions.map((x) => <option key={x}>{x}</option>)}
            </select>
            <select aria-label="platform" className="field py-1 text-[13px]" value={platform} onChange={(e) => setPlatform(e.target.value)}>
              {platforms.map((x) => <option key={x}>{x}</option>)}
            </select>
          </label>
        )}
        <span className="ml-auto"><Button onClick={list.reload}>Refresh</Button></span>
      </div>
      {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : list.items.length === 0 ? (
        <Empty>{prefix ? `No extension starts with “${prefix}”.` : 'No extensions in this channel yet.'}</Empty>
      ) : (
        <Table
          rows={list.items}
          rowKey={(r) => r.name}
          columns={[
            ['Extension', (r) => <Link className="font-mono font-medium no-underline" to={`${base}/channels/${seg(channel)}/extensions/${seg(r.name)}`}>{r.name}</Link>],
            ['Current', (r) => (r.current ? <Mono>{r.current}</Mono> : <span className="text-ink-muted">none for {platform}</span>)],
            ['Visibility', (r) => <Badge>{r.visibility}</Badge>],
          ]}
        />
      )}
      <Pager page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} />
    </>
  )
}

/** An extension administrator's extensions in the channel (from its grants: a few). */
function Mine({ channel }: { channel: string }) {
  const { perms, base } = useTenant()
  const exts = perms.extensionsIn(channel)
  if (exts.length === 0) return <Empty>You administer no extension in this channel.</Empty>
  return (
    <Table
      rows={exts}
      rowKey={(x) => x}
      columns={[['Extension', (x) => <Link className="font-mono font-medium no-underline" to={`${base}/channels/${seg(channel)}/extensions/${seg(x)}`}>{x}</Link>]]}
    />
  )
}

/** A list of releases, page by page: a channel's (its administrators) or one extension's. */
export function ReleaseList({ channel, ext }: { channel: string; ext?: string }) {
  const { client, api, base } = useTenant()
  const [state, setState] = useState<(typeof releaseStates)[number][0]>('')
  const [prefix, setPrefix] = useState('')
  const path = ext ? `${api}/channels/${seg(channel)}/extensions/${seg(ext)}/releases` : `${api}/channels/${seg(channel)}/releases`
  const list = usePaged<Release>(async (cursor, limit) => {
    const q = new URLSearchParams({ limit: String(limit) })
    if (state) q.set('state', state)
    if (prefix && !ext) q.set('prefix', prefix)
    if (cursor) q.set('cursor', cursor)
    const r = await client.get<{ releases: Release[]; next?: string }>(`${path}?${q}`)
    return { items: r.data.releases, next: r.data.next }
  }, [path, state, prefix])
  return (
    <>
      <div role="search" className="flex flex-wrap items-center gap-2">
        {!ext && <SearchBox label="Find releases by extension" value={prefix} onChange={setPrefix} />}
        <Chips label="state" value={state} options={releaseStates} onChange={setState} />
        <span className="ml-auto"><Button onClick={list.reload}>Refresh</Button></span>
      </div>
      {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : list.items.length === 0 ? <Empty>No releases.</Empty> : (
        <Table
          rows={list.items}
          rowKey={(r) => r.id}
          columns={[
            ['Release', (r) => (
              <Link className="font-mono font-medium no-underline" to={`${base}/channels/${seg(channel)}/releases/${seg(r.name)}/${seg(r.id)}`}>
                {ext ? r.version : `${r.name} ${r.version}`}
              </Link>
            )],
            ['Platform', (r) => <Mono>{r.platform}</Mono>],
            ['Build', (r) => <Mono>{r.abi} {r.build_duckdb_version ?? r.build_c_api ?? ''}</Mono>],
            ['State', (r) => <Badge>{r.state}</Badge>],
            ['Visibility', (r) => <Badge>{r.visibility}</Badge>],
            ['Current-eligible', (r) => (r.seq > 0 ? `#${r.seq}` : '')],
            ['Added', (r) => when(r.created_at)],
          ]}
        />
      )}
      <Pager page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} />
    </>
  )
}

function Releases({ channel }: { channel: string }) {
  return <ReleaseList channel={channel} />
}

/** One extension in a channel: its releases, page by page (oldest first: the API's order, so a
 * cursor survives new releases). */
export function Extension() {
  const { c = '', ext = '' } = useParams()
  const { base } = useTenant()
  return (
    <>
      <PageTitle eyebrow={<><Link to={`${base}/channels`} className="no-underline">Channels</Link> / <Link to={`${base}/channels/${seg(c)}`} className="no-underline">{c}</Link></>}
        title={<span className="font-mono">{ext}</span>} />
      <ReleaseList channel={c} ext={ext} />
    </>
  )
}
