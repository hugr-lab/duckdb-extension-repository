import { useEffect, useState } from 'react'
import { Shield } from 'lucide-react'
import { Button, Card } from '../components/ui'
import { validTenant } from '../lib/scope'
import { Shell } from './Shell'

const lastTenant = 'kista.last-tenant'

/** Where to sign in: a tenant (remembered, a convenience only) or the server's administration.
 * Each scope is its own page: these are full page loads. */
export function Landing() {
  const [tenant, setTenant] = useState(() => {
    try {
      return localStorage.getItem(lastTenant) ?? ''
    } catch {
      return ''
    }
  })
  const [server, setServer] = useState<{ environment: string; issuers: unknown[] } | null>(null)
  useEffect(() => {
    fetch('/api/v1/console', { credentials: 'omit', cache: 'no-store' })
      .then((r) => (r.ok ? r.json() : null))
      .then(setServer, () => setServer(null))
  }, [])
  const go = () => {
    if (!validTenant(tenant)) return
    try {
      localStorage.setItem(lastTenant, tenant)
    } catch {
      /* storage refused */
    }
    location.assign(`/ui/t/${tenant}/`)
  }
  return (
    <Shell environment={server?.environment}>
      <div className="mx-auto max-w-lg space-y-4 pt-10">
        <Card title="Tenant administration">
          <form className="flex gap-2" onSubmit={(e) => { e.preventDefault(); go() }}>
            <input aria-label="tenant" placeholder="tenant name" className="flex-1 rounded-full border border-line bg-surface px-4 py-1.5" value={tenant}
              onChange={(e) => setTenant(e.target.value.trim().toLowerCase())} />
            <Button type="submit" tone="brand" disabled={!validTenant(tenant)}>Continue</Button>
          </form>
        </Card>
        {server && server.issuers.length > 0 && (
          <Card title="Server administration">
            <a href="/ui/server/" className="inline-flex items-center gap-2 font-semibold text-brand-strong hover:underline">
              <Shield className="h-4 w-4" /> Sign in as a server administrator
            </a>
          </Card>
        )}
      </div>
    </Shell>
  )
}
