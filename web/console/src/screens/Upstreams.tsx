import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { Badge, Button, Card, Checks, Chips, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, Pager, SearchBox, Table, when } from '../components/ui'
import { useLoad, usePaged, useTenant } from '../lib/context'
import { ApiError, seg } from '../lib/http'
import { platforms } from './Channel'

// Upstreams (spec 0009; console phase 2b): where a channel's mirrored extensions come from, what is
// allowed in, and what each run did.

interface Entry {
  name: string
  versions: string[]
  allow_reserved?: boolean
}

interface RunResult {
  started: string
  ended: string
  dry_run?: boolean
  error?: string
  counts: Record<string, number>
  problems?: { duckdb_version: string; platform: string; name: string; outcome: string; detail?: string }[]
}

interface Upstream {
  name: string
  kind: string
  prefix?: string
  channel: string
  mode: string
  visibility: string
  state: string
  run: 'idle' | 'requested' | 'dry_run_requested' | 'running'
  keys: string[]
  platforms: string[]
  extensions: Entry[]
  last_run_at?: string
  last_run?: RunResult
  created_at: string
  created_by: string
  credential?: string
  etag: string
}

interface Cell {
  duckdb_version: string
  platform: string
  name: string
  outcome: string
  detail?: string
  body_hash?: string
  fetched_at: string
}

const kinds: [string, string][] = [['duckdb-core', 'DuckDB core'], ['duckdb-community', 'DuckDB community'], ['repository', 'Another repository']]
const outcomes = ['released', 'unchanged', 'yanked', 'conflict', 'missing', 'rejected', 'blocked', 'shadowed', 'failed']

const runWords: Record<Upstream['run'], string> = { idle: '', requested: 'a sync is requested', dry_run_requested: 'a dry run is requested', running: 'running' }

/** A run's counts in a line: released 12 · unchanged 340 · missing 3. */
export function countsLine(r?: RunResult) {
  if (!r) return ''
  return Object.entries(r.counts ?? {}).filter(([, n]) => n > 0).map(([k, n]) => `${k} ${n}`).join(' · ') || 'nothing to do'
}

export function Upstreams() {
  const { client, api, base } = useTenant()
  const [adding, setAdding] = useState(false)
  const list = usePaged<Upstream>(async (cursor, limit) => {
    const q = new URLSearchParams({ limit: String(limit) })
    if (cursor) q.set('cursor', cursor)
    const r = await client.get<{ upstreams: Upstream[]; next?: string }>(`${api}/upstreams?${q}`)
    return { items: r.data.upstreams, next: r.data.next }
  }, [api])
  return (
    <>
      <PageTitle title="Upstreams">
        <span className="pb-1 text-ink-muted">Where mirrored extensions come from. DuckDB's signature is checked at intake; the channel's key signs what it serves.</span>
        <span className="ml-auto"><Button tone="brand" onClick={() => setAdding(!adding)}>{adding ? 'Close' : 'New upstream'}</Button></span>
      </PageTitle>
      {adding && <NewUpstream />}
      {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : list.items.length === 0 ? <Empty>No upstreams: nothing is mirrored.</Empty> : (
        <Table
          rows={list.items}
          rowKey={(u) => u.name}
          columns={[
            ['Upstream', (u) => <Link className="font-mono font-medium no-underline" to={`${base}/upstreams/${seg(u.name)}`}>{u.name}</Link>],
            ['Source', (u) => (u.kind === 'repository' ? <Mono>{u.prefix}</Mono> : kinds.find(([k]) => k === u.kind)?.[1] ?? u.kind)],
            ['Into', (u) => <><Mono>{u.channel}</Mono> <span className="text-[13px] text-ink-muted">{u.mode}</span></>],
            ['State', (u) => <span className="flex gap-1"><Badge>{u.state}</Badge><Badge>{u.visibility}</Badge></span>],
            ['Last run', (u) => (
              <span className="text-[13px]">
                {u.last_run_at ? <>{when(u.last_run_at)}<div className="text-ink-muted">{u.last_run?.error ? 'failed' : countsLine(u.last_run)}</div></> : <span className="text-ink-muted">never</span>}
                {u.run !== 'idle' && <div className="text-warning">{runWords[u.run]}</div>}
              </span>
            )],
          ]}
        />
      )}
      <Pager page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} />
      <Shadows />
    </>
  )
}

/** The names the tenant released over a core name (spec 0009): passthrough channels stop serving
 * DuckDB's build of them until a shadow is removed. */
function Shadows() {
  const { client, api } = useTenant()
  const list = useLoad(() => client.get<{ shadows: { name: string; created_at: string; created_by: string }[] }>(`${api}/shadows`), [api])
  const [removing, setRemoving] = useState('')
  const [failure, setFailure] = useState<unknown>(null)
  const remove = async () => {
    const n = removing
    setRemoving('')
    setFailure(null)
    try {
      await client.del(`${api}/shadows/${seg(n)}`)
    } catch (e) {
      setFailure(e)
    }
    list.reload()
  }
  const rows = list.data?.data.shadows ?? []
  return (
    <Card title="Shadowed core names">
      <div className="flex flex-col gap-3">
        <Note>A core name the tenant released itself: passthrough channels no longer serve DuckDB's build of it. Removing the shadow serves DuckDB's again.</Note>
        {failure != null && <ErrorBox error={failure} />}
        {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : rows.length === 0 ? <Empty>None.</Empty> : (
          <Table rows={rows} rowKey={(x) => x.name} columns={[
            ['Name', (x) => <Mono>{x.name}</Mono>],
            ['Since', (x) => when(x.created_at)],
            ['', (x) => <Button tone="danger-outline" onClick={() => setRemoving(x.name)}>Remove…</Button>],
          ]} />
        )}
      </div>
      <Confirm open={removing !== ''} title={`Serve DuckDB's ${removing} again?`} confirm="Remove the shadow" danger onConfirm={remove} onClose={() => setRemoving('')}>
        <p>Passthrough channels serve DuckDB's own <Mono>{removing}</Mono> again; the tenant's releases of it stay.</p>
      </Confirm>
    </Card>
  )
}

/** The names typed into an allowlist field: separated by commas or spaces; "*" is every name. */
export function entryNames(s: string): string[] {
  return [...new Set(s.split(/[\s,]+/).filter(Boolean))]
}

function NewUpstream() {
  const { client, api, base } = useTenant()
  const nav = useNavigate()
  const channels = useLoad(() => client.get<{ channels: { name: string; kind: string }[] }>(`${api}/channels`), [api])
  const [f, setF] = useState({ name: '', kind: 'duckdb-core', channel: '', mode: 'mirror', visibility: 'private', prefix: '', keys: '', names: '', credential: '' })
  const [plats, setPlats] = useState<string[]>(['linux_amd64', 'linux_arm64', 'osx_arm64'])
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) => setF({ ...f, [k]: e.target.value })
  const repo = f.kind === 'repository'
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      const body: Record<string, unknown> = { name: f.name, kind: f.kind, channel: f.channel, mode: f.mode, visibility: f.visibility, platforms: plats,
        extensions: entryNames(f.names).map((name) => ({ name })) }
      if (repo) {
        body.prefix = f.prefix
        body.keys = entryNames(f.keys)
      }
      if (repo && f.credential) body.credential = f.credential
      await client.post(`${api}/upstreams`, { body })
      nav(`${base}/upstreams/${seg(f.name)}`)
    } catch (err) {
      setFailure(err)
    }
    setBusy(false)
  }
  return (
    <Card title="New upstream">
      <form onSubmit={submit} className="flex flex-col gap-4">
        <div className="flex flex-col gap-1">
          <span className="text-[13px] font-semibold text-ink">Source</span>
          <Chips label="source" value={f.kind} options={kinds} onChange={(k) => setF({ ...f, kind: k })} />
        </div>
        <div className="grid grid-cols-3 gap-4">
          <Field label="Name"><input className="field font-mono" required pattern="[a-z][a-z0-9-]{0,62}" value={f.name} onChange={set('name')} /></Field>
          <Field label="Into channel">
            <select className="field" required value={f.channel} onChange={set('channel')}>
              <option value="" disabled>choose…</option>
              {(channels.data?.data.channels ?? []).map((c) => <option key={c.name} value={c.name}>{c.name} ({c.kind})</option>)}
            </select>
          </Field>
          <Field label="Mode" hint="pull-through: fetched when first asked for">
            <select className="field" value={f.mode} onChange={set('mode')}><option>mirror</option><option>pull-through</option></select>
          </Field>
          <Field label="New releases are">
            <select className="field" value={f.visibility} onChange={set('visibility')}><option>private</option><option>public</option></select>
          </Field>
          {repo && <Field label="Repository prefix" hint="https://…, its .well-known must list a pinned key"><input className="field font-mono" required type="url" value={f.prefix} onChange={set('prefix')} /></Field>}
          {repo && <Field label="Pinned keys" hint="sha256:… fingerprints, separated by commas"><input className="field font-mono" required value={f.keys} onChange={set('keys')} /></Field>}
          {repo && <Field label="Credential" hint="a configured credential's name, for a private repository"><input className="field font-mono" value={f.credential} onChange={set('credential')} /></Field>}
        </div>
        <div className="flex flex-col gap-1">
          <span className="text-[13px] font-semibold text-ink">Platforms</span>
          <Checks label="platforms" options={platforms} value={plats} onChange={setPlats} />
        </div>
        <Field label="Extensions" hint="names separated by commas or spaces; * for every name the source has. More can be added later.">
          <input className="field font-mono" value={f.names} onChange={set('names')} />
        </Field>
        {failure != null && <ErrorBox error={failure} />}
        <div><Button type="submit" tone="brand" disabled={busy || plats.length === 0}>Add</Button></div>
      </form>
    </Card>
  )
}

/** One upstream, its state kept per upstream. */
export function UpstreamPage() {
  const { name = '' } = useParams()
  return <UpstreamView key={name} name={name} />
}

type Action = 'sync' | 'dry' | 'pause' | 'resume' | 'public' | 'private' | 'remove'

function UpstreamView({ name }: { name: string }) {
  const { client, api, base } = useTenant()
  const nav = useNavigate()
  const path = `${api}/upstreams/${seg(name)}`
  const up = useLoad(() => client.get<Upstream>(path), [path])
  const [pending, setPending] = useState<Action | null>(null)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  if (up.loading && !up.data) return <Loading />
  if (up.error) return up.error instanceof ApiError && up.error.status === 404 ? <Empty>No such upstream.</Empty> : <ErrorBox error={up.error} />
  const u = up.data!.data
  const etag = up.data!.etag || u.etag
  const run = async (a: Action) => {
    if (busy) return
    setBusy(true)
    setPending(null)
    setFailure(null)
    try {
      if (a === 'remove') {
        await client.del(path, { ifMatch: etag })
        nav(`${base}/upstreams`)
        return
      }
      if (a === 'sync' || a === 'dry') await client.post(`${path}/sync${a === 'dry' ? '?dry_run=true' : ''}`)
      else await client.post(`${path}/${a}`, { ifMatch: etag })
    } catch (e) {
      setFailure(e instanceof ApiError && e.status === 412 ? new Error('The upstream changed meanwhile: it was read again.') : e)
    } finally {
      setBusy(false)
    }
    up.reload()
  }
  const explain: Record<Action, string> = {
    sync: 'A running kista serve takes the run: it fetches what the allowlist names, checks DuckDB’s signature and releases what passes.',
    dry: 'A dry run checks what a sync would do and releases nothing.',
    pause: 'A paused upstream is never run, and pull-through fetches nothing from it.',
    resume: 'The upstream runs again when asked.',
    public: 'Its new releases are public: anyone may install them.',
    private: 'Its new releases are private: only callers granted install may install them.',
    remove: 'The upstream, its allowlist and its cells go. Releases it made stay in the channel.',
  }
  const facts: [string, React.ReactNode][] = [
    ['Source', u.kind === 'repository' ? <Mono>{u.prefix}</Mono> : kinds.find(([k]) => k === u.kind)?.[1] ?? u.kind],
    ['Into', <><Link to={`${base}/channels/${seg(u.channel)}`} className="font-mono">{u.channel}</Link> · {u.mode}</>],
    ['New releases', u.visibility],
    ['Credential', u.credential ? <Mono>{u.credential}</Mono> : <span className="text-ink-muted">none</span>],
    ['Allowlist', `${u.extensions.length} ${u.extensions.length === 1 ? 'entry' : 'entries'} × ${u.platforms.length} platforms`],
    ['Added', `${when(u.created_at)} by ${u.created_by}`],
  ]
  return (
    <>
      <PageTitle eyebrow={<Link to={`${base}/upstreams`} className="no-underline">Upstreams</Link>} title={<span className="font-mono">{u.name}</span>}>
        <span className="flex gap-1 pb-1"><Badge>{u.state}</Badge><Badge>{u.visibility}</Badge>{u.run !== 'idle' && <span className="chip bg-warning-soft text-warning">{runWords[u.run]}</span>}</span>
        <div className="ml-auto flex flex-wrap gap-2">
          <Button tone="brand" disabled={busy || u.state !== 'active'} onClick={() => setPending('sync')}>Sync now</Button>
          <Button tone="outline" disabled={busy || u.state !== 'active'} onClick={() => setPending('dry')}>Dry run</Button>
          <Button tone="outline" disabled={busy} onClick={() => setPending(u.state === 'active' ? 'pause' : 'resume')}>{u.state === 'active' ? 'Pause' : 'Resume'}</Button>
          <Button tone="outline" disabled={busy} onClick={() => setPending(u.visibility === 'public' ? 'private' : 'public')}>Make {u.visibility === 'public' ? 'private' : 'public'}</Button>
          <Button tone="danger-outline" disabled={busy} onClick={() => setPending('remove')}>Remove…</Button>
        </div>
      </PageTitle>
      {failure != null && <ErrorBox error={failure} />}
      <Card>
        <dl className="m-0 grid grid-cols-[10rem_1fr] gap-x-4 gap-y-2">
          {facts.map(([k, v]) => (
            <div key={k} className="contents"><dt className="text-ink-muted">{k}</dt><dd className="m-0 min-w-0 break-words">{v}</dd></div>
          ))}
        </dl>
      </Card>
      <LastRun u={u} onRefresh={up.reload} />
      <Allowlist path={path} onChanged={up.reload} />
      <Items u={u} path={path} onChanged={up.reload} etag={etag} />
      <Cells path={path} />
      <Confirm open={pending !== null} title={pending === 'dry' ? 'Dry run' : pending === 'remove' ? `Remove ${u.name}?` : `${pending ?? ''} ${u.name}`}
        confirm={pending === 'dry' ? 'Run' : pending ?? ''} danger={pending === 'remove'} onConfirm={() => run(pending!)} onClose={() => setPending(null)}>
        <p>{pending ? explain[pending] : ''}</p>
      </Confirm>
    </>
  )
}

function LastRun({ u, onRefresh }: { u: Upstream; onRefresh: () => void }) {
  const r = u.last_run
  return (
    <Card title="Last run" actions={<Button onClick={onRefresh}>Refresh</Button>}>
      {!r ? <Empty>{u.run !== 'idle' ? `Not run yet: ${runWords[u.run]}.` : 'Never run.'}</Empty> : (
        <div className="flex flex-col gap-3">
          <p className="m-0">{when(r.started)} → {when(r.ended)}{r.dry_run && <span className="chip ml-2 bg-surface-soft text-ink">dry run</span>}</p>
          {r.error && <ErrorBox error={r.error} />}
          <div className="flex flex-wrap gap-1.5">
            {Object.entries(r.counts ?? {}).map(([k, n]) => <span key={k} className="chip bg-surface-soft text-ink">{k} {n}</span>)}
          </div>
          {r.problems && r.problems.length > 0 && (
            <Table rows={r.problems} rowKey={(p) => `${p.duckdb_version}/${p.platform}/${p.name}`} columns={[
              ['Extension', (p) => <Mono>{p.name}</Mono>],
              ['For', (p) => <Mono>{p.duckdb_version} {p.platform}</Mono>],
              ['Outcome', (p) => <Badge>{p.outcome}</Badge>],
              ['Why', (p) => <span className="text-[13px]">{p.detail ?? ''}</span>],
            ]} />
          )}
        </div>
      )}
    </Card>
  )
}

/** The allowlist, page by page with a prefix; entries added (or their versions replaced) and removed. */
function Allowlist({ path, onChanged }: { path: string; onChanged: () => void }) {
  const { client } = useTenant()
  const [prefix, setPrefix] = useState('')
  const [f, setF] = useState({ name: '', versions: '', allow_reserved: false })
  const [removing, setRemoving] = useState('')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const list = usePaged<Entry>(async (cursor, limit) => {
    const q = new URLSearchParams({ limit: String(limit) })
    if (prefix) q.set('prefix', prefix)
    if (cursor) q.set('cursor', cursor)
    const r = await client.get<{ extensions: Entry[]; next?: string }>(`${path}/extensions?${q}`)
    return { items: r.data.extensions, next: r.data.next }
  }, [path, prefix])
  const act = async (fn: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      await fn()
      setF({ name: '', versions: '', allow_reserved: false })
    } catch (e) {
      setFailure(e)
    }
    setBusy(false)
    list.reload()
    onChanged()
  }
  return (
    <Card title="Allowlist">
      <div className="flex flex-col gap-4">
        <SearchBox label="Find an entry" value={prefix} onChange={setPrefix} />
        {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : list.items.length === 0 ? <Empty>{prefix ? `No entry starts with “${prefix}”.` : 'Nothing allowed in yet.'}</Empty> : (
          <Table rows={list.items} rowKey={(e) => e.name} columns={[
            ['Extension', (e) => <Mono>{e.name}</Mono>],
            ['Versions', (e) => (e.versions.length ? <Mono>{e.versions.join(' ')}</Mono> : <span className="text-ink-muted">every version</span>)],
            ['Reserved', (e) => (e.allow_reserved ? 'allowed over a core name' : '')],
            ['', (e) => <Button tone="danger-outline" disabled={busy} onClick={() => setRemoving(e.name)}>Remove…</Button>],
          ]} />
        )}
        <Pager page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} />
        <form className="flex flex-wrap items-end gap-3 border-t border-line pt-4"
          onSubmit={(e) => { e.preventDefault(); act(() => client.post(`${path}/extensions`, { body: { name: f.name, versions: entryNames(f.versions), allow_reserved: f.allow_reserved } })) }}>
          <Field label="Extension" hint="* for every name"><input className="field font-mono" required value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} /></Field>
          <Field label="Versions" hint="empty: every version; an existing entry's are replaced"><input className="field font-mono" value={f.versions} onChange={(e) => setF({ ...f, versions: e.target.value })} /></Field>
          <label className="flex items-center gap-2 pb-2 text-[13px]"><input type="checkbox" checked={f.allow_reserved} onChange={(e) => setF({ ...f, allow_reserved: e.target.checked })} /> allow a core name</label>
          <Button type="submit" tone="brand" disabled={busy}>Add or replace</Button>
        </form>
        {failure != null && <ErrorBox error={failure} />}
      </div>
      <Confirm open={removing !== ''} title={`Remove ${removing} from the allowlist?`} confirm="Remove" danger
        onConfirm={() => { const n = removing; setRemoving(''); act(() => client.del(`${path}/extensions/${seg(n)}`)) }} onClose={() => setRemoving('')}>
        <p>Runs stop fetching it. Releases already made stay.</p>
      </Confirm>
    </Card>
  )
}

/** Platforms and pinned keys (a repository's), added and removed; the credential set or cleared. */
function Items({ u, path, etag, onChanged }: { u: Upstream; path: string; etag: string; onChanged: () => void }) {
  const { client } = useTenant()
  const [platform, setPlatform] = useState('')
  const [key, setKey] = useState('')
  const [credential, setCredential] = useState(u.credential ?? '')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const act = async (fn: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      await fn()
      setPlatform('')
      setKey('')
    } catch (e) {
      setFailure(e instanceof ApiError && e.status === 412 ? new Error('The upstream changed meanwhile: it was read again.') : e)
    }
    setBusy(false)
    onChanged()
  }
  const chip = (label: string, onRemove: () => void) => (
    <span key={label} className="chip flex items-center gap-1 border border-line text-ink">
      <Mono>{label}</Mono>
      <button type="button" aria-label={`remove ${label}`} disabled={busy} onClick={onRemove} className="text-ink-muted hover:text-danger">×</button>
    </span>
  )
  return (
    <Card title="Platforms, keys and credential">
      <div className="flex flex-col gap-4">
        <div className="flex flex-col gap-2">
          <span className="text-[13px] font-semibold">Platforms</span>
          <div className="flex flex-wrap gap-1.5">{u.platforms.map((p) => chip(p, () => act(() => client.del(`${path}/platforms/${seg(p)}`))))}</div>
          <form className="flex items-center gap-2" onSubmit={(e) => { e.preventDefault(); act(() => client.post(`${path}/platforms`, { body: { platform } })) }}>
            <select aria-label="platform to add" className="field" value={platform} onChange={(e) => setPlatform(e.target.value)} required>
              <option value="" disabled>add a platform…</option>
              {platforms.filter((p) => !u.platforms.includes(p)).map((p) => <option key={p}>{p}</option>)}
            </select>
            <Button type="submit" disabled={busy || !platform}>Add</Button>
          </form>
        </div>
        {u.kind === 'repository' && (
          <div className="flex flex-col gap-2">
            <span className="text-[13px] font-semibold">Pinned keys</span>
            <div className="flex flex-wrap gap-1.5">{u.keys.map((k) => chip(k, () => act(() => client.del(`${path}/keys/${seg(k)}`))))}</div>
            <form className="flex items-center gap-2" onSubmit={(e) => { e.preventDefault(); act(() => client.post(`${path}/keys`, { body: { fingerprint: key } })) }}>
              <input aria-label="key fingerprint to pin" className="field w-[36rem] font-mono" placeholder="sha256:…" value={key} onChange={(e) => setKey(e.target.value)} required />
              <Button type="submit" disabled={busy}>Pin</Button>
            </form>
          </div>
        )}
        <form className="flex items-end gap-2" onSubmit={(e) => { e.preventDefault(); act(() => client.post(`${path}/credential`, { ifMatch: etag, body: { credential } })) }}>
          <Field label="Credential" hint="a configured credential's name; empty clears it"><input className="field font-mono" value={credential} onChange={(e) => setCredential(e.target.value)} /></Field>
          <Button type="submit" disabled={busy || credential === (u.credential ?? '')}>Save</Button>
        </form>
        {failure != null && <ErrorBox error={failure} />}
      </div>
    </Card>
  )
}

/** The cells (DuckDB version × platform × name) and their last outcome, page by page. */
function Cells({ path }: { path: string }) {
  const { client } = useTenant()
  const [outcome, setOutcome] = useState('')
  const list = usePaged<Cell>(async (cursor, limit) => {
    const q = new URLSearchParams({ limit: String(limit) })
    if (outcome) q.set('outcome', outcome)
    if (cursor) q.set('cursor', cursor)
    const r = await client.get<{ cells: Cell[]; next?: string }>(`${path}/cells?${q}`)
    return { items: r.data.cells, next: r.data.next }
  }, [path, outcome])
  return (
    <Card title="Cells">
      <div className="flex flex-col gap-4">
        <Chips label="outcome" value={outcome} onChange={setOutcome} options={[['', 'All'], ...outcomes.map((o) => [o, o] as [string, string])]} />
        {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : list.items.length === 0 ? <Empty>No cells{outcome ? ` ${outcome}` : ''}.</Empty> : (
          <Table rows={list.items} rowKey={(c) => `${c.duckdb_version}/${c.platform}/${c.name}`} columns={[
            ['Extension', (c) => <Mono>{c.name}</Mono>],
            ['DuckDB', (c) => <Mono>{c.duckdb_version}</Mono>],
            ['Platform', (c) => <Mono>{c.platform}</Mono>],
            ['Outcome', (c) => <Badge>{c.outcome}</Badge>],
            ['Why', (c) => <span className="text-[13px]">{c.detail ?? ''}</span>],
            ['Fetched', (c) => <span className="text-[13px] text-ink-muted">{when(c.fetched_at)}</span>],
          ]} />
        )}
        <Pager page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} />
      </div>
    </Card>
  )
}
