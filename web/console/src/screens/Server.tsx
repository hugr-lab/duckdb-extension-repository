import { useMemo, useState } from 'react'
import { Link, Navigate, Route, Routes, useNavigate, useParams } from 'react-router-dom'
import { Building2, Cpu, KeySquare, List, Lock } from 'lucide-react'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, Pager, Table, when } from '../components/ui'
import { Perms, TenantContext, useLoad, usePaged } from '../lib/context'
import { useSections } from '../lib/frame'
import { ApiError, type Client, seg } from '../lib/http'
import { useAction } from '../lib/action'
import { Events } from './Events'
import { Credentials, ReservedNames } from './ServerLists'
import { TenantArea } from './TenantArea'

export interface DeletionKey {
  kind: string
  id: string
  fingerprint: string
  signer_ref: string
}

export interface Tenant {
  name: string
  display_name?: string
  state: 'active' | 'suspended' | 'deleting' | 'deleted'
  storage_domain?: string
  created_at: string
  suspended_at?: string
  deletable_at?: string
  deletion?: {
    started_at: string
    started_by: string
    passes: number
    table?: string
    deleted_rows: number
    last_error?: string
    next_attempt_at?: string
    finished_at?: string
    keys: DeletionKey[]
  }
  /** The holder its licences must name (spec 0028), when one is set. */
  holder_id?: string
  etag: string
}

/** Whether a suspended tenant may be deleted now (spec 0019: from deletable_at). */
export function deletable(t: Pick<Tenant, 'state' | 'deletable_at'>, now: Date): boolean {
  return t.state === 'suspended' && !!t.deletable_at && new Date(t.deletable_at) <= now
}

/** The server scope: its administrator is gated by /api/v1/whoami, and acts in any tenant with the
 * server token (spec 0007), so tenant whoami is never asked. root is where its pages are:
 * "/server" standalone, "" embedded. */
export function Server({ client, root }: { client: Client; root: string }) {
  const me = useLoad(() => client.get<{ administrator: boolean }>('/whoami'), [])
  const ok = !!me.data?.data.administrator
  useSections('server', ok ? {
    items: [
      { to: `${root}/tenants`, label: 'Tenants', icon: Building2 },
      { to: `${root}/versions`, label: 'DuckDB versions', icon: Cpu },
      { to: `${root}/reserved`, label: 'Reserved names', icon: Lock },
      { to: `${root}/credentials`, label: 'Credentials', icon: KeySquare },
      { to: `${root}/events`, label: 'Server events', icon: List, end: true },
    ],
  } : null, `${root}|${ok}`)
  if (me.loading && !me.data) return <Loading />
  // a tenant's token here is 400 (spec 0007): not a server administrator either
  if (me.error && !(me.error instanceof ApiError && me.error.status === 400)) return <ErrorBox error={me.error} />
  if (!ok) return <Empty>Not a server administrator.</Empty>
  return (
    <Routes>
      <Route index element={<Navigate to={`${root}/tenants`} replace />} />
      <Route path="tenants" element={<Tenants client={client} root={root} />} />
      <Route path="versions" element={<Versions client={client} />} />
      <Route path="reserved" element={<ReservedNames client={client} />} />
      <Route path="credentials" element={<Credentials client={client} />} />
      <Route path="events" element={<Events key="server" client={client} path="/events" title="Server events" />} />
      <Route path="tenants/:t/*" element={<ServerTenant client={client} root={root} />} />
    </Routes>
  )
}

export interface NewTenantForm {
  name: string
  display_name: string
  storage_domain: string
  holder_id: string
}

/** A new tenant's request (POST /tenants): an empty holder id is none, so it is left out. The API
 * checks the rest (a holder id is 1 to 128 bytes without U+0000, as a licence's sub). */
export function newTenantBody(f: NewTenantForm) {
  const { holder_id, ...rest } = f
  return holder_id === '' ? rest : { ...rest, holder_id }
}

function NewTenant({ client, root }: { client: Client; root: string }) {
  const nav = useNavigate()
  const [f, setF] = useState<NewTenantForm>({ name: '', display_name: '', storage_domain: '', holder_id: '' })
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: e.target.value })
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      await client.post('/tenants', { body: newTenantBody(f) })
      nav(`${root}/tenants/${seg(f.name)}`)
    } catch (err) {
      setFailure(err)
    }
    setBusy(false)
  }
  return (
    <Card title="New tenant">
      <form className="flex flex-col gap-4" onSubmit={submit}>
        <div className="grid grid-cols-2 gap-4">
          <Field label="Name" hint="its path: /acme/…, lowercase letters, digits and -"><input className="field font-mono" required pattern="[a-z0-9][a-z0-9-]{0,62}" value={f.name} onChange={set('name')} /></Field>
          <Field label="Display name"><input className="field" maxLength={200} value={f.display_name} onChange={set('display_name')} /></Field>
          <Field label="Storage domain" hint="empty: the default one"><input className="field font-mono" value={f.storage_domain} onChange={set('storage_domain')} /></Field>
          <Field label="Expected holder" hint="optional: the holder id (a licence's sub, 1 to 128 bytes) its licences must name; empty: any">
            <input className="field font-mono" value={f.holder_id} onChange={set('holder_id')} />
          </Field>
        </div>
        {failure != null && <ErrorBox error={failure} />}
        <div><Button type="submit" tone="brand" disabled={busy}>Create</Button></div>
      </form>
    </Card>
  )
}

function Tenants({ client, root }: { client: Client; root: string }) {
  const [creating, setCreating] = useState(false)
  const list = usePaged<Tenant>(async (cursor, limit) => {
    const q = new URLSearchParams({ limit: String(limit) })
    if (cursor) q.set('cursor', cursor)
    const r = await client.get<{ tenants: Tenant[]; next?: string }>(`/tenants?${q}`)
    return { items: r.data.tenants, next: r.data.next }
  }, [])
  return (
    <>
      <PageTitle title="Tenants">
        <span className="pb-1 text-ink-muted">Open one to act in it with every right.</span>
        <span className="ml-auto flex gap-2"><Button onClick={list.reload}>Refresh</Button><Button tone="brand" onClick={() => setCreating(!creating)}>{creating ? 'Close' : 'New tenant'}</Button></span>
      </PageTitle>
      {creating && <NewTenant client={client} root={root} />}
      {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : (
        <Table
          rows={list.items}
          rowKey={(t) => t.name}
          columns={[
            ['Tenant', (t) => <Link className="font-mono font-medium no-underline" to={`${root}/tenants/${seg(t.name)}`}>{t.name}</Link>],
            ['Name', (t) => t.display_name ?? ''],
            ['State', (t) => <Badge>{t.state}</Badge>],
            ['Storage domain', (t) => <span className="font-mono text-[13px] text-ink-muted">{t.storage_domain ?? ''}</span>],
            ['Expected holder', (t) => (t.holder_id ? <Mono>{t.holder_id}</Mono> : '')],
            ['Created', (t) => when(t.created_at)],
          ]}
        />
      )}
      <Pager page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} />
    </>
  )
}

/** One tenant in the server scope, its state kept per tenant. */
function ServerTenant({ client, root }: { client: Client; root: string }) {
  const { t = '' } = useParams()
  return <ServerTenantView key={t} t={t} client={client} root={root} />
}

function ServerTenantView({ t, client, root }: { t: string; client: Client; root: string }) {
  const info = useLoad(() => client.get<Tenant>(`/tenants/${seg(t)}`), [t])
  const [pending, setPending] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [typed, setTyped] = useState('')
  const { busy, failure, act } = useAction('The tenant', info.reload)
  const suspended = info.data?.data.state === 'suspended'
  const toggle = async () => {
    setPending(false)
    await act(() => client.post(`/tenants/${seg(t)}/${suspended ? 'resume' : 'suspend'}`, { ifMatch: info.data!.etag || info.data!.data.etag }))
    info.reload()
  }
  const remove = async () => {
    setDeleting(false)
    setTyped('')
    await act(() => client.del(`/tenants/${seg(t)}`, { ifMatch: info.data!.etag || info.data!.data.etag }))
    info.reload()
  }
  const perms = useMemo(() => new Perms(true), [])
  const label = <>Tenant <span className="font-mono normal-case tracking-normal">{t}</span></>
  const tn = info.data?.data
  if (tn && (tn.state === 'deleting' || tn.state === 'deleted')) return <DeletionView t={tn} reload={info.reload} />
  return (
    <TenantContext.Provider value={{ client, tenant: t, perms, api: `/tenants/${seg(t)}`, base: `${root}/tenants/${seg(t)}` }}>
      <div className="flex items-center gap-3">
        <h1 className="m-0 font-mono text-[24px] font-bold leading-8">{t}</h1>
        {info.data && <Badge>{info.data.data.state}</Badge>}
        {info.data && (
          <span className="ml-auto flex gap-2">
            <Button tone={suspended ? 'outline' : 'danger-outline'} disabled={busy || info.loading} onClick={() => setPending(true)}>{suspended ? 'Resume…' : 'Suspend…'}</Button>
            {suspended && (
              <Button tone="danger-outline" disabled={busy || info.loading || !deletable(info.data.data, new Date())} onClick={() => setDeleting(true)}
                title={info.data.data.deletable_at ? `may be deleted from ${when(info.data.data.deletable_at)}` : undefined}>Delete…</Button>
            )}
          </span>
        )}
      </div>
      {tn && <Holder client={client} t={t} holder={tn.holder_id} etag={info.data!.etag || tn.etag} reload={info.reload} />}
      {suspended && tn?.deletable_at && !deletable(tn, new Date()) && <Note>It may be deleted from {when(tn.deletable_at)} (tenants.min_suspended).</Note>}
      {suspended && tn && !tn.deletable_at && <Note>An older version suspended it: resume it and suspend it again before deleting it.</Note>}
      {failure != null && <ErrorBox error={failure} />}
      <Confirm open={pending} title={`${suspended ? 'Resume' : 'Suspend'} ${t}?`} confirm={suspended ? 'Resume' : 'Suspend'} danger={!suspended} onConfirm={toggle} onClose={() => setPending(false)}>
        <p>{suspended ? 'The tenant serves and answers again.' : 'The tenant serves nothing and its API answers as if it did not exist, until it is resumed. Nothing is deleted.'}</p>
      </Confirm>
      <Confirm open={deleting} title={`Delete ${t}?`} confirm="Delete" danger disabled={typed !== t} onConfirm={remove}
        onClose={() => { setDeleting(false); setTyped('') }}>
        <p>Its channels, keys, releases, grants, publishers, upstreams, documents, feeds and statistics are removed, and cannot be
          restored. Its name is never given to another tenant. Its events stay until their retention ends.</p>
        <p>Retire its keys in their key sources afterwards: the deletion's record lists them.</p>
        <label className="block">
          Type <Mono>{t}</Mono> to confirm:
          <input data-autofocus aria-label="confirm name" className="field mt-1 w-full font-mono" value={typed} onChange={(e) => setTyped(e.target.value)} />
        </label>
      </Confirm>
      <TenantArea label={label} />
    </TenantContext.Provider>
  )
}

/** A tenant's expected holder (spec 0028): the holder id its licences must name (their sub). A
 * server administrator sets and clears it with the tenant's ETag; setting it while the tenant holds
 * a licence of another holder is refused (409, kista naming them), and a 412 reads the tenant again. */
export function Holder({ client, t, holder, etag, reload }: { client: Client; t: string; holder?: string; etag: string; reload: () => void }) {
  const [editing, setEditing] = useState(false)
  const [clearing, setClearing] = useState(false)
  const [value, setValue] = useState('')
  const { busy, failure, act } = useAction('The tenant', reload)
  const path = `/tenants/${seg(t)}/holder`
  const set = async () => {
    setEditing(false)
    if (await act(() => client.put(path, { body: { holder_id: value }, ifMatch: etag }))) reload()
  }
  const clear = async () => {
    setClearing(false)
    if (await act(() => client.del(path, { ifMatch: etag }))) reload()
  }
  return (
    <>
      <div role="group" aria-label="expected holder" className="flex flex-wrap items-center gap-3 text-[13px]">
        <span className="font-semibold">Expected holder</span>
        {holder ? <Mono>{holder}</Mono> : <span className="text-ink-muted">none: licences of any holder are stored</span>}
        <span className="ml-auto flex gap-2">
          <Button tone="outline" disabled={busy} onClick={() => { setValue(holder ?? ''); setEditing(true) }}>{holder ? 'Change holder…' : 'Set holder…'}</Button>
          {holder && <Button tone="danger-outline" disabled={busy} onClick={() => setClearing(true)}>Clear holder…</Button>}
        </span>
      </div>
      {failure != null && <ErrorBox error={failure} />}
      <Confirm open={editing} title={`The expected holder of ${t}`} confirm="Set" disabled={busy || value === '' || value === holder}
        onConfirm={set} onClose={() => setEditing(false)}>
        <p>kista then refuses a licence of another holder on every path that stores one: an import, an upstream's relay, a
          bundle. Setting it is refused while the tenant holds a licence of another holder: remove those first.</p>
        <label className="block">
          Holder id, as a licence's <Mono>sub</Mono> (1 to 128 bytes):
          <input data-autofocus aria-label="holder id" className="field mt-1 w-full font-mono" value={value} onChange={(e) => setValue(e.target.value)} />
        </label>
      </Confirm>
      <Confirm open={clearing} title={`Clear the expected holder of ${t}?`} confirm="Clear" danger onConfirm={clear} onClose={() => setClearing(false)}>
        <p>Licences of any holder may then be stored in the tenant. Those stored stay.</p>
      </Confirm>
    </>
  )
}

/** A tenant whose deletion started (spec 0019): its tenant screens would all be unknown, so its
 * deletion is shown instead, with the keys to retire. */
function DeletionView({ t, reload }: { t: Tenant; reload: () => void }) {
  const d = t.deletion
  return (
    <>
      <div className="flex items-center gap-3">
        <h1 className="m-0 font-mono text-[24px] font-bold leading-8">{t.name}</h1>
        <Badge>{t.state}</Badge>
        <span className="ml-auto"><Button onClick={reload}>Refresh</Button></span>
      </div>
      <Card title="Deletion">
        {!d ? <Empty>No deletion record.</Empty> : (
          <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-[13px]">
            <dt className="text-ink-muted">Started</dt><dd>{when(d.started_at)} by <Mono>{d.started_by}</Mono></dd>
            {d.finished_at && <><dt className="text-ink-muted">Deleted</dt><dd>{when(d.finished_at)}</dd></>}
            <dt className="text-ink-muted">Rows deleted</dt><dd>{d.deleted_rows} in {d.passes} {d.passes === 1 ? 'pass' : 'passes'}</dd>
            {d.table && <><dt className="text-ink-muted">Reached</dt><dd><Mono>{d.table}</Mono></dd></>}
            {d.last_error && <><dt className="text-ink-muted">Last error</dt><dd className="text-danger">{d.last_error}</dd></>}
            {!d.finished_at && d.next_attempt_at && <><dt className="text-ink-muted">Next attempt</dt><dd>{when(d.next_attempt_at)}</dd></>}
          </dl>
        )}
      </Card>
      <Card title="Keys to retire">
        <Note>kista only signed with these; disable or destroy them in their key sources.</Note>
        {!d || d.keys.length === 0 ? <Empty>No keys.</Empty> : (
          <Table rows={d.keys} rowKey={(k) => k.id} columns={[
            ['Kind', (k) => <Badge>{k.kind}</Badge>],
            ['Fingerprint', (k) => <Mono>{k.fingerprint}</Mono>],
            ['Signer', (k) => <Mono>{k.signer_ref}</Mono>],
          ]} />
        )}
      </Card>
    </>
  )
}

interface Version {
  name: string
  kind: string
  c_api_maxima?: string[]
}

/** coreExtensions reads what kista duckdb core-extensions prints: a name per line, and the commit
 * its "# commit" line names (spec 0030). */
export function coreExtensions(text: string): { core_extensions: string[]; core_extensions_commit?: string } {
  const names: string[] = []
  let commit: string | undefined
  for (const raw of text.split('\n')) {
    const l = raw.trim()
    if (l.startsWith('#')) {
      if (l.includes('duckdb/community-extensions')) throw new Error("this is duckdb/community-extensions' list, not a DuckDB version's core extensions")
      const m = /^#\s*commit\s+([0-9a-f]{7,64})\b/.exec(l)
      if (m && !commit) commit = m[1]
    } else if (l) {
      names.push(l)
    }
  }
  return commit ? { core_extensions: names, core_extensions_commit: commit } : { core_extensions: names }
}

/** DuckDB versions (server administrators): the ones channels may serve, with a maximum C API per
 * major (spec 0006) and their core extensions (spec 0030). */
function Versions({ client }: { client: Client }) {
  const list = useLoad(() => client.get<{ duckdb_versions: Version[] }>('/duckdb-versions'), [])
  const empty = { name: '', kind: 'release', capis: '', core: '' }
  const [f, setF] = useState(empty)
  const [capi, setCapi] = useState<{ version: string; value: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const act = async (fn: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      await fn()
      setF(empty)
      setCapi(null)
    } catch (e) {
      setFailure(e)
    }
    setBusy(false)
    list.reload()
  }
  return (
    <>
      <PageTitle title="DuckDB versions"><span className="pb-1 text-ink-muted">What channels may serve; a channel names its own.</span></PageTitle>
      <Card title="Add a version">
        <form className="flex flex-col gap-4" onSubmit={(e) => { e.preventDefault(); act(() => client.post('/duckdb-versions', { body: { name: f.name, kind: f.kind, c_api_maxima: f.capis.split(/[\s,]+/).filter(Boolean), ...coreExtensions(f.core) } })) }}>
          <div className="grid grid-cols-3 gap-4">
            <Field label="Version" hint="a release tag (v2.0.0) or a source id"><input className="field font-mono" required value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} /></Field>
            <Field label="Kind"><select className="field" value={f.kind} onChange={(e) => setF({ ...f, kind: e.target.value })}><option>release</option><option>dev</option></select></Field>
            <Field label="Maximum C APIs" hint="one per major: v1.5.6, v2.0.0"><input className="field font-mono" value={f.capis} onChange={(e) => setF({ ...f, capis: e.target.value })} /></Field>
          </div>
          <Field label="Core extensions" hint="what kista duckdb core-extensions <version> prints: they become reserved names">
            <textarea className="field h-28 font-mono" required value={f.core} onChange={(e) => setF({ ...f, core: e.target.value })} />
          </Field>
          <div><Button type="submit" tone="brand" disabled={busy}>Add</Button></div>
        </form>
      </Card>
      {failure != null && <ErrorBox error={failure} />}
      {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : list.data!.data.duckdb_versions.length === 0 ? <Empty>No versions yet.</Empty> : (
        <Table rows={list.data!.data.duckdb_versions} rowKey={(v) => v.name} columns={[
          ['Version', (v) => <Mono>{v.name}</Mono>],
          ['Kind', (v) => <Badge>{v.kind}</Badge>],
          ['Maximum C APIs', (v) => <Mono>{(v.c_api_maxima ?? []).join(' ')}</Mono>],
          ['', (v) => (capi?.version === v.name ? (
            <form className="flex gap-2" onSubmit={(e) => { e.preventDefault(); act(() => client.post(`/duckdb-versions/${seg(v.name)}/c-apis`, { body: { c_api: capi.value } })) }}>
              <input aria-label="C API" className="field w-32 font-mono" required placeholder="v3.0.0" value={capi.value} onChange={(e) => setCapi({ version: v.name, value: e.target.value })} />
              <Button type="submit" disabled={busy}>Add</Button>
              <Button onClick={() => setCapi(null)}>Cancel</Button>
            </form>
          ) : <Button tone="outline" onClick={() => setCapi({ version: v.name, value: '' })}>+ C API</Button>)],
        ]} />
      )}
      <Note>A maximum C API names the newest C API a major of DuckDB offers: C API extensions built for it or older load there.</Note>
    </>
  )
}
