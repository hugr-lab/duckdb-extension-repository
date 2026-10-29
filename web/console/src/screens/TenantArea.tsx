import { Navigate, Route, Routes } from 'react-router-dom'
import { Ban, BarChart3, Boxes, CloudDownload, Fingerprint, Gauge, KeyRound, Library, List, Package, Send, ShieldCheck } from 'lucide-react'
import { type Perms, useTenant } from '../lib/context'
import { useSections, type NavItem } from '../lib/frame'
import { Empty } from '../components/ui'
import { Access } from './Access'
import { Blocks } from './Blocks'
import { Catalogue, CatalogueEntryPage } from './Catalogue'
import { Channel, Extension } from './Channel'
import { Channels } from './Channels'
import { Events } from './Events'
import { GroupPage, Groups } from './Groups'
import { Identity } from './Identity'
import { Licenses } from './Licenses'
import { Limits } from './Limits'
import { PublisherPage, Publishers } from './Publishers'
import { UpstreamPage, Upstreams } from './Upstreams'
import { Release } from './Release'
import { Stats } from './Stats'

/** A tenant's sections, for whatever the caller's grants allow; they go to the frame's sidebar
 * (standalone) or tabs (embedded). */
export function TenantArea({ label }: { label?: React.ReactNode }) {
  const { perms, client, api, base } = useTenant()
  const tabs = tenantSections(perms, base)
  useSections('tenant', perms.anything ? { label, items: tabs } : null, `${base}|${tabs.map((t) => t.to).join(',')}`)
  if (!perms.anything) return <Empty>Nothing to administer here.</Empty>
  return (
    <Routes>
      <Route index element={<Navigate to={tabs[0]?.to ?? `${base}/events`} replace />} />
      <Route path="channels" element={<Channels />} />
      <Route path="channels/:c" element={<Channel />} />
      <Route path="channels/:c/extensions/:ext" element={<Extension />} />
      <Route path="channels/:c/releases/:ext/:id" element={<Release />} />
      <Route path="catalogue" element={<Catalogue />} />
      <Route path="catalogue/:source/:name" element={<CatalogueEntryPage />} />
      <Route path="upstreams" element={<Upstreams />} />
      <Route path="upstreams/:name" element={<UpstreamPage />} />
      <Route path="blocks" element={<Blocks />} />
      <Route path="licenses" element={<Licenses />} />
      <Route path="limits" element={<Limits />} />
      <Route path="groups" element={<Groups />} />
      <Route path="groups/:g" element={<GroupPage />} />
      <Route path="access" element={<Access />} />
      <Route path="identity" element={<Identity />} />
      <Route path="publishers" element={<Publishers />} />
      <Route path="publishers/:name" element={<PublisherPage />} />
      <Route path="events" element={<Events key={api} client={client} path={`${api}/events`} title="Events" />} />
      <Route path="stats" element={<Stats />} />
      <Route path="*" element={<Empty>Not found.</Empty>} />
    </Routes>
  )
}

/** The sections a caller's grants show in a tenant (the API decides what each answers). */
export function tenantSections(perms: Perms, base: string): NavItem[] {
  const admin = perms.server || perms.grants.some((g) => g.verbs.includes('admin'))
  const tabs: NavItem[] = []
  if (admin) tabs.push({ to: `${base}/channels`, label: 'Channels', icon: Package })
  // spec 0015 phases 2a, 2b: a group's administrators read their groups; the rest is a tenant administrator's
  if (perms.tenantAdmin) {
    tabs.push({ to: `${base}/catalogue`, label: 'Catalogue', icon: Library }) // spec 0033: the tenant's administrators read it
    tabs.push({ to: `${base}/upstreams`, label: 'Upstreams', icon: CloudDownload })
  }
  if (perms.tenantAdmin || perms.groups.length > 0) tabs.push({ to: `${base}/groups`, label: 'Groups', icon: Boxes })
  if (perms.tenantAdmin) {
    tabs.push({ to: `${base}/access`, label: 'Access', icon: ShieldCheck })
    tabs.push({ to: `${base}/identity`, label: 'Identity', icon: Fingerprint })
    tabs.push({ to: `${base}/publishers`, label: 'Publishers', icon: Send })
    tabs.push({ to: `${base}/blocks`, label: 'Blocks', icon: Ban })
    tabs.push({ to: `${base}/licenses`, label: 'Licences', icon: KeyRound })
    tabs.push({ to: `${base}/limits`, label: 'Limits', icon: Gauge }) // spec 0038: read only
  }
  if (perms.canAudit) tabs.push({ to: `${base}/events`, label: 'Events', icon: List })
  if (admin || perms.canAudit) tabs.push({ to: `${base}/stats`, label: 'Statistics', icon: BarChart3 }) // the API scopes them to the caller's grants
  return tabs
}
