import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { Client } from '../lib/http'
import { Holder, coreExtensions, deletable, newTenantBody } from './Server'

describe('deletable', () => {
  const now = new Date('2026-10-12T12:00:00Z')
  it('is a suspended tenant from deletable_at', () => {
    expect(deletable({ state: 'suspended', deletable_at: '2026-10-12T11:00:00Z' }, now)).toBe(true)
    expect(deletable({ state: 'suspended', deletable_at: '2026-10-13T11:00:00Z' }, now)).toBe(false)
  })
  it('is never an active, deleting or old-suspended tenant', () => {
    expect(deletable({ state: 'active', deletable_at: '2026-10-12T11:00:00Z' }, now)).toBe(false)
    expect(deletable({ state: 'deleting' }, now)).toBe(false)
    expect(deletable({ state: 'suspended' }, now)).toBe(false)
  })
})

// Spec 0028: a tenant's expected holder, given at creation, set and cleared by a server administrator.

describe('newTenantBody', () => {
  it('leaves an empty holder id out and sends one as typed', () => {
    const f = { name: 'acme', display_name: '', storage_domain: '', holder_id: '' }
    expect(newTenantBody(f)).toEqual({ name: 'acme', display_name: '', storage_domain: '' })
    expect(newTenantBody({ ...f, holder_id: 'org-7f3a' })).toEqual({ name: 'acme', display_name: '', storage_domain: '', holder_id: 'org-7f3a' })
  })
})

describe('Holder', () => {
  beforeAll(() => {
    // jsdom has no modal dialogs: open and close as the attribute
    const proto = HTMLDialogElement.prototype as { showModal?: () => void; close: () => void }
    proto.showModal ??= function (this: HTMLDialogElement) {
      this.setAttribute('open', '')
    }
    proto.close = function (this: HTMLDialogElement) {
      this.removeAttribute('open')
    }
  })
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  const problem = (status: number, detail: string) =>
    new Response(JSON.stringify({ type: 'urn:kista:problem:x', title: 'x', status, detail }), { status, headers: { 'Content-Type': 'application/problem+json' } })

  function api(answer: () => Response) {
    const calls: { method: string; path: string; ifMatch: string | null; body?: unknown }[] = []
    vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
      const h = new Headers(init?.headers)
      calls.push({ method: init?.method ?? 'GET', path: new URL(input).pathname, ifMatch: h.get('If-Match'), body: init?.body ? JSON.parse(String(init.body)) : undefined })
      return answer()
    }))
    return calls
  }

  const client = () => new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })

  it('sets the holder with the tenant ETag', async () => {
    const calls = api(() => new Response(JSON.stringify({ name: 'acme', holder_id: 'org-a' }), { status: 200 }))
    const reload = vi.fn()
    render(<Holder client={client()} t="acme" etag={'"v3"'} reload={reload} />)
    expect(screen.getByText(/none: licences of any holder/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Clear holder…' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Set holder…' }))
    const set = screen.getByRole('button', { name: 'Set' }) as HTMLButtonElement
    expect(set.disabled).toBe(true) // nothing typed
    fireEvent.change(screen.getByLabelText('holder id'), { target: { value: 'org-a' } })
    fireEvent.click(set)
    await waitFor(() => expect(reload).toHaveBeenCalledTimes(1))
    expect(calls).toEqual([{ method: 'PUT', path: '/api/v1/tenants/acme/holder', ifMatch: '"v3"', body: { holder_id: 'org-a' } }])
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('clears the holder after a confirmation', async () => {
    const calls = api(() => new Response(JSON.stringify({ name: 'acme' }), { status: 200 }))
    const reload = vi.fn()
    render(<Holder client={client()} t="acme" holder="org-a" etag={'"v4"'} reload={reload} />)
    expect(screen.getByText('org-a')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Clear holder…' }))
    fireEvent.click(screen.getByRole('button', { name: 'Clear' }))
    await waitFor(() => expect(reload).toHaveBeenCalledTimes(1))
    expect(calls).toEqual([{ method: 'DELETE', path: '/api/v1/tenants/acme/holder', ifMatch: '"v4"', body: undefined }])
  })

  it('shows kista naming the licences of another holder (409), and keeps the holder unchanged', async () => {
    const detail = 'the tenant holds licences of another holder; remove them first: vault (lic-1, holder "acme")'
    api(() => problem(409, detail))
    const reload = vi.fn()
    render(<Holder client={client()} t="acme" holder="org-a" etag={'"v4"'} reload={reload} />)
    fireEvent.click(screen.getByRole('button', { name: 'Change holder…' }))
    const input = screen.getByLabelText('holder id') as HTMLInputElement
    expect(input.value).toBe('org-a')
    expect((screen.getByRole('button', { name: 'Set' }) as HTMLButtonElement).disabled).toBe(true) // unchanged
    fireEvent.change(input, { target: { value: 'org-b' } })
    fireEvent.click(screen.getByRole('button', { name: 'Set' }))
    expect((await screen.findByRole('alert')).textContent).toContain(detail)
    expect(reload).not.toHaveBeenCalled()
  })

  it('reads the tenant again on a 412', async () => {
    api(() => problem(412, 'it changed: read it again'))
    const reload = vi.fn()
    render(<Holder client={client()} t="acme" etag={'"v2"'} reload={reload} />)
    fireEvent.click(screen.getByRole('button', { name: 'Set holder…' }))
    fireEvent.change(screen.getByLabelText('holder id'), { target: { value: 'org-a' } })
    fireEvent.click(screen.getByRole('button', { name: 'Set' }))
    expect((await screen.findByRole('alert')).textContent).toContain('The tenant changed meanwhile: it was read again.')
    expect(reload).toHaveBeenCalledTimes(1)
  })
})

// Spec 0030: a version's core extensions, pasted as kista duckdb core-extensions prints them.
describe('coreExtensions', () => {
  it('reads the names and the commit line', () => {
    expect(coreExtensions('# commit d54cbbe3df17e8e5ce68312f483b4155836112aa (duckdb/duckdb v2.0.0)\njson\n\n parquet \n')).toEqual({
      core_extensions: ['json', 'parquet'],
      core_extensions_commit: 'd54cbbe3df17e8e5ce68312f483b4155836112aa',
    })
  })
  it('refuses the community list', () => {
    expect(() => coreExtensions('# commit 250f2d603d5b2fc121ec7d1e2067668383e2f192 (duckdb/community-extensions main)\na5')).toThrow()
  })
  it('leaves the commit out when no line names one', () => {
    expect(coreExtensions('# duckdb/duckdb ./src\nicu')).toEqual({ core_extensions: ['icu'] })
  })
})
