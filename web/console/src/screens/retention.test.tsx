import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Perms, TenantContext } from '../lib/context'
import { Client } from '../lib/http'
import { type InForce, type PlanRetire, inForce, lineRows, linesOf, retentionWords, ruleBody } from '../lib/retention'
import { changes } from './Release'
import { Retention } from './Retention'
import type { Release } from './types'

// The Retention tab (spec 0042): the rule in force and the channel's own, each channel's rule, the
// pins, the versions by line, the plan's lists with Restore, and the line act; the release page's
// Restore.

beforeAll(() => {
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

const rel = (id: string, name: string, version: string, more: Partial<Release> = {}): Release => ({
  id, name, version, platform: 'linux_amd64', slot: 'v2.0.0', abi: 'cpp', build_duckdb_version: 'v2.0.0', state: 'active', visibility: 'public',
  seq: 0, body_hash: 'ab', created_at: '', origin: 'admin', source: 'tenant', selected: true, etag: '"v1"', ...more,
})

const versions = [{ duckdb_version: 'v1.5.2', c_apis: ['v1.2.0'] }, { duckdb_version: 'v2.0.0', c_apis: ['v1.5.6', 'v2.0.0'] }]

describe('retention in words and numbers', () => {
  const f: InForce = { keep: { value: 3, from: 'tenant' }, grace: { value: '720h0m0s', from: 'channel' }, missing: [] }
  it('words the rule in force, none and a field missing', () => {
    expect(inForce(f)).toBe(true)
    expect(retentionWords(f)).toBe('keep 3 · grace 720h')
    expect(retentionWords({ ...f, grace: { value: '0s', from: 'tenant' } })).toBe('keep 3 · grace 0 (purged at the next pass)')
    expect(retentionWords({ ...f, keep: { value: 'none', from: 'channel' } })).toBe('no retention (none in this channel)')
    expect(retentionWords({ keep: f.keep, grace: null, missing: ['grace'] })).toBe('no retention (no grace set)')
  })

  it('sends the whole rule: a field left empty is left out, days as hours', () => {
    expect(ruleBody('5', '')).toEqual({ keep: 5 })
    expect(ruleBody('none', '30d')).toEqual({ keep: 'none', grace: '720h' })
    expect(ruleBody('', '')).toEqual({})
  })

  it("finds a release's lines: a C API build by the maxima of its major, another by its DuckDB version", () => {
    expect(linesOf({ abi: 'c_struct', build_c_api: 'v1.2.0' }, versions)).toEqual(['v1.5.2', 'v2.0.0'])
    expect(linesOf({ abi: 'c_struct', build_c_api: 'v1.3.0' }, versions)).toEqual(['v2.0.0'])
    expect(linesOf({ abi: 'cpp', build_duckdb_version: 'v1.5.2' }, versions)).toEqual(['v1.5.2'])
    expect(linesOf({ abi: 'cpp', build_duckdb_version: 'v1.4.0' }, versions)).toEqual([])
  })

  it('counts per name and line the current, kept, next retired and retired versions', () => {
    const rs = [rel('a', 'geo', '1.0', { seq: 1 }), rel('b', 'geo', '1.1', { seq: 2 }), rel('c', 'geo', '1.2', { seq: 0 }),
      rel('d', 'geo', '0.9', { state: 'retired' }), rel('e', 'geo', '0.8', { state: 'yanked' }),
      rel('f', 'geo', '2.0', { abi: 'c_struct', build_duckdb_version: undefined, build_c_api: 'v1.2.0', seq: 3, slot: 'capi:1' }),
      rel('g', 'geo', '3.0', { selected: false, seq: 9 })]
    const retire: PlanRetire[] = [{ source: 'tenant', name: 'geo', version: '1.0', lines: ['v2.0.0'], platforms: ['linux_amd64'], releases: [],
      bytes: 0, kept: { 'v2.0.0': ['2.0', '1.2'] }, kept_by: [] }]
    expect(lineRows(rs, versions, retire)).toEqual([
      { name: 'geo', line: 'v2.0.0', current: ['2.0'], kept: ['2.0', '1.2', '1.1'], retire: ['1.0'], retired: ['0.9'] },
      { name: 'geo', line: 'v1.5.2', current: ['2.0'], kept: ['2.0'], retire: [], retired: [] },
    ])
  })

  it('offers a retired release Restore, a yank and a purge; a withdrawn entry\'s no Restore', () => {
    expect(changes(rel('a', 'geo', '1.0', { state: 'retired' }))).toEqual(['restore', 'yank', 'purge'])
    expect(changes(rel('a', 'geo', '1.0', { state: 'retired', withdrawn: true }))).toEqual(['yank', 'purge'])
  })
})

type Call = { method: string; path: string; body?: unknown; ifMatch?: string }

const json = (body: unknown, status = 200, etag = '') =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...(etag ? { ETag: etag } : {}) } })

// prod keeps 2 (its own) with the tenant's grace; geo 1.0 is retired next, 0.9 is past its grace, 0.8
// in it; a pin keeps 0.7
function api() {
  const calls: Call[] = []
  const inForceNow = { keep: { value: 2, from: 'channel' }, grace: { value: '720h0m0s', from: 'tenant' }, missing: [] }
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const u = new URL(input)
    const body = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined
    const path = u.pathname.replace('/api/v1/tenants/acme', '')
    calls.push({ method: init?.method ?? 'GET', path, body, ifMatch: (init?.headers as Record<string, string>)?.['If-Match'] })
    const method = init?.method ?? 'GET'
    if (path === '/channels/prod/retention' && method === 'PUT') return json({ channel: 'prod', keep: body.keep ?? null, grace: body.grace ?? null,
      in_force: inForceNow, checked_at: null, next_run: null }, 200, '"r5.-"')
    if (path === '/channels/prod/retention') return json({ channel: 'prod', keep: 2, grace: null, in_force: inForceNow,
      checked_at: '2026-11-05T10:00:00Z', next_run: '2026-11-06T10:00:00Z' }, 200, '"r2.-"')
    if (path === '/channels/prod/retention/plan') {
      const r = { id: 'r1', platform: 'linux_amd64', slot: 'v2.0.0', body_hash: 'ab', bytes: 2048, shared: true, shared_by: ['staging'], others: 1 }
      return json({ in_force: inForceNow, next_run: null, purge_off: false,
        retire: [{ source: 'tenant', name: 'geo', version: '1.0', lines: ['v2.0.0'], platforms: ['linux_amd64'], releases: [r], bytes: 2048,
          kept: { 'v2.0.0': ['1.2', '1.1'] }, kept_by: [] }],
        purge: [{ source: 'tenant', name: 'geo', version: '0.9', release: { ...r, id: 'r9', shared: false, shared_by: [], others: 0 },
          retired_at: '2026-09-01T00:00:00Z', purge_at: '2026-10-01T00:00:00Z', pinned: false }],
        retired: [{ source: 'tenant', name: 'geo', version: '0.8', release: { ...r, id: 'r8' }, retired_at: '2026-11-01T00:00:00Z',
          purge_at: '2026-12-01T00:00:00Z', pinned: false }] })
    }
    if (path === '/channels/prod/retention/pins' && method === 'POST') return json({ id: 'p2', ...body, pinned_by: 'me', pinned_at: '' }, 201)
    if (path === '/channels/prod/retention/pins') return json({ pins: [{ id: 'p1', name: 'geo', version: '0.7', reason: 'LTS nodes',
      pinned_by: 'tom', pinned_at: '2026-11-01T00:00:00Z' }] })
    if (path === '/channels/prod/retention/pins/p1') return new Response(null, { status: 204 })
    if (path === '/channels/prod/retention/lines') return json({ duckdb_version: body.duckdb_version, retired: [{ id: 'x' }], kept: [],
      withdrawn: 1, pins_removed: 0 })
    if (path === '/channels/prod/releases') return json({ releases: [rel('r1', 'geo', '1.0', { seq: 1 }), rel('r2', 'geo', '1.1', { seq: 2 }),
      rel('r3', 'geo', '1.2', { seq: 3 }), rel('r8', 'geo', '0.8', { state: 'retired' })] })
    if (/\/releases\/r8\/restore$/.test(path)) return json(rel('r8', 'geo', '0.8'))
    if (path === '/retention') return json({ keep: 3, grace: '720h' }, 200, '"r3.2592000"')
    if (path === '/channels') return json({ channels: [{ name: 'prod', kind: 'signed' }, { name: 'core', kind: 'passthrough' }] })
    return json({}, 404)
  }))
  return calls
}

function page() {
  const client = new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })
  return render(
    <MemoryRouter initialEntries={['/t/acme/channels/prod']}>
      <TenantContext.Provider value={{ client, tenant: 'acme', perms: new Perms(true), api: '/tenants/acme', base: '/t/acme' }}>
        <Retention channel="prod" versions={versions} />
      </TenantContext.Provider>
    </MemoryRouter>)
}

describe('the Retention tab', () => {
  it('shows the rule in force with where each field comes from, the channels, pins, lines and the plan', async () => {
    api()
    page()
    expect(await screen.findByText('keep 2 · grace 720h')).toBeTruthy()
    expect(screen.getByText("2, the channel's")).toBeTruthy()
    expect(screen.getByText("720h, the tenant's")).toBeTruthy()
    expect(await screen.findByText('Tenant default: keep 3 · grace 720h')).toBeTruthy()
    const pins = await screen.findByText('LTS nodes')
    expect(pins.closest('tr')?.textContent).toContain('every version')
    // the plan's lists: retire with why and bodies, purge, retired with Restore
    expect(await screen.findByText('v2.0.0 keeps 1.2 1.1')).toBeTruthy()
    expect(screen.getByText('kept by staging, 1 more')).toBeTruthy()
    expect(screen.getByText('purged by the next pass')).toBeTruthy()
    // the versions by line: geo's current 1.2, 1.0 retired next, 0.8 retired
    const line = (await screen.findAllByRole('row')).find((r) => r.textContent?.startsWith('geov2.0.0'))!
    expect(within(line).getAllByRole('cell').map((c) => c.textContent)).toEqual(['geo', 'v2.0.0', '1.2', '1.2 1.1', '1.0', '0.8'])
    // the passthrough channel is not listed among the channels' rules
    expect(screen.queryByText('core')).toBeNull()
  })

  it("saves the channel's whole rule with its ETag, pins and unpins, restores, and retires a line", async () => {
    const calls = api()
    page()
    const keep = await screen.findByRole('textbox', { name: 'keep' })
    fireEvent.change(keep, { target: { value: '5' } })
    fireEvent.submit(screen.getByRole('group', { name: "the channel's rule" }))
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT' && c.path === '/channels/prod/retention')).toBe(true))
    expect(calls.find((c) => c.method === 'PUT')).toMatchObject({ body: { keep: 5 }, ifMatch: '"r2.-"' })
    // a pin for one DuckDB version, with its reason
    fireEvent.change(screen.getByRole('textbox', { name: 'pin name' }), { target: { value: 'geo' } })
    fireEvent.change(screen.getByRole('textbox', { name: 'pin version' }), { target: { value: '1.1' } })
    fireEvent.change(screen.getByRole('combobox', { name: 'pin DuckDB version' }), { target: { value: 'v2.0.0' } })
    fireEvent.change(screen.getByRole('textbox', { name: 'pin reason' }), { target: { value: 'LTS' } })
    fireEvent.submit(screen.getByRole('group', { name: 'pin a version' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'POST' && c.path.endsWith('/pins'))?.body)
      .toEqual({ name: 'geo', version: '1.1', duckdb_version: 'v2.0.0', reason: 'LTS' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Unpin' }))
    fireEvent.click(within(screen.getAllByRole('dialog', { hidden: true }).find((d) => d.hasAttribute('open'))!).getByRole('button', { name: 'Unpin' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'DELETE' && c.path === '/channels/prod/retention/pins/p1')).toBe(true))
    // Restore a retired release in its grace
    fireEvent.click(await screen.findByRole('button', { name: 'Restore' }))
    await waitFor(() => expect(calls.find((c) => c.path.endsWith('/r8/restore'))).toMatchObject({ method: 'POST', ifMatch: '*',
      path: '/channels/prod/extensions/geo/releases/r8/restore' }))
    // Retire the line…
    fireEvent.click(screen.getByRole('button', { name: 'Retire the line…' }))
    const dialog = screen.getAllByRole('dialog', { hidden: true }).find((d) => d.hasAttribute('open'))!
    fireEvent.change(within(dialog).getByRole('combobox', { name: 'line to retire' }), { target: { value: 'v1.5.2' } })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Retire the line' }))
    await waitFor(() => expect(calls.find((c) => c.path.endsWith('/retention/lines'))?.body).toEqual({ duckdb_version: 'v1.5.2' }))
    expect(await screen.findByText(/1 retired, 0 kept by another DuckDB version, 1 withdrawn skipped/)).toBeTruthy()
  })
})
