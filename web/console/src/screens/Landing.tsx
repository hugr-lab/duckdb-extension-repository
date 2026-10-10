import { useEffect, useState } from 'react'
import { Shield } from 'lucide-react'
import { Button } from '../components/ui'
import { validTenant } from '../lib/scope'
import { EntryCard } from './Shell'

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
    <EntryCard environment={server?.environment}>
      <div className="flex flex-col gap-1.5">
        <span className="eyebrow">kista console</span>
        <h1 className="m-0 text-[26px] font-bold leading-[34px] tracking-[-0.01em]">Sign in to manage extensions</h1>
        <span className="font-mono text-[13px] text-ink-muted">{location.host}</span>
      </div>
      <form className="flex flex-col gap-2" onSubmit={(e) => { e.preventDefault(); go() }}>
        <span className="text-[13px] font-semibold">Tenant administration</span>
        <div className="flex gap-2">
          <input aria-label="tenant" placeholder="tenant name" className="field min-w-0 flex-1 font-mono" value={tenant}
            onChange={(e) => setTenant(e.target.value.trim().toLowerCase())} />
          <Button type="submit" tone="brand" disabled={!validTenant(tenant)}>Continue</Button>
        </div>
      </form>
      {server && server.issuers.length > 0 && (
        <div className="flex flex-col gap-2 border-t border-line pt-5">
          <span className="text-[13px] font-semibold">Server administration</span>
          <a href="/ui/server/" className="inline-flex items-center gap-2 font-semibold text-brand-strong no-underline hover:underline">
            <Shield size={16} aria-hidden /> Sign in as a server administrator
          </a>
        </div>
      )}
    </EntryCard>
  )
}
