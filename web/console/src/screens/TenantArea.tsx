import { NavLink, Navigate, Route, Routes } from 'react-router-dom'
import { useTenant } from '../lib/context'
import { Empty } from '../components/ui'
import { Channel } from './Channel'
import { Channels } from './Channels'
import { Events } from './Events'
import { Release } from './Release'
import { Stats } from './Stats'

/** A tenant's sections, for whatever the caller's grants allow. */
export function TenantArea() {
  const { perms, client, api, tenant, base } = useTenant()
  if (!perms.anything) return <Empty>Nothing to administer here.</Empty>
  const admin = perms.server || perms.grants.some((g) => g.verbs.includes('admin'))
  const tabs: [string, string][] = []
  if (admin) tabs.push(['channels', 'Channels'])
  if (perms.canAudit) tabs.push(['events', 'Events'])
  if (admin || perms.canAudit) tabs.push(['stats', 'Statistics']) // the API scopes them to the caller's grants
  return (
    <div className="space-y-4">
      <nav className="flex gap-2" aria-label={`${tenant} sections`}>
        {tabs.map(([to, label]) => (
          <NavLink key={to} to={`${base}/${to}`} className={({ isActive }) => `rounded-full px-4 py-1.5 text-sm font-semibold ${isActive ? 'bg-brand text-brand-on' : 'text-ink hover:bg-row'}`}>
            {label}
          </NavLink>
        ))}
      </nav>
      <Routes>
        <Route index element={<Navigate to={`${base}/${tabs[0]?.[0] ?? 'events'}`} replace />} />
        <Route path="channels" element={<Channels />} />
        <Route path="channels/:c" element={<Channel />} />
        <Route path="channels/:c/releases/:ext/:id" element={<Release />} />
        <Route path="events" element={<Events key={api} client={client} path={`${api}/events`} title="Events" />} />
        <Route path="stats" element={<Stats />} />
        <Route path="*" element={<Empty>Not found.</Empty>} />
      </Routes>
    </div>
  )
}
