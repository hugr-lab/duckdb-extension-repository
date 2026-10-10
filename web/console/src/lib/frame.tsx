// The console's frame (spec 0015 phase 1b): the sections a screen offers go to the standalone
// sidebar, or to tabs above the content when a host's shell holds the console.
import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import type { LucideIcon } from 'lucide-react'

export interface NavItem {
  to: string
  label: string
  icon: LucideIcon
  /** Active on this path only, not below it. */
  end?: boolean
}

export interface NavGroup {
  /** A heading above the items ("Tenant acme"); none: no heading. */
  label?: ReactNode
  items: NavItem[]
}

/** Sections by the screen that offers them; the frame shows them in this order. */
const order = ['server', 'tenant']

interface Frame {
  embedded: boolean
  groups: NavGroup[]
  setGroup: (key: string, g: NavGroup | null) => void
}

const FrameContext = createContext<Frame>({ embedded: false, groups: [], setGroup: () => {} })

export function FrameProvider({ embedded, children }: { embedded: boolean; children: ReactNode }) {
  const [byKey, setByKey] = useState<Record<string, NavGroup>>({})
  const value = useMemo<Frame>(
    () => ({
      embedded,
      groups: order.flatMap((k) => (byKey[k] ? [byKey[k]] : [])),
      setGroup: (key, g) =>
        setByKey((m) => {
          const next = { ...m }
          if (g) next[key] = g
          else delete next[key]
          return next
        }),
    }),
    [embedded, byKey],
  )
  return <FrameContext.Provider value={value}>{children}</FrameContext.Provider>
}

export function useFrame() {
  return useContext(FrameContext)
}

/** Offers a screen's sections to the frame while it is shown. sig names the group's content, so
 * an equal group is not offered again. */
export function useSections(key: string, group: NavGroup | null, sig: string) {
  const { setGroup } = useFrame()
  useEffect(() => {
    setGroup(key, group)
    return () => setGroup(key, null)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, sig])
}
