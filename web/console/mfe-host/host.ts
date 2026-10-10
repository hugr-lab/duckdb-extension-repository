// The test platform (spec 0015 phase 1b): it holds the tokens (here from Playwright's exposed
// hostToken, or a pasted one), owns the address bar, and mounts two kista consoles from kista's
// origin by the contract: <kista-console> with getToken and kista-* events.

type Mode = 'ok' | 'expired' | 'revoked'
declare global {
  interface Window {
    hostToken?: (audience: string, renew: boolean) => Promise<string>
    calls: { audience: string; renew: boolean }[]
    unauthorized: number
  }
}

// test-only: it loads the module named by ?kista= and hands it tokens, so it is never built into
// dist/ nor served by kista
const params = new URLSearchParams(location.search)
const kista = params.get('kista') ?? sessionStorage.getItem('kista') ?? 'https://localhost:8443'
sessionStorage.setItem('kista', kista)
window.calls = []
window.unauthorized = 0
let mode: Mode = 'ok'
const status = document.getElementById('status')!

async function getToken(audience: string, how?: { renew?: boolean }): Promise<string> {
  const renew = !!how?.renew
  window.calls.push({ audience, renew })
  // expired: the cached token is refused, a renewed one works; revoked: neither does
  if (mode === 'revoked' || (mode === 'expired' && !renew)) return 'not-a-token'
  if (window.hostToken) return window.hostToken(audience, renew)
  const t = (document.getElementById('token') as HTMLInputElement).value.trim()
  if (!t) throw new Error('no token')
  return t
}

const els = [...document.querySelectorAll<HTMLElement & { getToken?: unknown; navigate(p: string): void }>('kista-console')]
for (const el of els) {
  el.setAttribute('api-base', kista)
  el.addEventListener('kista-navigate', (e) => {
    const { path, replace } = (e as CustomEvent<{ path: string; replace: boolean }>).detail
    if (location.pathname + location.search !== path) history[replace ? 'replaceState' : 'pushState'](null, '', path)
  })
  el.addEventListener('kista-title', (e) => {
    document.getElementById(`title-${el.id}`)!.textContent = (e as CustomEvent<string>).detail
  })
  el.addEventListener('kista-unauthorized', () => {
    window.unauthorized++
    status.textContent = `unauthorized ×${window.unauthorized}`
  })
}

// the host's back button: each console follows the paths below its own base
addEventListener('popstate', () => {
  for (const el of els) {
    const base = el.getAttribute('base-path')!
    if (location.pathname === base || location.pathname.startsWith(base + '/')) el.navigate(location.pathname + location.search)
  }
})

document.getElementById('theme')!.onclick = () => {
  for (const el of els) el.setAttribute('theme', el.getAttribute('theme') === 'dark' ? 'light' : 'dark')
}
document.getElementById('brand')!.onclick = () => els[0].classList.toggle('branded')
for (const m of ['expire', 'revoke', 'restore'] as const) {
  document.getElementById(m)!.onclick = () => {
    mode = m === 'expire' ? 'expired' : m === 'revoke' ? 'revoked' : 'ok'
    status.textContent = `token: ${mode}`
  }
}

// the module, from kista's origin (CORS: ui.allowed_origins); getToken is set after the element is
// defined, as a property
await import(/* @vite-ignore */ `${kista}/ui/mfe/kista.js`)
for (const el of els) el.getToken = getToken
status.textContent = `kista contract ${els[0].dataset.contract ?? '?'}`
