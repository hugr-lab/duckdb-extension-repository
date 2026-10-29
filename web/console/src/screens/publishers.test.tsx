import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { Perms, TenantContext } from '../lib/context'
import { Client } from '../lib/http'
import { PublisherPage } from './Publishers'

// Spec 0037: a publisher's page shows its id.

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('PublisherPage', () => {
  it('shows the publisher with its id', async () => {
    vi.stubGlobal('fetch', vi.fn(async (input: string) => {
      const u = new URL(input)
      const body = u.pathname.endsWith('/publishers/ci')
        ? { id: '0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b', name: 'ci', created_at: '2026-10-01T00:00:00Z', created_by: 'tom', keys: [], license_keys: false }
        : { grants: [] }
      return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
    }))
    const client = new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })
    render(
      <MemoryRouter initialEntries={['/t/acme/publishers/ci']}>
        <TenantContext.Provider value={{ client, tenant: 'acme', perms: new Perms(false), api: '/tenants/acme', base: '/t/acme' }}>
          <Routes><Route path="/t/acme/publishers/:name" element={<PublisherPage />} /></Routes>
        </TenantContext.Provider>
      </MemoryRouter>)
    expect(await screen.findByText('0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b')).toBeTruthy()
  })
})
