import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { Perms, TenantContext } from '../lib/context'
import { Client } from '../lib/http'
import { PublisherPage, Publishers } from './Publishers'

// Spec 0037: a publisher's page shows its id; the Publishers screen shows a distributor's publisher's
// effective hold and the tenant's minimum hold for distributors' publishers (its administrators set
// it; to a caller who may not read it, nothing), and no tenant reservations (removed).

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

describe('Publishers', () => {
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
  const show = (hold: Response | (() => Response)) => {
    const calls: { method: string; path: string; body?: unknown }[] = []
    vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
      const u = new URL(input)
      const method = init?.method ?? 'GET'
      calls.push({ method, path: u.pathname, body: init?.body ? JSON.parse(String(init.body)) : undefined })
      if (u.pathname.endsWith('/publishers')) {
        return json({ publishers: [{ id: 'p1', name: 'enterest-ci', created_at: '2026-10-01T00:00:00Z', created_by: 'da', keys: [],
          license_keys: false, distributor: 'enterest', effective_hold: '24h0m0s' }] })
      }
      if (u.pathname.endsWith('/distributor-hold')) return method === 'PUT' ? json({ hold: '0s' }) : typeof hold === 'function' ? hold() : hold.clone()
      return json({}, 404)
    }))
    const client = new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })
    render(
      <MemoryRouter initialEntries={['/t/acme/publishers']}>
        <TenantContext.Provider value={{ client, tenant: 'acme', perms: new Perms(false), api: '/tenants/acme', base: '/t/acme' }}>
          <Routes><Route path="/t/acme/publishers" element={<Publishers />} /></Routes>
        </TenantContext.Provider>
      </MemoryRouter>)
    return calls
  }

  it('shows the minimum hold, sets it, and reads no tenant reservations', async () => {
    const calls = show(() => json({ hold: '24h0m0s' }))
    expect(await screen.findByText("Minimum hold for distributors' publishers")).toBeTruthy()
    expect(await screen.findAllByText('24h')).toHaveLength(2) // the publisher's effective hold, and the minimum
    fireEvent.change(screen.getByLabelText('new minimum hold'), { target: { value: '0' } })
    fireEvent.click(screen.getByText('Set minimum'))
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true))
    expect(calls.find((c) => c.method === 'PUT')).toEqual({ method: 'PUT', path: '/api/v1/tenants/acme/distributor-hold', body: { hold: '0' } })
    expect(calls.some((c) => c.path.includes('reserved-names'))).toBe(false)
  })

  it('shows nothing of the minimum to a caller who may not read it', async () => {
    show(() => json({ type: 'urn:kista:problem:not-found', title: 'Not Found', status: 404 }, 404))
    expect(await screen.findByText('enterest-ci')).toBeTruthy()
    await waitFor(() => expect(screen.queryByText("Minimum hold for distributors' publishers")).toBeNull())
  })
})
