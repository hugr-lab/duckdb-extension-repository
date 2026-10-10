import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import type { Client } from './http'

export interface Grant {
  principal: string
  verbs: string[]
  channel?: string
  extension?: string
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
    return this.server || this.grants.some((g) => this.admin(g) && !g.channel && !g.extension)
  }
  channelAdmin(c: string) {
    return this.tenantAdmin || this.grants.some((g) => this.admin(g) && g.channel === c && !g.extension)
  }
  /** The extensions of a channel the caller administers by an extension grant. */
  extensionsIn(c: string): string[] {
    const out = new Set<string>()
    for (const g of this.grants) {
      if (this.admin(g) && g.extension && (!g.channel || g.channel === c)) out.add(g.extension)
    }
    return [...out].sort()
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
