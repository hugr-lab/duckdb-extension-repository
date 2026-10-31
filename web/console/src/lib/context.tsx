import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import type { Client } from './http'

export interface Grant {
  principal: string
  verbs: string[]
  channel?: string
  extension?: string
  /** spec 0017: a grant on an extension group */
  extension_group?: string
}

/** What a caller may administer in a tenant (from whoami's grants; a server administrator: all).
 * It only hides things: the API decides. */
export class Perms {
  constructor(
    readonly server: boolean,
    readonly grants: Grant[] = [],
    readonly audit = false,
  ) {}

  private admin(g: Grant) {
    return g.verbs.includes('admin')
  }
  get tenantAdmin() {
    return this.server || this.grants.some((g) => this.admin(g) && !g.channel && !g.extension && !g.extension_group)
  }
  channelAdmin(c: string) {
    return this.tenantAdmin || this.grants.some((g) => this.admin(g) && g.channel === c && !g.extension && !g.extension_group)
  }
  /** The extension groups the caller administers by a grant on them (spec 0017: it reads them). */
  get groups(): string[] {
    return [...new Set(this.grants.filter((g) => this.admin(g) && g.extension_group).map((g) => g.extension_group!))].sort()
  }
  /** The extensions of a channel the caller administers by an extension grant. */
  extensionsIn(c: string): string[] {
    const out = new Set<string>()
    for (const g of this.grants) {
      if (this.admin(g) && g.extension && (!g.channel || g.channel === c)) out.add(g.extension)
    }
    return [...out].sort()
  }
  /** Whether the caller administers an extension in every channel (a tenant administrator, or a
   * grant on the extension without a channel): it manages the extension's documents (spec 0011). */
  extensionAdmin(ext: string) {
    return this.tenantAdmin || this.grants.some((g) => this.admin(g) && g.extension === ext && !g.channel)
  }
  /** Whether the caller may place a version of an extension in a channel (spec 0033): promote on it,
   * which admin does not imply (spec 0008). A grant on a group may cover it (its members are not
   * known here), and a reserved name needs a grant naming it: the API decides. */
  mayPromote(c: string, ext: string) {
    return this.server || this.grants.some((g) => g.verbs.includes('promote') && (!g.channel || g.channel === c) && (!g.extension || g.extension === ext))
  }
  /** Whether the caller may publish into the catalogue (spec 0033): publish tenant-wide, that is a
   * grant without a channel (on the name, a group, or every name); the API decides which names. */
  get mayPublishCatalogue() {
    return this.server || this.grants.some((g) => g.verbs.includes('publish') && !g.channel)
  }
  /** Whether the caller administers anything in a channel. */
  anyIn(c: string) {
    return this.channelAdmin(c) || this.extensionsIn(c).length > 0
  }
  get canAudit() {
    return this.server || this.audit || this.tenantAdmin
  }
  get anything() {
    return this.server || this.audit || this.grants.some((g) => this.admin(g))
  }
}

export interface TenantCtx {
  client: Client
  tenant: string
  perms: Perms
  /** The API path of the tenant, "/tenants/acme". */
  api: string
  /** The console's path of the tenant under /ui, "/t/acme" or "/server/tenants/acme". */
  base: string
}

export const TenantContext = createContext<TenantCtx | null>(null)

/** The session's sign-in count: a load that failed loads again after a new sign-in. */
export const EpochContext = createContext(0)

export function useTenant(): TenantCtx {
  const c = useContext(TenantContext)
  if (!c) throw new Error('no tenant context')
  return c
}

/** Loads with f when deps change; reload() loads again. */
export function useLoad<T>(f: () => Promise<T>, deps: unknown[]) {
  const [state, setState] = useState<{ data?: T; error?: unknown; loading: boolean }>({ loading: true })
  const [n, setN] = useState(0)
  const epoch = useContext(EpochContext)
  const failed = useRef(false)
  failed.current = state.error !== undefined
  useEffect(() => {
    if (failed.current) setN((x) => x + 1)
  }, [epoch])
  useEffect(() => {
    let alive = true
    setState((s) => ({ ...s, loading: true }))
    f().then(
      (data) => alive && setState({ data, loading: false }),
      (error) => alive && setState({ error, loading: false }),
    )
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, n])
  const reload = useCallback(() => setN((x) => x + 1), [])
  return { ...state, reload }
}

export interface Page<T> {
  items: T[]
  next?: string
}

/** A list read page by page from the API's cursors (spec 0015 phase 1b): nothing is loaded ahead,
 * and "previous" replays the cursors seen. A change of deps starts again from the first page. */
export function usePaged<T>(fetchPage: (cursor: string, limit: number) => Promise<Page<T>>, deps: unknown[]) {
  const key = JSON.stringify(deps)
  const [size, setSizeState] = useState(50)
  const [at, setAt] = useState<{ key: string; cursors: string[] }>({ key, cursors: [''] })
  if (at.key !== key) setAt({ key, cursors: [''] }) // other filters: the first page, never cursors kept from before
  const cursors = at.key === key ? at.cursors : ['']
  const page = cursors.length - 1
  const load = useLoad(() => fetchPage(cursors[page], size), [key, size, cursors.join('\n')])
  const next = load.data?.next
  return {
    ...load,
    items: load.data?.items ?? [],
    page,
    size,
    hasNext: !!next && !load.loading,
    next: () => next && setAt({ key, cursors: [...cursors, next] }),
    prev: () => page > 0 && setAt({ key, cursors: cursors.slice(0, -1) }),
    setSize: (n: number) => {
      setSizeState(n)
      setAt({ key, cursors: [''] })
    },
  }
}
