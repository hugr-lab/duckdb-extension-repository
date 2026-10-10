import { useMemo } from 'react'
import { Link, Navigate, Route, Routes, useParams } from 'react-router-dom'
import { Building2, List } from 'lucide-react'
import { Badge, Button, Empty, ErrorBox, Loading, PageTitle, Pager, Table, when } from '../components/ui'
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
      <Route path="events" element={<Events key="server" client={client} path="/events" title="Server events" />} />
      <Route path="tenants/:t/*" element={<ServerTenant client={client} root={root} />} />
    </Routes>
  )
}

function Tenants({ client, root }: { client: Client; root: string }) {
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
        <span className="ml-auto"><Button onClick={list.reload}>Refresh</Button></span>
      </PageTitle>
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
  const perms = useMemo(() => new Perms(true), [])
  const label = <>Tenant <span className="font-mono normal-case tracking-normal">{t}</span></>
  return (
    <TenantContext.Provider value={{ client, tenant: t, perms, api: `/tenants/${seg(t)}`, base: `${root}/tenants/${seg(t)}` }}>
      <div className="flex items-center gap-3">
        <h1 className="m-0 font-mono text-[24px] font-bold leading-8">{t}</h1>
        {info.data && <Badge>{info.data.data.state}</Badge>}
      </div>
      <TenantArea label={label} />
    </TenantContext.Provider>
  )
}
