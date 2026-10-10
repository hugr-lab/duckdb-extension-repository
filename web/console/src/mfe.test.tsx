import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { waitFor } from '@testing-library/react'
import { KistaConsole, contract, innerPath, mountKista, type KistaHandle } from './mfe'

// The micro-frontend contract (spec 0015 phase 1b): mountKista and <kista-console>, the audience
// asked of the host, routing under basePath, tenant updates, a rejected getToken.

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

function api(over: Record<string, () => Response> = {}) {
  const calls: { url: string; auth: string | null }[] = []
  const routes: Record<string, () => Response> = {
    '/api/v1/console': () => json(200, { environment: '', admin_token_max_age: 3600, audience: 'api://kista-server', issuers: [] }),
    '/api/v1/tenants/acme/console': () => json(200, { audience: 'https://k.example/acme', issuers: [] }),
    '/api/v1/tenants/other/console': () => json(404, { title: 'Not Found' }),
    '/api/v1/tenants/beta/console': () => json(200, { audience: 'https://k.example/beta', issuers: [] }),
    '/api/v1/tenants/beta/whoami': () => json(200, { grants: [{ principal: 'role:kc|a', verbs: ['admin'] }], holds_admin: true, holds_audit: false }),
    '/api/v1/tenants/beta/channels': () => json(200, { channels: [] }),
    '/api/v1/tenants/acme/whoami': () => json(200, { grants: [{ principal: 'role:kc|a', verbs: ['admin'] }], holds_admin: true, holds_audit: false }),
    '/api/v1/tenants/acme/channels': () => json(200, { channels: [] }),
    ...over,
  }
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const u = new URL(input)
    calls.push({ url: u.pathname, auth: new Headers(init?.headers).get('Authorization') })
    return (routes[u.pathname] ?? (() => json(404, {})))()
  }))
  return calls
}

const text = (el: HTMLElement) => el.shadowRoot?.textContent ?? ''

describe('innerPath', () => {
  it('takes the console path below basePath, else the default', () => {
    expect(innerPath('/platform/acme', '/platform/acme/channels/prod?x=1', '/channels')).toBe('/channels/prod?x=1')
    expect(innerPath('/platform/acme/', '/platform/acme', '/channels')).toBe('/channels')
    expect(innerPath('/platform/acme', '/platform/acmex/channels', '/channels')).toBe('/channels')
    expect(innerPath('/platform/acme', '/elsewhere', '/tenants')).toBe('/tenants')
    expect(innerPath('', '/channels', '/x')).toBe('/channels')
  })
})

describe('mountKista', () => {
  let el: HTMLElement
  let h: KistaHandle | undefined
  beforeEach(() => {
    history.replaceState(null, '', '/platform/acme')
    el = document.createElement('div')
    document.body.appendChild(el)
  })
  afterEach(() => {
    h?.unmount()
    h = undefined
    el.remove()
    vi.unstubAllGlobals()
  })

  it('asks the host for the tenant audience and reports its route as the host path', async () => {
    const calls = api()
    const getToken = vi.fn(async () => 'tok')
    const onNavigate = vi.fn()
    const onTitle = vi.fn()
    h = mountKista(el, { apiBase: 'https://k.example', getToken, tenant: 'acme', basePath: '/platform/acme', onNavigate, onTitle, theme: 'dark' })
    await waitFor(() => expect(text(el)).toContain('Channels'))
    expect(getToken).toHaveBeenCalledWith('https://k.example/acme', undefined)
    expect(calls.find((c) => c.url === '/api/v1/tenants/acme/whoami')?.auth).toBe('Bearer tok')
    // the host's basePath alone: the console's default route, replacing the host's entry
    expect(onNavigate).toHaveBeenCalledWith('/platform/acme/channels', { replace: true })
    expect(onTitle).toHaveBeenCalledWith('Channels')
    expect(el.dataset.kistaTheme).toBe('dark')
    h.update({ theme: 'light' })
    expect(el.dataset.kistaTheme).toBe('light')
    expect(() => mountKista(el, { apiBase: 'x', getToken })).toThrow(/already/)
  })

  it('an audience the host overrides is the one asked for', async () => {
    api()
    const getToken = vi.fn(async () => 'tok')
    h = mountKista(el, { apiBase: 'https://k.example', getToken, tenant: 'acme', audience: 'api://kista-acme', onNavigate: () => {} })
    await waitFor(() => expect(getToken).toHaveBeenCalled())
    expect(getToken).toHaveBeenCalledWith('api://kista-acme', undefined)
  })

  it('renews a token issued too long ago before the API refuses it', async () => {
    api()
    const old = 'x.' + btoa(JSON.stringify({ iat: Math.floor(Date.now() / 1000) - 7200 })) + '.y'
    const getToken = vi.fn(async (_a: string, how?: { renew?: boolean }) => (how?.renew ? 'fresh' : old))
    h = mountKista(el, { apiBase: 'https://k.example', getToken, tenant: 'acme', onNavigate: () => {} })
    await waitFor(() => expect(getToken).toHaveBeenCalledWith('https://k.example/acme', { renew: true }))
  })

  it('a rejected getToken says so and is never an ended session', async () => {
    api()
    const onUnauthorized = vi.fn()
    h = mountKista(el, { apiBase: 'https://k.example', getToken: () => Promise.reject(new Error('no')), tenant: 'acme', onUnauthorized, onNavigate: () => {} })
    await waitFor(() => expect(text(el)).toContain('no token for'))
    expect(onUnauthorized).not.toHaveBeenCalled()
  })

  it('another tenant remounts; an unknown one is not found', async () => {
    api()
    h = mountKista(el, { apiBase: 'https://k.example', getToken: async () => 'tok', tenant: 'acme', onNavigate: () => {} })
    await waitFor(() => expect(text(el)).toContain('Channels'))
    h.update({ tenant: 'other' })
    await waitFor(() => expect(text(el)).toContain('No such tenant.'))
  })

  it('another tenant under a basePath tells the host its new route', async () => {
    api()
    const onNavigate = vi.fn()
    h = mountKista(el, { apiBase: 'https://k.example', getToken: async () => 'tok', tenant: 'acme', basePath: '/platform/acme', onNavigate })
    await waitFor(() => expect(onNavigate).toHaveBeenCalledWith('/platform/acme/channels', { replace: true }))
    onNavigate.mockClear()
    h.update({ tenant: 'beta' })
    await waitFor(() => expect(onNavigate).toHaveBeenCalledWith('/platform/acme/channels', { replace: true }))
  })

  it('a 401 even renewed tells the host, once', async () => {
    api({ '/api/v1/tenants/acme/whoami': () => json(401, { title: 'Unauthorized' }) })
    const onUnauthorized = vi.fn()
    h = mountKista(el, { apiBase: 'https://k.example', getToken: async () => 'tok', tenant: 'acme', onUnauthorized, onNavigate: () => {} })
    await waitFor(() => expect(text(el)).toContain('Your session ended'))
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('keeps the history itself without onNavigate, and ignores popstate outside its basePath', async () => {
    api()
    history.replaceState(null, '', '/platform/acme')
    h = mountKista(el, { apiBase: 'https://k.example', getToken: async () => 'tok', tenant: 'acme', basePath: '/platform/acme' })
    await waitFor(() => expect(location.pathname).toBe('/platform/acme/channels'))
    history.pushState(null, '', '/other-console/x')
    dispatchEvent(new PopStateEvent('popstate'))
    await new Promise((r) => setTimeout(r, 20))
    expect(location.pathname).toBe('/other-console/x')
  })
})

describe('<kista-console>', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('mounts once getToken is set, names its contract and sends events', async () => {
    api()
    history.replaceState(null, '', '/p')
    expect(customElements.get('kista-console')).toBe(KistaConsole)
    const el = document.createElement('kista-console') as KistaConsole
    el.setAttribute('api-base', 'https://k.example')
    el.setAttribute('tenant', 'acme')
    el.setAttribute('base-path', '/p')
    const nav = vi.fn()
    el.addEventListener('kista-navigate', (e) => nav((e as CustomEvent).detail))
    document.body.appendChild(el)
    expect(el.dataset.contract).toBe(String(contract))
    expect(el.shadowRoot).toBeNull() // no token function yet: nothing mounted
    el.getToken = async () => 'tok'
    await waitFor(() => expect(nav).toHaveBeenCalledWith({ path: '/p/channels', replace: true }))
    el.setAttribute('theme', 'dark')
    expect(el.dataset.kistaTheme).toBe('dark')
    el.remove()
    await Promise.resolve() // unmounted after the host's commit
    expect(el.dataset.kistaTheme).toBeUndefined()
  })
})
