import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, Client } from './http'

type Call = { url: string; init: RequestInit }

function stub(statuses: number[]) {
  const calls: Call[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      calls.push({ url, init })
      const status = statuses.shift() ?? 200
      return new Response(status === 204 ? null : JSON.stringify(status < 400 ? { ok: true } : { detail: 'no' }), {
        status,
        headers: { ETag: '"v3"' },
      })
    }),
  )
  return calls
}

const auth = (c: Call) => (c.init.headers as Record<string, string>).Authorization

afterEach(() => vi.unstubAllGlobals())

describe('Client', () => {
  it('sends the token, If-Match and JSON, without cookies', async () => {
    const calls = stub([200])
    const c = new Client({ base: '/api/v1', prefix: '/tenants/acme/', getToken: async () => 't1' })
    const r = await c.post('/tenants/acme/x', { body: { a: 1 }, ifMatch: '"v2"' })
    expect(r.etag).toBe('"v3"')
    const h = calls[0].init.headers as Record<string, string>
    expect(h.Authorization).toBe('Bearer t1')
    expect(h['If-Match']).toBe('"v2"')
    expect(calls[0].init.credentials).toBe('omit')
    expect(calls[0].init.body).toBe('{"a":1}')
  })

  it('never sends a token outside its scope', async () => {
    const calls = stub([200])
    const c = new Client({ base: '/api/v1', prefix: '/tenants/acme/', getToken: async () => 't1' })
    await expect(c.get('/tenants/other/channels')).rejects.toThrow(/outside this scope/)
    // dot segments resolved by the URL parser cannot climb out of it either
    await expect(c.get('/tenants/acme/../other/channels')).rejects.toThrow(/outside this scope/)
    await expect(c.get('/tenants/acme/channels/%2e%2e/%2e%2e/../other')).rejects.toThrow(/outside this scope/)
    await c.get('/event-kinds', { anonymous: true })
    expect(calls).toHaveLength(1)
    expect(auth(calls[0])).toBeUndefined()
  })

  it('renews once on a 401 and retries, shared by concurrent requests', async () => {
    const calls = stub([401, 401, 200, 200])
    let renewals = 0
    const c = new Client({
      base: '',
      prefix: '/',
      getToken: async (renew) => {
        if (renew) renewals++
        return renew ? 'fresh' : 'stale'
      },
    })
    await Promise.all([c.get('/a'), c.get('/b')])
    expect(renewals).toBe(1)
    expect(calls.filter((x) => auth(x) === 'Bearer fresh')).toHaveLength(2)
  })

  it('does not renew on a 404 or 400', async () => {
    stub([404, 400])
    const getToken = vi.fn(async (_renew: boolean) => "t")
    const c = new Client({ base: '', prefix: '/', getToken })
    await expect(c.get('/a')).rejects.toMatchObject({ status: 404 })
    await expect(c.get('/b')).rejects.toBeInstanceOf(ApiError)
    expect(getToken.mock.calls.every(([renew]) => !renew)).toBe(true)
  })

  it('calls onUnauthorized at most once per 30 seconds', async () => {
    stub([401, 401, 401, 401, 401, 401])
    let now = 0
    const onUnauthorized = vi.fn()
    const c = new Client({ base: '', prefix: '/', getToken: async () => 't', onUnauthorized, now: () => now })
    await expect(c.get('/a')).rejects.toMatchObject({ status: 401 })
    now = 10_000
    await expect(c.get('/a')).rejects.toMatchObject({ status: 401 })
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
    now = 40_000
    await expect(c.get('/a')).rejects.toMatchObject({ status: 401 })
    expect(onUnauthorized).toHaveBeenCalledTimes(2)
  })
})
