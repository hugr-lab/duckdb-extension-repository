// The console as a micro-frontend (spec 0015 phase 1b, kista's contract version 1): mountKista and
// <kista-console>. The host owns sign-in, navigation and the theme; the console renders in the
// element's shadow root with its own styles, and asks the host for a token per audience.
import { StrictMode, useEffect, useMemo, useRef, useState } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { MemoryRouter, Route, Routes, useLocation, useNavigate, useNavigationType } from 'react-router-dom'
import css from './styles.css?inline'
import { Button, Empty, ErrorBox, Loading } from './components/ui'
import { EpochContext } from './lib/context'
import { FrameProvider } from './lib/frame'
import { ApiError, Client } from './lib/http'
import { validTenant } from './lib/scope'
import { claims, renewAt } from './lib/token'
import { Server } from './screens/Server'
import { Shell } from './screens/Shell'
import { Tenant } from './screens/Tenant'

/** The contract this module implements: an addition keeps it, a breaking change raises it. */
export const contract = 1

/** The languages the console speaks; another falls back to the first (reserved: English only). */
export const locales = ['en'] as const

export type Theme = 'light' | 'dark'

export interface KistaOptions {
  /** kista's origin, or the shell's proxy prefix for it, without a trailing slash. */
  apiBase: string
  /** A token carrying the audience; with renew, a newly issued one (a fresh iat), not the cached one. */
  getToken: (audience: string, how?: { renew?: boolean }) => Promise<string>
  /** A tenant's scope; absent: the server's. */
  tenant?: string
  /** The audience to ask for, instead of the one kista names. */
  audience?: string
  /** The host's path the console lives under; the console's own paths follow it. */
  basePath?: string
  theme?: Theme
  locale?: string
  /** The console moved: the host's full path and query (replace: the entry is replaced). Without
   * it, the console keeps the browser's history itself. */
  onNavigate?: (path: string, how: { replace: boolean }) => void
  /** The section shown, for the host's title or breadcrumbs. */
  onTitle?: (title: string) => void
  /** The API refused the token even renewed: the host signs the user in again (at most once per
   * 30 seconds per element). */
  onUnauthorized?: () => void
}

export interface KistaHandle {
  /** A new theme or language, a host path (its back button), another tenant (a remount). */
  update(o: { theme?: Theme; path?: string; locale?: string; tenant?: string }): void
  unmount(): void
}

/** getToken rejected: the host has no token for the audience. Never an unauthorized session. */
export class NoToken extends Error {
  constructor(readonly audience: string) {
    super(`The platform has no token for kista's audience ${audience}.`)
  }
}

const unauthorizedEvery = 30_000
const told = new WeakMap<HTMLElement, number>()
const mounted = new WeakSet<HTMLElement>()

const trim = (p: string) => p.replace(/\/+$/, '')

/** The console's own path (and query) from the host's: below basePath, else the default. */
export function innerPath(basePath: string, hostPath: string, fallback: string): string {
  const base = trim(basePath)
  const q = hostPath.indexOf('?')
  const [p, query] = q < 0 ? [hostPath, ''] : [hostPath.slice(0, q), hostPath.slice(q)]
  if (base && p !== base && !p.startsWith(base + '/')) return fallback
  const own = p.slice(base.length)
  return own && own !== '/' ? own + query : fallback
}

const titles: [RegExp, string][] = [
  [/^\/tenants\/[^/]+\/channels/, 'Channels'], [/^\/tenants\/[^/]+\/events/, 'Events'], [/^\/tenants\/[^/]+\/stats/, 'Statistics'],
  [/^\/tenants/, 'Tenants'], [/^\/events/, 'Events'], [/^\/channels/, 'Channels'], [/^\/stats/, 'Statistics'],
]

// --- styles: constructed sheets in the shadow root, so a host's CSP needs no 'unsafe-inline' ---

let sheet: CSSStyleSheet | undefined
const fontsAdded = new Set<string>()

// The build names the fonts by new URL(…, import.meta.url): absolute, so they resolve against this
// module, not the host's document.
function style(root: ShadowRoot): () => void {
  const full = css
  const faces = full.match(/@font-face\s*{[^}]*}/g) ?? []
  // @font-face in a shadow root is not used: the faces go to the document, once per family, and
  // a family the page has already (tresor's console loads Manrope too) is not loaded twice
  const have = new Set<string>()
  try {
    document.fonts?.forEach((f) => have.add(f.family.replace(/["']/g, '')))
  } catch {
    /* no FontFaceSet */
  }
  const add = faces.filter((f) => {
    const fam = /font-family:\s*['"]?([^;'"]+)/.exec(f)?.[1] ?? ''
    return !have.has(fam) && !fontsAdded.has(fam)
  })
  add.forEach((f) => fontsAdded.add(/font-family:\s*['"]?([^;'"]+)/.exec(f)?.[1] ?? ''))
  if (typeof CSSStyleSheet !== 'undefined' && 'replaceSync' in CSSStyleSheet.prototype && 'adoptedStyleSheets' in root) {
    sheet ??= (() => {
      const s = new CSSStyleSheet()
      s.replaceSync(full)
      return s
    })()
    const own = sheet
    root.adoptedStyleSheets = [...root.adoptedStyleSheets.filter((s) => s !== own), own]
    if (add.length) {
      const f = new CSSStyleSheet()
      f.replaceSync(add.join('\n'))
      document.adoptedStyleSheets = [...document.adoptedStyleSheets, f]
    }
    return () => {
      root.adoptedStyleSheets = root.adoptedStyleSheets.filter((s) => s !== own)
    }
  }
  // a browser without constructed sheets (and jsdom): a <style>, which a strict CSP refuses
  const el = document.createElement('style')
  el.textContent = full
  root.appendChild(el)
  return () => el.remove()
}

// --- the console inside the host ---

interface Live {
  theme: Theme
  tenant?: string
  path?: { to: string; n: number }
  generation: number
}

interface Conf {
  maxAge: number
  audience: string
}

function Embedded({ o, live, element }: { o: KistaOptions; live: Live; element: HTMLElement }) {
  const tenant = live.tenant
  const [conf, setConf] = useState<Conf | null>(null)
  const [failure, setFailure] = useState<unknown>(null)
  const [ended, setEnded] = useState(false)
  const [epoch, setEpoch] = useState(0)
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])
  const api = `${trim(o.apiBase)}/api/v1`

  useEffect(() => {
    const get = async <T,>(path: string): Promise<T> => {
      const r = await fetch(api + path, { credentials: 'omit', cache: 'no-store', headers: { Accept: 'application/json' } })
      if (!r.ok) throw new ApiError(r.status, r.status === 404 ? 'No such tenant.' : `${r.status}`)
      return r.json()
    }
    ;(async () => {
      if (tenant !== undefined && !validTenant(tenant)) throw new ApiError(404, 'No such tenant.')
      const server = await get<{ admin_token_max_age: number; audience: string }>('/console')
      let audience = server.audience
      if (tenant !== undefined) {
        const t = await get<{ audience: string; issuers: { audience: string }[] }>(`/tenants/${tenant}/console`)
        audience = t.issuers[0]?.audience ?? t.audience
      }
      if (alive.current) setConf({ maxAge: server.admin_token_max_age, audience: o.audience || audience })
    })().catch((e) => alive.current && setFailure(e))
  }, [api, tenant, o.audience])

  const client = useMemo(() => {
    if (!conf) return null
    const ask0 = (renew: boolean) =>
      o.getToken(conf.audience, renew ? { renew: true } : undefined).then(
        (t) => {
          if (typeof t !== 'string' || !t) throw new NoToken(conf.audience)
          return t
        },
        () => {
          throw new NoToken(conf.audience)
        },
      )
    // one renewal at a time, shared by the requests that need it (a host's forced refresh is costly)
    let renewing: Promise<string> | null = null
    const ask = (renew: boolean) => {
      if (!renew) return ask0(false)
      renewing ??= ask0(true).finally(() => {
        renewing = null
      })
      return renewing
    }
    return new Client({
      base: api,
      prefix: tenant !== undefined ? `/tenants/${tenant}/` : '/',
      getToken: async (renew) => {
        const t = await ask(renew)
        // management wants iat within admin_token_max_age (spec 0007): a newly issued one before then
        if (!renew && Date.now() >= renewAt(claims(t).iat, undefined, conf.maxAge)) return ask(true)
        return t
      },
      unauthorizedEvery: 0, // the banner shows every time; the host is told at most once per 30 s, below
      onUnauthorized: () => {
        if (!alive.current) return
        setEnded(true)
        const now = Date.now()
        if (now - (told.get(element) ?? -Infinity) >= unauthorizedEvery) {
          told.set(element, now)
          o.onUnauthorized?.()
        }
      },
    })
  }, [conf, api, tenant, o, element])

  if (failure) return failure instanceof ApiError && failure.status === 404 ? <Empty>No such tenant.</Empty> : <ErrorBox error={failure} />
  if (!client) return <Loading />
  return (
    <Shell>
      {ended && (
        <div role="alert" className="flex items-center gap-3 rounded-md bg-warning-soft px-3.5 py-2.5 text-warning">
          <span>Your session ended: sign in again in the platform. What you were editing is kept.</span>
          <Button tone="outline" onClick={() => { setEnded(false); setEpoch((n) => n + 1) }}>Try again</Button>
        </div>
      )}
      <EpochContext.Provider value={epoch}>
        <Sync o={o} live={live} fallback={tenant !== undefined ? '/channels' : '/tenants'} />
        <Routes>
          <Route path="/*" element={tenant !== undefined ? <Tenant client={client} tenant={tenant} root="" /> : <Server client={client} root="" />} />
        </Routes>
      </EpochContext.Provider>
    </Shell>
  )
}

/** Keeps the host's address and the console's route together, both ways. */
function Sync({ o, live, fallback }: { o: KistaOptions; live: Live; fallback: string }) {
  const navigate = useNavigate()
  const go = useRef(navigate) // useNavigate's function changes with the location
  go.current = navigate
  const how = useNavigationType()
  const loc = useLocation()
  const base = trim(o.basePath ?? '')

  // the host navigated (its back button): follow, without telling it again
  useEffect(() => {
    if (live.path) go.current(innerPath(base, live.path.to, fallback), { replace: true, state: { fromHost: true } })
  }, [live.path, base, fallback])

  // the console navigated: the host's address and title follow
  useEffect(() => {
    const full = base + loc.pathname + loc.search
    if (!(loc.state as { fromHost?: boolean } | null)?.fromHost) {
      const replace = how !== 'PUSH' // the first entry and a replace take the host's entry over
      if (o.onNavigate) o.onNavigate(full, { replace })
      else if (window.location.pathname + window.location.search !== full) window.history[replace ? 'replaceState' : 'pushState'](null, '', full)
    }
    const t = titles.find(([re]) => re.test(loc.pathname))
    if (t) o.onTitle?.(t[1])
  }, [loc, how, base, o])

  // the browser's back button when the console keeps the history itself: its own paths only, so
  // two consoles on a page do not reset each other
  useEffect(() => {
    if (o.onNavigate) return
    const back = () => {
      const p = window.location.pathname
      if (base && p !== base && !p.startsWith(base + '/')) return
      go.current(innerPath(base, p + window.location.search, fallback), { replace: true, state: { fromHost: true } })
    }
    window.addEventListener('popstate', back)
    return () => window.removeEventListener('popstate', back)
  }, [o, base, fallback])
  return null
}

/** Mounts the console in element's shadow root (made open when it has none). */
export function mountKista(element: HTMLElement, options: KistaOptions): KistaHandle {
  if (!options?.apiBase || typeof options.getToken !== 'function') throw new Error('mountKista: apiBase and getToken are required')
  if (mounted.has(element)) throw new Error('mountKista: this element holds a console already (unmount it first)')
  mounted.add(element)
  const shadow = element.shadowRoot ?? element.attachShadow({ mode: 'open' })
  const unstyle = style(shadow)
  const frame = document.createElement('div')
  frame.className = 'kista-root'
  shadow.appendChild(frame)
  const root: Root = createRoot(frame)
  let live: Live = { theme: options.theme === 'dark' ? 'dark' : 'light', tenant: options.tenant, generation: 0 }
  let start = window.location.pathname + window.location.search
  let n = 0
  let gone = false
  const render = () => {
    element.dataset.kistaTheme = live.theme // the variables' dark defaults, on :host
    const fallback = live.tenant !== undefined ? '/channels' : '/tenants'
    const first = innerPath(options.basePath ?? '', start, fallback)
    const [pathname, search] = first.includes('?') ? [first.slice(0, first.indexOf('?')), first.slice(first.indexOf('?'))] : [first, '']
    // nothing to tell the host when its path is the console's already, or is not below basePath at
    // all (another console's, on the same page); its basePath alone is told where the console went
    const base = trim(options.basePath ?? '')
    const p = start.split('?')[0]
    const below = !base || p === base || p.startsWith(base + '/')
    const fromHost = !below || base + first === start
    root.render(
      <StrictMode>
        <FrameProvider embedded>
          <MemoryRouter key={`${live.tenant ?? ''}|${live.generation}`} initialEntries={[{ pathname, search, state: { fromHost } }]} initialIndex={0}>
            <Embedded key={`${live.tenant ?? ''}|${live.generation}`} o={options} live={live} element={element} />
          </MemoryRouter>
        </FrameProvider>
      </StrictMode>,
    )
  }
  render()
  return {
    update({ theme, path, locale: _locale, tenant }) {
      if (gone) return
      if (tenant !== undefined && tenant !== live.tenant) {
        // another tenant: a new router at the default route (or the path given with it); answers
        // in flight are dropped with the old one
        start = path ?? trim(options.basePath ?? '') // the basePath alone: the host is told the new route
        live = { ...live, tenant, path: undefined, generation: live.generation + 1, theme: theme ?? live.theme }
      } else {
        live = { ...live, theme: theme ?? live.theme, path: path === undefined ? live.path : { to: path, n: ++n } }
      }
      render()
    },
    unmount() {
      if (gone) return
      gone = true
      mounted.delete(element)
      root.unmount()
      frame.remove()
      delete element.dataset.kistaTheme
      unstyle()
    },
  }
}

type Fn<T> = T | undefined

/** <kista-console api-base tenant audience base-path theme locale>: getToken and the callbacks are
 * properties (a token is never an attribute), or the kista-navigate, kista-title and
 * kista-unauthorized events; it mounts once getToken is set; data-contract names the contract. */
export class KistaConsole extends HTMLElement {
  static observedAttributes = ['tenant', 'theme', 'locale']
  private handle?: KistaHandle
  private leaving?: KistaHandle // unmounted after the host's commit, unless the element comes back
  private tokenFn?: KistaOptions['getToken']
  declare onNavigate: Fn<KistaOptions['onNavigate']>
  declare onTitle: Fn<KistaOptions['onTitle']>
  declare onUnauthorized: Fn<KistaOptions['onUnauthorized']>

  constructor() {
    super()
    // properties a host set before this module defined the element hide the class's own
    for (const key of ['getToken', 'onNavigate', 'onTitle', 'onUnauthorized'] as const) {
      if (Object.prototype.hasOwnProperty.call(this, key)) {
        const value = (this as Record<string, unknown>)[key]
        delete (this as Record<string, unknown>)[key]
        ;(this as Record<string, unknown>)[key] = value
      }
    }
  }

  set getToken(fn: KistaOptions['getToken'] | undefined) {
    this.tokenFn = typeof fn === 'function' ? fn : undefined
    if (this.tokenFn) this.mount()
    else this.disconnectedCallback() // nothing to call the API with
  }
  get getToken() {
    return this.tokenFn
  }

  connectedCallback() {
    this.dataset.contract = String(contract)
    this.mount()
  }
  disconnectedCallback() {
    // a React host removes the element while it commits: unmounting another root synchronously
    // then is refused, so it waits a microtask (and a move, disconnected and connected at once,
    // keeps its console)
    const h = this.handle
    this.handle = undefined
    if (!h) return
    this.leaving = h
    queueMicrotask(() => {
      if (this.leaving === h) {
        this.leaving = undefined
        h.unmount()
      }
    })
  }
  attributeChangedCallback(name: string, old: string | null, value: string | null) {
    if (old === value) return
    if (name === 'theme' && (value === 'light' || value === 'dark')) this.handle?.update({ theme: value })
    if (name === 'locale' && value) this.handle?.update({ locale: value })
    if (name === 'tenant') {
      if (value === null) {
        this.disconnectedCallback() // the server scope is another mount, never an update
        this.mount()
      } else this.handle?.update({ tenant: value })
    }
  }
  /** A path the host navigated to. */
  navigate(path: string) {
    this.handle?.update({ path })
  }

  private mount() {
    if (this.handle || !this.isConnected || !this.tokenFn) return
    if (this.leaving) {
      this.handle = this.leaving
      this.leaving = undefined
      return
    }
    const theme = this.getAttribute('theme')
    this.handle = mountKista(this, {
      apiBase: this.getAttribute('api-base') ?? '',
      basePath: this.getAttribute('base-path') ?? '',
      tenant: this.getAttribute('tenant') ?? undefined,
      audience: this.getAttribute('audience') ?? undefined,
      theme: theme === 'dark' ? 'dark' : 'light',
      locale: this.getAttribute('locale') ?? undefined,
      getToken: (a, how) => this.tokenFn!(a, how),
      onUnauthorized: () => (this.onUnauthorized ? this.onUnauthorized() : this.dispatchEvent(new CustomEvent('kista-unauthorized'))),
      onNavigate: (p, how) => (this.onNavigate ? this.onNavigate(p, how) : this.dispatchEvent(new CustomEvent('kista-navigate', { detail: { path: p, ...how } }))),
      onTitle: (t) => (this.onTitle ? this.onTitle(t) : this.dispatchEvent(new CustomEvent('kista-title', { detail: t }))),
    })
  }
}

if (typeof customElements !== 'undefined' && !customElements.get('kista-console')) customElements.define('kista-console', KistaConsole)
