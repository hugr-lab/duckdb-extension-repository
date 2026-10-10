import { type ReactNode, useEffect, useMemo, useRef, useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { LogIn } from 'lucide-react'
import { Button, ErrorBox, Loading } from '../components/ui'
import { type ConsoleIssuer, NeedSignIn, Session, rememberedIssuer } from '../lib/auth'
import { EpochContext } from '../lib/context'
import { ApiError, Client } from '../lib/http'
import { type Scope, apiPrefix, scopePath } from '../lib/scope'
import { EntryCard, Shell } from './Shell'

const API = '/api/v1'

export interface ServerConsole {
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
  const [conf, setConf] = useState<{ server: ServerConsole; issuers: ConsoleIssuer[]; version: string } | null>(null)
  const [failure, setFailure] = useState<unknown>(null)
  const [session, setSession] = useState<Session | null>(null)
  const [, rerender] = useState(0)
  const completing = useRef(false) // a callback's code is redeemed once (StrictMode runs effects twice)

  useEffect(() => {
    ;(async () => {
      const [server, info] = await Promise.all([
        anonymous.get<ServerConsole>('/console', { anonymous: true }).then((r) => r.data),
        anonymous.get<{ kista: string }>('/info', { anonymous: true }).then((r) => r.data.kista, () => ''),
      ])
      let issuers = server.issuers
      if (scope.kind === 'tenant') {
        issuers = (await anonymous.get<{ issuers: ConsoleIssuer[] }>(`/tenants/${scope.tenant}/console`, { anonymous: true })).data.issuers
      }
      setConf({ server, issuers, version: info })
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

  const env = conf?.server.environment ?? ''
  const title = scope.kind === 'server' ? 'Server administration' : <>Tenant <span className="font-mono">{scope.tenant}</span></>
  if (failure && !session) {
    return (
      <EntryCard environment={env}>
        <h1 className="m-0 text-[22px] font-bold leading-8">{title}</h1>
        <ErrorBox error={failure} />
        <a href={`${base}/`} className="font-semibold text-brand-strong hover:underline">Start again</a>
      </EntryCard>
    )
  }
  if (!conf || (callback && !session)) {
    return (
      <EntryCard environment={env}>
        <Loading />
      </EntryCard>
    )
  }
  if (!session || !client) {
    return (
      <EntryCard environment={env}>
        <SignIn title={title} host={location.host} issuers={conf.issuers} onPick={(is) => {
          const s = new Session(base, is, conf.server.admin_token_max_age)
          s.signIn('/ui' + loc.pathname + loc.search).catch(setFailure)
        }} />
      </EntryCard>
    )
  }
  return (
    <Shell environment={env} scope={scope.kind === 'server' ? 'Server' : title} user={session.subject}
      role={scope.kind === 'server' ? 'Server administrator' : undefined} service={{ host: location.host, version: conf.version }}
      onSignOut={() => session.signOut().catch(setFailure)}>
      {failure != null && <ErrorBox error={failure} />}
      {session.expired && (
        <div role="alert" className="flex items-center gap-3 rounded-md bg-warning-soft px-3.5 py-2.5 text-warning">
          <span>Your session has ended. Sign in again to go on; what you were editing is kept.</span>
          <Button tone="brand" onClick={() => session.signInPopup().catch(setFailure)}><LogIn size={16} aria-hidden /> Sign in</Button>
        </div>
      )}
      <EpochContext.Provider value={session.epoch}>{children(client, session, env)}</EpochContext.Provider>
    </Shell>
  )
}

/** The issuer choice: each shown with its IdP's host, and never a redirect without a click, so
 * this origin never sends anyone to an unknown login page unannounced. */
function SignIn({ title, host, issuers, onPick }: { title: ReactNode; host: string; issuers: ConsoleIssuer[]; onPick: (is: ConsoleIssuer) => void }) {
  return (
    <>
      <div className="flex flex-col gap-1.5">
        <span className="eyebrow">kista console</span>
        <h1 className="m-0 text-[26px] font-bold leading-[34px] tracking-[-0.01em]">{title}</h1>
        <span className="font-mono text-[13px] text-ink-muted">{host}</span>
      </div>
      {issuers.length === 0 ? (
        <span className="text-ink-muted">No sign-in is configured here. An administrator sets the console's client at an issuer.</span>
      ) : (
        <div className="flex flex-col gap-2.5">
          {issuers.map((is, i) => (
            <div key={is.name} className="flex flex-col gap-1">
              <Button tone={i === 0 ? 'brand' : 'outline'} wide onClick={() => onPick(is)}>
                <LogIn size={18} aria-hidden /> Sign in with {is.name}
              </Button>
              <span className="text-center text-[12px] text-ink-muted">You will sign in at <span className="font-mono">{hostOf(is.issuer)}</span></span>
            </div>
          ))}
        </div>
      )}
      <span className="text-[13px] text-ink-muted">You leave kista only when you click, at the host shown. <a href="/ui/" className="text-brand-strong">Another tenant</a></span>
    </>
  )
}

export function hostOf(url: string) {
  try {
    return new URL(url).host
  } catch {
    return url
  }
}
