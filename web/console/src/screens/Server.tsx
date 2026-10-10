import { Link, NavLink, Route, Routes, useParams } from 'react-router-dom'
import { Badge, Card, Empty, ErrorBox, Loading, Table, when } from '../components/ui'
import { Perms, TenantContext, useLoad } from '../lib/context'
import { type Client, seg } from '../lib/http'
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
 * server token (spec 0007), so tenant whoami is never asked. */
export function Server({ client }: { client: Client }) {
  const me = useLoad(() => client.get<{ administrator: boolean }>('/whoami'), [])
  if (me.loading) return <Loading />
  if (me.error) return <ErrorBox error={me.error} />
  if (!me.data?.data.administrator) return <Empty>Not a server administrator.</Empty>
  return (
    <div className="space-y-4">
      <nav className="flex gap-2" aria-label="server sections">
        {[['/server', 'Tenants'], ['/server/events', 'Server events']].map(([to, label]) => (
          <NavLink key={to} end to={to} className={({ isActive }) => `rounded-full px-4 py-1.5 text-sm font-semibold ${isActive ? 'bg-brand text-brand-on' : 'text-ink hover:bg-row'}`}>
            {label}
          </NavLink>
        ))}
      </nav>
      <Routes>
        <Route index element={<Tenants client={client} />} />
        <Route path="events" element={<Events key="server" client={client} path="/events" title="Server events" />} />
        <Route path="tenants/:t/*" element={<ServerTenant client={client} />} />
      </Routes>
    </div>
  )
}

function Tenants({ client }: { client: Client }) {
  const { data, error, loading } = useLoad(async () => {
    const out: Tenant[] = []
    let cursor = ''
    for (let i = 0; i < 100; i++) { // 50,000 tenants
      const q = new URLSearchParams({ limit: '500' })
      if (cursor) q.set('cursor', cursor)
      const r = await client.get<{ tenants: Tenant[]; next?: string }>(`/tenants?${q}`)
      out.push(...r.data.tenants)
      if (!r.data.next) break
      cursor = r.data.next
    }
    return out
  }, [])
  if (loading) return <Loading />
  if (error) return <ErrorBox error={error} />
  return (
    <Card title="Tenants">
      <Table
        rows={data!}
        rowKey={(t) => t.name}
        columns={[
          ['Tenant', (t) => <Link className="font-semibold text-brand-strong hover:underline" to={`/server/tenants/${t.name}`}>{t.name}</Link>],
          ['Name', (t) => t.display_name ?? ''],
          ['State', (t) => <Badge>{t.state}</Badge>],
          ['Storage domain', (t) => t.storage_domain ?? ''],
          ['Created', (t) => when(t.created_at)],
        ]}
      />
    </Card>
  )
}

function ServerTenant({ client }: { client: Client }) {
  const { t = '' } = useParams()
  const info = useLoad(() => client.get<Tenant>(`/tenants/${seg(t)}`), [t])
  return (
    <TenantContext.Provider value={{ client, tenant: t, perms: new Perms(true), api: `/tenants/${seg(t)}`, base: `/server/tenants/${seg(t)}` }}>
      <div className="flex items-center gap-3">
        <h1 className="text-2xl font-bold">{t}</h1>
        {info.data && <Badge>{info.data.data.state}</Badge>}
      </div>
      <TenantArea />
    </TenantContext.Provider>
  )
}
