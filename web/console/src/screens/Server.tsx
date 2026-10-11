import { useMemo, useState } from 'react'
import { Link, Navigate, Route, Routes, useNavigate, useParams } from 'react-router-dom'
import { Building2, Cpu, List } from 'lucide-react'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, Pager, Table, when } from '../components/ui'
import { Perms, TenantContext, useLoad, usePaged } from '../lib/context'
import { useSections } from '../lib/frame'
import { ApiError, type Client, seg } from '../lib/http'
import { Events } from './Events'
import { TenantArea } from './TenantArea'

interface Tenant {
  name: string
  display_name?: string
  state: string
  storage_domain?: string
  created_at: string
  etag: string
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
      <Route path="events" element={<Events key="server" client={client} path="/events" title="Server events" />} />
      <Route path="tenants/:t/*" element={<ServerTenant client={client} root={root} />} />
    </Routes>
  )
}

function NewTenant({ client, root }: { client: Client; root: string }) {
  const nav = useNavigate()
  const [f, setF] = useState({ name: '', display_name: '', storage_domain: '' })
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: e.target.value })
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      await client.post('/tenants', { body: f })
      nav(`${root}/tenants/${seg(f.name)}`)
    } catch (err) {
      setFailure(err)
    }
    setBusy(false)
  }
  return (
    <Card title="New tenant">
      <form className="flex flex-col gap-4" onSubmit={submit}>
        <div className="grid grid-cols-3 gap-4">
          <Field label="Name" hint="its path: /acme/…, lowercase letters, digits and -"><input className="field font-mono" required pattern="[a-z0-9][a-z0-9-]{0,62}" value={f.name} onChange={set('name')} /></Field>
          <Field label="Display name"><input className="field" maxLength={200} value={f.display_name} onChange={set('display_name')} /></Field>
          <Field label="Storage domain" hint="empty: the default one"><input className="field font-mono" value={f.storage_domain} onChange={set('storage_domain')} /></Field>
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
            ['Created', (t) => when(t.created_at)],
          ]}
        />
      )}
      <Pager page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} />
    </>
  )
}

function ServerTenant({ client, root }: { client: Client; root: string }) {
  const { t = '' } = useParams()
  const info = useLoad(() => client.get<Tenant>(`/tenants/${seg(t)}`), [t])
  const [pending, setPending] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const suspended = info.data?.data.state === 'suspended'
  const toggle = async () => {
    setPending(false)
    setFailure(null)
    try {
      await client.post(`/tenants/${seg(t)}/${suspended ? 'resume' : 'suspend'}`, { ifMatch: info.data!.etag || info.data!.data.etag })
    } catch (e) {
      setFailure(e)
    }
    info.reload()
  }
  const perms = useMemo(() => new Perms(true), [])
  const label = <>Tenant <span className="font-mono normal-case tracking-normal">{t}</span></>
  return (
    <TenantContext.Provider value={{ client, tenant: t, perms, api: `/tenants/${seg(t)}`, base: `${root}/tenants/${seg(t)}` }}>
      <div className="flex items-center gap-3">
        <h1 className="m-0 font-mono text-[24px] font-bold leading-8">{t}</h1>
        {info.data && <Badge>{info.data.data.state}</Badge>}
        {info.data && <span className="ml-auto"><Button tone={suspended ? 'outline' : 'danger-outline'} onClick={() => setPending(true)}>{suspended ? 'Resume…' : 'Suspend…'}</Button></span>}
      </div>
      {failure != null && <ErrorBox error={failure} />}
      <Confirm open={pending} title={`${suspended ? 'Resume' : 'Suspend'} ${t}?`} confirm={suspended ? 'Resume' : 'Suspend'} danger={!suspended} onConfirm={toggle} onClose={() => setPending(false)}>
        <p>{suspended ? 'The tenant serves and answers again.' : 'The tenant serves nothing and its API answers as if it did not exist, until it is resumed. Nothing is deleted.'}</p>
      </Confirm>
      <TenantArea label={label} />
    </TenantContext.Provider>
  )
}

interface Version {
  name: string
  kind: string
  c_api_maxima?: string[]
}

/** DuckDB versions (server administrators): the ones channels may serve, with a maximum C API per
 * major (spec 0006). */
function Versions({ client }: { client: Client }) {
  const list = useLoad(() => client.get<{ duckdb_versions: Version[] }>('/duckdb-versions'), [])
  const [f, setF] = useState({ name: '', kind: 'release', capis: '' })
  const [capi, setCapi] = useState<{ version: string; value: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const act = async (fn: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      await fn()
      setF({ name: '', kind: 'release', capis: '' })
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
        <form className="flex flex-col gap-4" onSubmit={(e) => { e.preventDefault(); act(() => client.post('/duckdb-versions', { body: { name: f.name, kind: f.kind, c_api_maxima: f.capis.split(/[\s,]+/).filter(Boolean) } })) }}>
          <div className="grid grid-cols-3 gap-4">
            <Field label="Version" hint="a release tag (v2.0.0) or a source id"><input className="field font-mono" required value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} /></Field>
            <Field label="Kind"><select className="field" value={f.kind} onChange={(e) => setF({ ...f, kind: e.target.value })}><option>release</option><option>dev</option></select></Field>
            <Field label="Maximum C APIs" hint="one per major: v1.5.6, v2.0.0"><input className="field font-mono" value={f.capis} onChange={(e) => setF({ ...f, capis: e.target.value })} /></Field>
          </div>
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
