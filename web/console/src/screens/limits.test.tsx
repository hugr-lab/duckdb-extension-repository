import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Perms, TenantContext } from '../lib/context'
import { ApiError, Client, final } from '../lib/http'
import { transient } from '../lib/upload'
import { Limits, limitText } from './Limits'
import { tenantSections } from './TenantArea'

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
    expect(screen.getAllByTitle('1,073,741,824 bytes').length).toBe(2) // used and applies, exactly
    expect(screen.getByText('(reached)', { exact: false })).toBeTruthy()
    expect(screen.getByText('1000')).toBeTruthy()
    expect(screen.getByText('unlimited')).toBeTruthy()
    expect(screen.getAllByText('default').length).toBe(2)
  })

  it('shows an error when the answer is refused', async () => {
    for (const status of [403, 404]) {
      vi.stubGlobal('fetch', vi.fn(async () => json({ detail: 'not yours' }, status)))
      const client = new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })
      render(
        <MemoryRouter>
          <TenantContext.Provider value={{ client, tenant: 'acme', perms: new Perms(false), api: '/tenants/acme', base: '/t/acme' }}>
            <Limits />
          </TenantContext.Provider>
        </MemoryRouter>)
      expect(await screen.findByText('not yours', { exact: false })).toBeTruthy()
      expect(screen.queryByText('Bytes')).toBeNull()
      cleanup()
    }
  })

  it('is a section for tenant administrators only', () => {
    const shown = (p: Perms) => tenantSections(p, '/t/acme').some((t) => t.label === 'Limits')
    const g = (verbs: string[], more: object = {}) => ({ principal: 'p', verbs, ...more })
    expect(shown(new Perms(true))).toBe(true)
    expect(shown(new Perms(false, [g(['admin'])]))).toBe(true)
    expect(shown(new Perms(false, [g(['admin'], { channel: 'prod' })]))).toBe(false)
    expect(shown(new Perms(false, [g(['admin'], { extension: 'acl' })]))).toBe(false)
    expect(shown(new Perms(false, [], true))).toBe(false) // an auditor
    expect(shown(new Perms(false, [g(['install'])]))).toBe(false)
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
