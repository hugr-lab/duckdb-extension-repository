// The console's scopes (spec 0015), by path: the landing, the server's, a tenant's. Each is its
// own document: moving between them is a full page load, so no token outlives its scope.

export type Scope = { kind: 'landing' } | { kind: 'server' } | { kind: 'tenant'; tenant: string }

const tenantName = /^[a-z0-9][a-z0-9-]{0,62}$/

/** The scope of a path under the console's root ("/ui"). */
export function scopeOf(path: string, root = '/ui'): Scope {
  const rest = path.startsWith(root + '/') ? path.slice(root.length + 1) : ''
  const [first, second] = rest.split('/')
  if (first === 'server') return { kind: 'server' }
  if (first === 't' && second && tenantName.test(second)) return { kind: 'tenant', tenant: second }
  return { kind: 'landing' }
}

/** The path a scope's pages live under, relative to the root ("server", "t/acme"). */
export function scopePath(s: Scope): string {
  switch (s.kind) {
    case 'server':
      return 'server'
    case 'tenant':
      return `t/${s.tenant}`
    default:
      return ''
  }
}

/** The API paths a scope's token may be sent to. */
export function apiPrefix(s: Scope): string {
  return s.kind === 'tenant' ? `/tenants/${s.tenant}/` : '/'
}

export const validTenant = (s: string) => tenantName.test(s)
