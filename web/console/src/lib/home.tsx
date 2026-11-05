// The reads behind a tenant administrator's home (spec 0041): the catalogue, each signed channel's
// rules, follows and hold, and the decisions, read once for the matrix, the queue and the
// navigation's count, and read again after a change.
import { createContext, useContext, useMemo, type ReactNode } from 'react'
import type { CatalogueEntry } from '../screens/Catalogue'
import type { FollowRow, SourceRow } from '../screens/Sources'
import { useLoad, useTenant } from './context'
import { type Client, seg } from './http'
import { type ChannelReads, type Decision, type HomeReads, type Item, queue } from './queue'

interface ChannelRow {
  name: string
  kind: string
  package_type?: string
}

/** Every decision of the tenant: the API's pages followed to the end. */
export async function allDecisions(client: Client, api: string): Promise<Decision[]> {
  const out: Decision[] = []
  let cursor = ''
  for (;;) {
    const q = new URLSearchParams({ limit: '500' })
    if (cursor) q.set('cursor', cursor)
    const r = await client.get<{ decisions: Decision[]; next?: string }>(`${api}/decisions?${q}`)
    out.push(...r.data.decisions)
    if (!r.data.next || r.data.next === cursor) return out
    cursor = r.data.next
  }
}

/** The home's reads: the signed channels of the default package type (other types through the API,
 * as the Catalogue section does), one read of each's rules, follows and hold. */
export async function readHome(client: Client, api: string): Promise<HomeReads> {
  const [cat, chs, decisions] = await Promise.all([
    client.get<{ entries: CatalogueEntry[] }>(`${api}/catalogue`),
    client.get<{ channels: ChannelRow[] }>(`${api}/channels`),
    allDecisions(client, api),
  ])
  const signed = chs.data.channels.filter((c) => c.kind === 'signed' && !c.package_type).map((c) => c.name).sort()
  const channels = await Promise.all(signed.map(async (name): Promise<ChannelReads> => {
    const path = `${api}/channels/${seg(name)}`
    const [sources, fs, hold] = await Promise.all([
      client.get<{ sources: SourceRow[] }>(`${path}/sources`),
      client.get<{ follows: FollowRow[] }>(`${path}/follows`),
      client.get<{ hold: string }>(`${path}/hold`),
    ])
    return { name, hold: hold.data.hold, follows: fs.data.follows, sources: sources.data.sources }
  }))
  return { entries: cat.data.entries, channels, decisions }
}

export interface Home {
  reads?: HomeReads
  error?: unknown
  loading: boolean
  reload: () => void
  /** The decision queue: what waits for an administrator. */
  items: Item[]
}

const HomeContext = createContext<Home | null>(null)

/** The home's reads for a tenant administrator's pages. */
export function HomeProvider({ children }: { children: ReactNode }) {
  const { client, api } = useTenant()
  const load = useLoad(() => readHome(client, api), [api])
  const items = useMemo(() => (load.data ? queue(load.data) : []), [load.data])
  const value = useMemo<Home>(() => ({ reads: load.data, error: load.error, loading: load.loading, reload: load.reload, items }),
    [load.data, load.error, load.loading, load.reload, items])
  return <HomeContext.Provider value={value}>{children}</HomeContext.Provider>
}

/** The home's reads; null outside a tenant administrator's pages. */
export function useHome(): Home | null {
  return useContext(HomeContext)
}
