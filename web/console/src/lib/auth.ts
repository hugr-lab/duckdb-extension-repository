// Standalone sign-in (spec 0015): Authorization Code with PKCE (oidc-client-ts); the tokens stay in
// memory, only the PKCE state (and which issuer a scope signs in with) is in sessionStorage. A
// token is renewed through its refresh token before the API's admin_token_max_age would refuse it,
// never through a hidden iframe.

import { InMemoryWebStorage, User, UserManager, WebStorageStateStore } from 'oidc-client-ts'

export interface ConsoleIssuer {
  name: string
  issuer: string
  client_id: string
  scopes: string[]
  audience_parameter: string
  audience: string
}

/** When a token must be renewed, in ms: a minute before it expires, or five minutes before the
 * API's freshness limit (iat + maxAge), whichever is first. */
export function renewAt(iat: number | undefined, exp: number | undefined, maxAgeSeconds: number): number {
  const limits: number[] = []
  if (exp) limits.push((exp - 60) * 1000)
  // the margin is at most half the limit, so a short one does not renew on every request
  if (iat && maxAgeSeconds > 0) limits.push((iat + maxAgeSeconds - Math.min(300, maxAgeSeconds / 2)) * 1000)
  return limits.length ? Math.min(...limits) : Infinity
}

function claims(token: string): { iat?: number; exp?: number } {
  try {
    const part = token.split('.')[1].replace(/-/g, '+').replace(/_/g, '/')
    return JSON.parse(atob(part.padEnd(part.length + ((4 - (part.length % 4)) % 4), '=')))
  } catch {
    return {}
  }
}

/** Whether this window is a popup this origin opened (a sign-in popup): an opener elsewhere (a
 * page that opened the console in a new tab) is not one, and a redirect's callback runs. */
export function openedByThisOrigin(): boolean {
  try {
    return !!window.opener && window.opener !== window && window.opener.location.origin === location.origin
  } catch {
    return false // a cross-origin opener: its location cannot be read
  }
}

export class NeedSignIn extends Error {
  constructor() {
    super('sign in again')
  }
}

const chosenKey = (scopeBase: string) => `kista.issuer:${scopeBase}`

export function rememberIssuer(scopeBase: string, name: string) {
  try {
    sessionStorage.setItem(chosenKey(scopeBase), name)
  } catch {
    /* storage refused: the user picks again */
  }
}

export function rememberedIssuer(scopeBase: string): string | null {
  try {
    return sessionStorage.getItem(chosenKey(scopeBase))
  } catch {
    return null
  }
}

export class Session {
  readonly manager: UserManager
  private user: User | null = null
  private listeners = new Set<() => void>()
  expired = false

  /**
   * @param scopeBase the scope's absolute path, e.g. "/ui/t/acme"
   * @param maxAge the API's admin_token_max_age, in seconds
   */
  constructor(
    readonly scopeBase: string,
    readonly issuer: ConsoleIssuer,
    readonly maxAge: number,
  ) {
    const extra: Record<string, string> = issuer.audience_parameter ? { [issuer.audience_parameter]: issuer.audience } : {}
    this.manager = new UserManager({
      authority: issuer.issuer,
      client_id: issuer.client_id,
      redirect_uri: `${location.origin}${scopeBase}/callback`,
      popup_redirect_uri: `${location.origin}${scopeBase}/callback`,
      post_logout_redirect_uri: `${location.origin}${scopeBase}/`,
      response_type: 'code',
      scope: issuer.scopes.join(' '),
      userStore: new WebStorageStateStore({ store: new InMemoryWebStorage() }),
      stateStore: new WebStorageStateStore({ store: sessionStorage }),
      automaticSilentRenew: false,
      monitorSession: false,
      extraQueryParams: extra,
      extraTokenParams: extra,
    })
  }

  onChange(f: () => void): () => void {
    this.listeners.add(f)
    return () => this.listeners.delete(f)
  }

  private changed() {
    this.listeners.forEach((f) => f())
  }

  get signedIn() {
    return this.user !== null && !this.expired
  }

  get subject(): string {
    const p = this.user?.profile
    return (p?.preferred_username as string) || (p?.name as string) || p?.sub || ''
  }

  /** Redirects to the issuer; returnTo is the path to come back to. */
  async signIn(returnTo: string) {
    rememberIssuer(this.scopeBase, this.issuer.name)
    await this.manager.signinRedirect({ state: { returnTo } })
  }

  /** Signs in again in a popup, keeping the page (and a form being edited). */
  async signInPopup() {
    this.user = await this.manager.signinPopup()
    this.expired = false
    this.epoch++
    this.changed()
  }

  /** Moves on every sign-in again: what failed before it loads again. */
  epoch = 0

  /** Completes a sign-in at the callback: a popup's (opened by this console, same origin) hands its
   * answer to the opener and returns null; a redirect's returns the path to go back to. */
  async completeCallback(): Promise<string | null> {
    if (openedByThisOrigin()) {
      await this.manager.signinPopupCallback()
      return null
    }
    const user = await this.manager.signinRedirectCallback()
    this.user = user
    this.expired = false
    this.changed()
    const st = user.state as { returnTo?: string } | undefined
    return st?.returnTo && st.returnTo.startsWith(this.scopeBase + '/') ? st.returnTo : this.scopeBase + '/'
  }

  /** A token for the API: renewed when renew is asked or its freshness is running out. */
  async getToken(renew: boolean): Promise<string> {
    if (!this.user) throw new NeedSignIn()
    const c = claims(this.user.access_token)
    if (renew || Date.now() >= renewAt(c.iat, c.exp, this.maxAge)) {
      await this.renew()
    }
    return this.user.access_token
  }

  private renewing: Promise<void> | null = null

  /** One renewal at a time: a refresh token is redeemed once (IdPs that rotate them revoke a
   * family whose token is reused). */
  private renew(): Promise<void> {
    this.renewing ??= this.renewOnce().finally(() => {
      this.renewing = null
    })
    return this.renewing
  }

  private async renewOnce() {
    if (!this.user?.refresh_token) {
      this.expire()
      throw new NeedSignIn()
    }
    try {
      this.user = await this.manager.signinSilent() // with a refresh token: a token request, no iframe
      this.changed()
    } catch {
      this.expire()
      throw new NeedSignIn()
    }
  }

  expire() {
    if (!this.expired) {
      this.expired = true
      this.changed()
    }
  }

  async signOut() {
    const user = this.user
    this.user = null
    this.changed()
    try {
      if (user?.refresh_token) await this.manager.revokeTokens(['refresh_token'])
    } catch {
      /* the IdP has no revocation endpoint */
    }
    await this.manager.signoutRedirect({ id_token_hint: user?.id_token })
  }
}
