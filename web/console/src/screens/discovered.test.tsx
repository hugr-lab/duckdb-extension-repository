import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Perms, TenantContext } from '../lib/context'
import { Client } from '../lib/http'
import { Discovered, type Upstream } from './Upstreams'

// Spec 0039 PR 2: a mirror with "*" shows the names it took from its kista source.

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

const upstream = (over: Partial<Upstream>): Upstream => ({
  name: 'enterest', kind: 'repository', channel: 'prod', mode: 'mirror', visibility: 'private', state: 'active', run: 'idle', keys: [],
  platforms: ['linux_amd64'], extensions: [{ name: '*', versions: [], allow_reserved: false }], created_at: '', created_by: '', usage: 'off',
  etag: '"v1"', remote_kista: true, discovered: 2, ...over,
} as Upstream)

function show(u: Upstream) {
  const client = new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })
  render(
    <MemoryRouter>
      <TenantContext.Provider value={{ client, tenant: 'acme', perms: new Perms(false), api: '/tenants/acme', base: '/t/acme' }}>
        <Discovered u={u} path="/tenants/acme/upstreams/enterest" />
      </TenantContext.Provider>
    </MemoryRouter>)
}

describe('Discovered', () => {
  it('lists the names', async () => {
    vi.stubGlobal('fetch', vi.fn(async (input: string) =>
      new URL(input).pathname.endsWith('/upstreams/enterest/names') ? json({ names: ['geo', 'tresor'] }) : json({}, 404)))
    show(upstream({}))
    expect(await screen.findByText('tresor')).toBeTruthy()
    expect(screen.getByText('geo')).toBeTruthy()
    expect(screen.queryByText(/has not answered as a kista/)).toBeNull()
  })

  it('says when there are none yet, and when the source is no kista', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => json({ names: [] })))
    show(upstream({ remote_kista: false, discovered: 0 }))
    expect(await screen.findByText(/the next run reads them/)).toBeTruthy()
    expect(screen.getByText(/has not answered as a kista/)).toBeTruthy()
  })
})
