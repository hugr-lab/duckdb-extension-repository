import { Navigate, Route, Routes } from 'react-router-dom'
import { BarChart3, List, Package } from 'lucide-react'
import { useTenant } from '../lib/context'
import { useSections, type NavItem } from '../lib/frame'
import { Empty } from '../components/ui'
import { Channel, Extension } from './Channel'
import { Channels } from './Channels'
import { Events } from './Events'
import { Release } from './Release'
import { Stats } from './Stats'

/** A tenant's sections, for whatever the caller's grants allow; they go to the frame's sidebar
 * (standalone) or tabs (embedded). */
export function TenantArea({ label }: { label?: React.ReactNode }) {
  const { perms, client, api, base } = useTenant()
  const admin = perms.server || perms.grants.some((g) => g.verbs.includes('admin'))
  const tabs: NavItem[] = []
  if (admin) tabs.push({ to: `${base}/channels`, label: 'Channels', icon: Package })
  if (perms.canAudit) tabs.push({ to: `${base}/events`, label: 'Events', icon: List })
  if (admin || perms.canAudit) tabs.push({ to: `${base}/stats`, label: 'Statistics', icon: BarChart3 }) // the API scopes them to the caller's grants
  useSections('tenant', perms.anything ? { label, items: tabs } : null, `${base}|${tabs.map((t) => t.to).join(',')}`)
  if (!perms.anything) return <Empty>Nothing to administer here.</Empty>
  return (
    <Routes>
      <Route index element={<Navigate to={tabs[0]?.to ?? `${base}/events`} replace />} />
      <Route path="channels" element={<Channels />} />
      <Route path="channels/:c" element={<Channel />} />
      <Route path="channels/:c/extensions/:ext" element={<Extension />} />
      <Route path="channels/:c/releases/:ext/:id" element={<Release />} />
      <Route path="events" element={<Events key={api} client={client} path={`${api}/events`} title="Events" />} />
      <Route path="stats" element={<Stats />} />
      <Route path="*" element={<Empty>Not found.</Empty>} />
    </Routes>
  )
}
