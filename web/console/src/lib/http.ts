// The management API client (spec 0015): bearer tokens bound to the scope's route prefix,
// If-Match on changes, one shared renewal and one retry on a 401, none on a 404 or 400.

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message)
  }
}

export interface Answer<T> {
  status: number
  data: T
  etag: string
}

export interface ClientOptions {
  /** The API's base, e.g. "/api/v1" or "https://kista.example/api/v1". */
  base: string
  /** The paths (under base) the scope's token may be sent to, e.g. "/tenants/acme/". */
  prefix: string
  /** A token for the scope; renew asks for a newly issued one. */
  getToken: (renew: boolean) => Promise<string>
  /** Called when a renewed token is refused too; throttled to once per unauthorizedEvery. */
  onUnauthorized?: () => void
  /** The throttle, in ms (default 30 seconds); 0: every time (a caller that throttles itself). */
  unauthorizedEvery?: number
  now?: () => number
}

export interface RequestOptions {
  body?: unknown
  ifMatch?: string
  /** A public route: no token is sent. */
  anonymous?: boolean
}

export class Client {
  private renewing: Promise<string> | null = null
  private lastUnauthorized = -Infinity

  constructor(private readonly o: ClientOptions) {}

  get<T>(path: string, opts: RequestOptions = {}) {
    return this.request<T>('GET', path, opts)
  }
  post<T>(path: string, opts: RequestOptions = {}) {
    return this.request<T>('POST', path, opts)
  }
  del<T>(path: string, opts: RequestOptions = {}) {
    return this.request<T>('DELETE', path, opts)
  }

  async request<T>(method: string, path: string, opts: RequestOptions = {}): Promise<Answer<T>> {
    if (!opts.anonymous && !this.inScope(path)) {
      // a token never leaves its scope: a request outside it is a bug
      throw new Error(`kista console: ${path} is outside this scope (${this.o.prefix})`)
    }
    let res = await this.send(method, path, opts, false)
    if (res.status === 401 && !opts.anonymous) {
      res = await this.send(method, path, opts, true)
      if (res.status === 401) {
        const now = (this.o.now ?? Date.now)()
        if (now - this.lastUnauthorized >= (this.o.unauthorizedEvery ?? 30_000)) {
          this.lastUnauthorized = now
          this.o.onUnauthorized?.()
        }
      }
    }
    const text = await res.text()
    let data: unknown = undefined
    if (text) {
      try {
        data = JSON.parse(text)
      } catch {
        data = text
      }
    }
    if (!res.ok) {
      const detail = (data as { detail?: string; title?: string } | undefined) ?? {}
      throw new ApiError(res.status, detail.detail || detail.title || `${res.status}`)
    }
    return { status: res.status, data: data as T, etag: res.headers.get('ETag') ?? '' }
  }

  /** Whether path stays below the scope's prefix once resolved: a ".." segment (or "%2e%2e") the
   * URL parser removes cannot lead a token elsewhere. */
  private inScope(path: string): boolean {
    if (!path.startsWith(this.o.prefix)) return false
    try {
      const base = new URL(this.o.base, location.href)
      const want = base.pathname.replace(/\/+$/, '') + this.o.prefix
      return new URL(this.o.base + path, location.href).pathname.startsWith(want)
    } catch {
      return false
    }
  }

  private async send(method: string, path: string, opts: RequestOptions, renew: boolean): Promise<Response> {
    const headers: Record<string, string> = { Accept: 'application/json' }
    if (!opts.anonymous) {
      headers.Authorization = `Bearer ${await this.token(renew)}`
    }
    if (opts.ifMatch) {
      headers['If-Match'] = opts.ifMatch
    }
    let body: string | undefined
    if (opts.body !== undefined) {
      headers['Content-Type'] = 'application/json'
      body = JSON.stringify(opts.body)
    }
    return fetch(this.o.base + path, { method, headers, body, credentials: 'omit', cache: 'no-store' })
  }

  private token(renew: boolean): Promise<string> {
    if (!renew) {
      return this.renewing ?? this.o.getToken(false)
    }
    // one renewal shared by every request that met a 401 meanwhile
    if (!this.renewing) {
      this.renewing = this.o.getToken(true).finally(() => {
        this.renewing = null
      })
    }
    return this.renewing
  }
}

/** A path segment, escaped. */
export const seg = (s: string) => encodeURIComponent(s)
