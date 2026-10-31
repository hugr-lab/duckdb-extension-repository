import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Perms, TenantContext } from '../lib/context'
import { Client } from '../lib/http'
import { nameFromFile, parseFooter } from '../lib/footer'
import { sectionStart } from '../lib/frame'
import { addBuildPath, fromFile } from './AddBuild'
import { activeVersions } from './Channel'
import { HoldForm } from './Holds'
import { firstActive } from './Keys'
import { PromoteButton, promoteBody, promoteTargets } from './Promote'
import { Credentials, ReservedNames, removable, splitNames } from './ServerLists'
import { tenantSections } from './TenantArea'

// Console gaps (K4): the promotion dialog, holds set in place, a build added to the catalogue, the
// server's reserved names and credentials, a channel's first key active at once, and a tenant's
// navigation in sections.

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

type Call = { method: string; path: string; search: string; body?: unknown }

function api(answer: (c: Call) => Response) {
  const calls: Call[] = []
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const u = new URL(input)
    const body = typeof init?.body === 'string' ? JSON.parse(init.body) : init?.body
    const c = { method: init?.method ?? 'GET', path: u.pathname, search: u.search, body }
    calls.push(c)
    return answer(c)
  }))
  return calls
}

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

function tenant(ui: React.ReactNode, perms = new Perms(true)) {
  const client = new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })
  return render(
    <MemoryRouter>
      <TenantContext.Provider value={{ client, tenant: 'acme', perms, api: '/tenants/acme', base: '/t/acme' }}>{ui}</TenantContext.Provider>
    </MemoryRouter>)
}

const server = () => new Client({ base: 'https://k.example/api/v1', prefix: '/', getToken: async () => 'tok' })

describe('promotion', () => {
  it('sends the version from the source channel, narrowed only when asked', () => {
    const p = { target: 'prod', version: 'v1.2.0', narrow: false, notCurrent: false }
    expect(promoteBody('staging', p)).toEqual({ from_channel: 'staging', version: 'v1.2.0' })
    expect(promoteBody('staging', { ...p, narrow: true, notCurrent: true })).toEqual({ from_channel: 'staging', version: 'v1.2.0', visibility: 'private', current: false })
  })

  it('offers the other signed channels of the same type the caller may promote to', () => {
    const chans = [
      { name: 'staging', kind: 'signed' },
      { name: 'prod', kind: 'signed' },
      { name: 'core', kind: 'passthrough' },
      { name: 'drivers', kind: 'signed', package_type: 'adbc_driver' },
      { name: 'nightly', kind: 'signed' },
    ]
    expect(promoteTargets(chans, 'staging', undefined, () => true)).toEqual(['prod', 'nightly'])
    expect(promoteTargets(chans, 'staging', undefined, (c) => c === 'prod')).toEqual(['prod'])
  })

  it('lists the active versions newest first, once each', () => {
    expect(activeVersions([{ version: 'v1' }, { version: 'v1' }, { version: 'v2' }])).toEqual(['v2', 'v1'])
  })

  it('promotes a version into the chosen channel', async () => {
    const calls = api((c) => (c.path.endsWith('/channels') ? json({ channels: [{ name: 'staging', kind: 'signed' }, { name: 'prod', kind: 'signed' }] })
      : json({ releases: [] }, 201)))
    tenant(<PromoteButton channel="staging" ext="geo" versions={['v2.0.0', 'v1.0.0']} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Promote…' }))
    fireEvent.click(screen.getByRole('button', { name: 'Promote' }))
    await screen.findByText('v2.0.0 promoted to prod')
    expect(calls.at(-1)).toMatchObject({ method: 'POST', path: '/api/v1/tenants/acme/channels/prod/extensions/geo/releases/promote',
      body: { from_channel: 'staging', version: 'v2.0.0' } })
  })

  it('offers nothing where no other channel takes the name', async () => {
    api(() => json({ channels: [{ name: 'staging', kind: 'signed' }] }))
    tenant(<PromoteButton channel="staging" ext="geo" versions={['v1']} />)
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Promote…' })).toBeNull())
  })
})

describe('holds', () => {
  it('sets a hold, days as hours, and reads the owner again', async () => {
    const calls = api(() => json({ name: 'ci', hold: '48h0m0s' }))
    const changed = vi.fn()
    tenant(<HoldForm path="/tenants/acme/publishers/ci/hold" label="publisher hold" current="24h0m0s" note="n" onChanged={changed} />)
    expect(screen.getByText('24h')).toBeTruthy()
    fireEvent.change(screen.getByLabelText('new hold'), { target: { value: '2d' } })
    fireEvent.click(screen.getByRole('button', { name: 'Set hold' }))
    await waitFor(() => expect(changed).toHaveBeenCalled())
    expect(calls).toEqual([{ method: 'PUT', path: '/api/v1/tenants/acme/publishers/ci/hold', search: '', body: { hold: '48h' } }])
  })
})

describe('adding a build to the catalogue', () => {
  // a footer: the metadata block (eight 32-byte fields, reversed), then a 256-byte signature
  function footer(fields: string[]) {
    const b = new Uint8Array(512)
    fields.forEach((f, i) => b.set(new TextEncoder().encode(f), (7 - i) * 32))
    return b
  }

  it('reads the platform, version and DuckDB version from a footer', () => {
    expect(parseFooter(footer(['4', 'linux_amd64', 'v2.0.0', 'v1.4.2', 'CPP']))).toEqual({ platform: 'linux_amd64', version: 'v1.4.2', duckdb: 'v2.0.0', abi: 'CPP' })
    expect(parseFooter(footer(['4', 'osx_arm64', 'v0.0.1', 'v3', 'C_STRUCT']))?.abi).toBe('C_STRUCT')
  })

  it('refuses what is not an extension footer', () => {
    expect(parseFooter(new Uint8Array(100))).toBeNull()
    expect(parseFooter(footer(['3', 'linux_amd64', 'v2.0.0', 'v1', 'CPP']))).toBeNull()
    expect(parseFooter(footer(['4', 'linux amd64', 'v2.0.0', 'v1', 'CPP']))).toBeNull()
  })

  it('takes the name from the file name', () => {
    expect(nameFromFile('geo_tools.duckdb_extension')).toBe('geo_tools')
    expect(nameFromFile('geo.duckdb_extension.gz')).toBeNull()
    const was = { name: 'x', version: '', platform: '', visibility: '' as const }
    expect(fromFile('geo.duckdb_extension', { platform: 'linux_amd64', version: 'v1', duckdb: 'v2.0.0', abi: 'CPP' }, was))
      .toEqual({ name: 'geo', version: 'v1', platform: 'linux_amd64', visibility: '' })
    expect(fromFile('a.bin', null, was)).toEqual(was)
  })

  it('uploads to the catalogue route, public only when asked', () => {
    const a = { name: 'geo', version: 'v1.0.0', platform: 'linux_amd64', visibility: '' as const }
    expect(addBuildPath('/tenants/acme', a)).toBe('/tenants/acme/catalogue/extensions/geo/builds?version=v1.0.0&platform=linux_amd64')
    expect(addBuildPath('/tenants/acme', { ...a, visibility: 'public' })).toBe('/tenants/acme/catalogue/extensions/geo/builds?version=v1.0.0&platform=linux_amd64&visibility=public')
  })

  it('is offered to a caller with publish tenant-wide only', () => {
    const g = (verbs: string[], more: object = {}) => ({ principal: 'p', verbs, ...more })
    expect(new Perms(true).mayPublishCatalogue).toBe(true)
    expect(new Perms(false, [g(['publish'])]).mayPublishCatalogue).toBe(true)
    expect(new Perms(false, [g(['publish'], { extension: 'geo' })]).mayPublishCatalogue).toBe(true)
    expect(new Perms(false, [g(['publish'], { channel: 'prod' })]).mayPublishCatalogue).toBe(false)
    expect(new Perms(false, [g(['admin'])]).mayPublishCatalogue).toBe(false) // admin never implies publish
  })
})

describe("a channel's first key", () => {
  it('is active at once only when it is the first, and asked', () => {
    expect(firstActive(0, true)).toBe(true)
    expect(firstActive(0, false)).toBe(false)
    expect(firstActive(1, true)).toBe(false)
  })
})

describe('reserved names', () => {
  it('removes only names administrators alone reserved', () => {
    expect(removable({ sources: ['admin:principal:ops'] })).toBe(true)
    expect(removable({ sources: ['admin:principal:ops', 'builtin'] })).toBe(false)
    expect(removable({ sources: ['version:v2.0.0@abc'] })).toBe(false)
    expect(removable({})).toBe(false)
  })

  it('splits the names typed', () => {
    expect(splitNames('a, b  c,a\nd')).toEqual(['a', 'b', 'c', 'd'])
  })

  it('reserves names and offers force on a conflict', async () => {
    let n = 0
    const calls = api((c) => {
      if (c.method === 'GET') return json({ names: [{ name: 'spatial', kind: 'core', added_at: '2026-10-01T00:00:00Z', sources: ['builtin'] }] })
      n++
      return n === 1 ? json({ type: 'urn:kista:problem:conflict', title: 'x', status: 409, detail: 'tenants have releases: acme' }, 409)
        : json({ added: ['geo'], tenants: ['acme'] })
    })
    render(<ReservedNames client={server()} />)
    expect(await screen.findByText('spatial')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Remove…' })).toBeNull() // builtin
    fireEvent.change(screen.getByLabelText('Names'), { target: { value: 'geo' } })
    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }))
    const force = await screen.findByRole('checkbox')
    fireEvent.click(force)
    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }))
    expect(await screen.findByText(/tenants keeping releases: acme/)).toBeTruthy()
    expect(calls.filter((c) => c.method === 'POST').map((c) => c.body)).toEqual([
      { names: ['geo'], kind: 'community', force: false },
      { names: ['geo'], kind: 'community', force: true },
    ])
  })

  it('lists the credentials without their settings', async () => {
    api(() => json({ credentials: [{ name: 'gh', kind: 'token_file', tenants: ['*'], prefixes: ['https://ext.example/acme/prod'], allow_public: false,
      forwards_usage: [{ tenant: 'acme', upstream: 'up' }] }] }))
    render(<Credentials client={server()} />)
    expect(await screen.findByText('gh')).toBeTruthy()
    expect(screen.getByText('every tenant')).toBeTruthy()
    expect(screen.getByText('acme/up')).toBeTruthy()
  })
})

describe("a tenant's navigation", () => {
  it('groups the organisation and the record under headings', () => {
    const tabs = tenantSections(new Perms(true), '/t/acme')
    const heads = tabs.map((_, i) => sectionStart(tabs, i)).filter((s) => s)
    expect(heads).toEqual(['Organisation', 'Audit'])
    expect(tabs.find((t) => t.label === 'Channels')?.section).toBeUndefined()
    expect(tabs.find((t) => t.label === 'Access')?.section).toBe('Organisation')
    expect(tabs.find((t) => t.label === 'Events')?.section).toBe('Audit')
  })
})
