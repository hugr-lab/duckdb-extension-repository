import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Perms, TenantContext } from '../lib/context'
import { ApiError, Client, final } from '../lib/http'
import { transient } from '../lib/upload'
import { Limits, limitText } from './Limits'

// Spec 0038: the tenant's limits, read only (set, in force, used, a reached one marked); a
// tenant-limit or catalogue-full refusal is final, never retried.

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/problem+json' } })

describe('Limits', () => {
  it('shows each limit: used, applies, set', async () => {
    vi.stubGlobal('fetch', vi.fn(async (input: string) =>
      new URL(input).pathname.endsWith('/tenants/acme/limits')
        ? json({ limits: {
            bytes: { set: 1073741824, limit: 1073741824, used: 1073741824 },
            channels: { set: 'default', limit: 1000, used: 3 },
            upstreams: { set: 'unlimited', limit: 100, used: 1 },
            publishers: { set: 'default', limit: 200, used: 0 },
          } })
        : json({}, 404)))
    const client = new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })
    render(
      <MemoryRouter>
        <TenantContext.Provider value={{ client, tenant: 'acme', perms: new Perms(false), api: '/tenants/acme', base: '/t/acme' }}>
          <Limits />
        </TenantContext.Provider>
      </MemoryRouter>)
    expect(await screen.findByText('Bytes')).toBeTruthy()
    expect(screen.getAllByText('1 GiB').length).toBe(3)
    expect(screen.getByText('(reached)', { exact: false })).toBeTruthy()
    expect(screen.getByText('1000')).toBeTruthy()
    expect(screen.getByText('unlimited')).toBeTruthy()
    expect(screen.getAllByText('default').length).toBe(2)
  })

  it('reads values', () => {
    expect(limitText('bytes', null)).toBe('unlimited')
    expect(limitText('channels', 'default')).toBe('default')
    expect(limitText('channels', 20)).toBe('20')
  })
})

describe('a refusal by a limit', () => {
  it('is final: never retried', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => json({ type: 'urn:kista:problem:tenant-limit', detail: "the tenant's channels limit is reached", limit: 'channels' }, 429)))
    const client = new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/acme/', getToken: async () => 'tok' })
    const err = await client.post('/tenants/acme/channels', { body: { name: 'x' } }).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.problem).toBe('tenant-limit')
    expect(err.message).toContain('channels limit')
    expect(final(err)).toBe(true)
    expect(transient(err)).toBe(false)
    expect(transient(new ApiError(429, 'busy', 5, 'upload-slots'))).toBe(true)
    expect(transient(new ApiError(429, 'full', undefined, 'catalogue-full'))).toBe(false)
  })
})
