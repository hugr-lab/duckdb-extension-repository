import { type ReactNode, useEffect, useMemo, useRef, useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { LogIn } from 'lucide-react'
import { Button, Card, ErrorBox, Loading } from '../components/ui'
import { type ConsoleIssuer, NeedSignIn, Session, rememberedIssuer } from '../lib/auth'
import { EpochContext } from '../lib/context'
import { ApiError, Client } from '../lib/http'
import { type Scope, apiPrefix, scopePath } from '../lib/scope'
import { Shell } from './Shell'

const API = '/api/v1'

interface ServerConsole {
  environment: string
  admin_token_max_age: number
  audience: string
  issuers: ConsoleIssuer[]
}

const anonymous = new Client({ base: API, prefix: '/', getToken: () => Promise.reject(new NeedSignIn()) })

/** A scope's frame: its sign-in configuration, the issuer choice, the callback, the session and
 * its expiry banner. children render once signed in, with the scope's API client. */
export function ScopeRoot({ scope, children }: { scope: Exclude<Scope, { kind: 'landing' }>; children: (c: Client, s: Session, env: string) => ReactNode }) {
  const base = `/ui/${scopePath(scope)}`
  const loc = useLocation()
  const nav = useNavigate()
  const [conf, setConf] = useState<{ server: ServerConsole; issuers: ConsoleIssuer[] } | null>(null)
  const [failure, setFailure] = useState<unknown>(null)
  const [session, setSession] = useState<Session | null>(null)
  const [, rerender] = useState(0)
  const completing = useRef(false) // a callback's code is redeemed once (StrictMode runs effects twice)

  useEffect(() => {
    ;(async () => {
      const server = (await anonymous.get<ServerConsole>('/console', { anonymous: true })).data
      let issuers = server.issuers
      if (scope.kind === 'tenant') {
        issuers = (await anonymous.get<{ issuers: ConsoleIssuer[] }>(`/tenants/${scope.tenant}/console`, { anonymous: true })).data.issuers
      }
      setConf({ server, issuers })
    })().catch((e) => setFailure(e instanceof ApiError && e.status === 404 ? new Error('No such tenant.') : e))
  }, [base])

  // the redirect's callback: the issuer chosen before it, from sessionStorage
  const callback = loc.pathname === `/${scopePath(scope)}/callback` // the router's paths are under /ui
  useEffect(() => {
    if (!conf || !callback || session || completing.current) return
    completing.current = true
    const is = conf.issuers.find((i) => i.name === rememberedIssuer(base))
    if (!is) {
      setFailure(new Error('The sign-in was not started here; start again.'))
      return
    }
    const s = new Session(base, is, conf.server.admin_token_max_age)
    s.completeCallback().then(
      (to) => {
        if (to === null) return // a popup's: the opener has it, and this window closes
        setSession(s)
        nav(to.replace(/^\/ui/, ''), { replace: true })
      },
      (e) => setFailure(e),
    )
  }, [conf, callback])

  useEffect(() => session?.onChange(() => rerender((n) => n + 1)), [session])

  const client = useMemo(
    () => session && new Client({ base: API, prefix: apiPrefix(scope), getToken: (renew) => session.getToken(renew), onUnauthorized: () => session.expire() }),
    [session],
  )

  const where = scope.kind === 'server' ? 'Server administration' : `Tenant ${scope.tenant}`
  const env = conf?.server.environment ?? ''
  if (failure) {
    return (
      <Shell environment={env} where={where}>
        <ErrorBox error={failure} />
        <a href={`${base}/`} className="mt-4 inline-block font-semibold text-brand-strong hover:underline">Start again</a>
      </Shell>
    )
  }
  if (!conf || (callback && !session)) return <Shell environment={env} where={where}><Loading /></Shell>
  if (!session || !client) {
    return (
      <Shell environment={env} where={where}>
        <SignIn issuers={conf.issuers} onPick={(is) => {
          const s = new Session(base, is, conf.server.admin_token_max_age)
          s.signIn('/ui' + loc.pathname + loc.search).catch(setFailure)
        }} />
      </Shell>
    )
  }
  return (
    <Shell environment={env} where={where} user={session.subject} onSignOut={() => session.signOut().catch(setFailure)}>
      {session.expired && (
        <div role="alert" className="mb-4 flex items-center justify-between rounded-xl bg-warning-soft p-4 text-warning">
          <span>Your session has ended. Sign in again to go on; what you were editing is kept.</span>
          <Button tone="brand" onClick={() => session.signInPopup().catch(setFailure)}><LogIn className="h-4 w-4" /> Sign in</Button>
        </div>
      )}
      <EpochContext.Provider value={session.epoch}>{children(client, session, env)}</EpochContext.Provider>
    </Shell>
  )
}

/** The issuer choice: each shown with its IdP's host, and never a redirect without a click, so
 * this origin never sends anyone to an unknown login page unannounced. */
function SignIn({ issuers, onPick }: { issuers: ConsoleIssuer[]; onPick: (is: ConsoleIssuer) => void }) {
  if (issuers.length === 0) {
    return <Card title="Sign in"><p className="text-ink-muted">No sign-in is configured here. An administrator sets the console's client at an issuer.</p></Card>
  }
  return (
    <Card title="Sign in">
      <ul className="space-y-3">
        {issuers.map((is) => (
          <li key={is.name} className="flex items-center justify-between gap-4 rounded-xl border border-line p-4">
            <div>
              <div className="font-semibold">{is.name}</div>
              <div className="text-sm text-ink-muted">You will sign in at <span className="font-mono">{hostOf(is.issuer)}</span></div>
            </div>
            <Button tone="brand" onClick={() => onPick(is)}><LogIn className="h-4 w-4" /> Sign in</Button>
          </li>
        ))}
      </ul>
    </Card>
  )
}

export function hostOf(url: string) {
  try {
    return new URL(url).host
  } catch {
    return url
  }
}
