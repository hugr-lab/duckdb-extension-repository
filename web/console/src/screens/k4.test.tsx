import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Perms, TenantContext } from '../lib/context'
import { Client } from '../lib/http'
import { nameFromFile, parseFooter } from '../lib/footer'
import { FrameProvider, sectionRuns, useSections } from '../lib/frame'
import { Shell } from './Shell'
import { AddBuildButton, addBuildPath, fromFile, wasmPlatform } from './AddBuild'
import { HoldForm } from './Holds'
import { Keys, firstActive } from './Keys'
import { PromoteButton, activeVersions, allActive, mayPromoteFrom, promoteBody, promoteTargets } from './Promote'
import { Credentials, ReservedNames, keptWords, removable, reserveBody, splitNames } from './ServerLists'
import { tenantSections } from './TenantArea'
import { MirrorHold, type Upstream } from './Upstreams'

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
  // nor Blob.arrayBuffer: read it as browsers did before it
  Blob.prototype.arrayBuffer ??= function (this: Blob) {
    return new Promise<ArrayBuffer>((resolve, reject) => {
      const r = new FileReader()
      r.onload = () => resolve(r.result as ArrayBuffer)
      r.onerror = () => reject(r.error)
      r.readAsArrayBuffer(this)
    })
  }
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

type Call = { method: string; path: string; search: string; body?: unknown; headers: Record<string, string> }

function api(answer: (c: Call) => Response) {
  const calls: Call[] = []
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const u = new URL(input)
    const body = typeof init?.body === 'string' ? JSON.parse(init.body) : init?.body
    const c = { method: init?.method ?? 'GET', path: u.pathname, search: u.search, body, headers: (init?.headers ?? {}) as Record<string, string> }
    calls.push(c)
    return answer(c)
  }))
  return calls
}

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

const g = (verbs: string[], more: object = {}) => ({ principal: 'p', verbs, ...more })

const tenantClient = () => new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })

function tenant(ui: React.ReactNode, perms = new Perms(true)) {
  return render(
    <MemoryRouter>
      <TenantContext.Provider value={{ client: tenantClient(), tenant: 'acme', perms, api: '/tenants/acme', base: '/t/acme' }}>{ui}</TenantContext.Provider>
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

  // the channels, the source's active releases, and the promotion's answer
  function promoting(releases: { version: string }[], status = 201) {
    return api((c) => (c.method === 'POST' ? json({ releases: [] }, status)
      : c.path.endsWith('/channels') ? json({ channels: [{ name: 'staging', kind: 'signed' }, { name: 'prod', kind: 'signed' }] })
        : json({ releases })))
  }

  it('promotes a version into the chosen channel', async () => {
    const calls = promoting([{ version: 'v1.0.0' }, { version: 'v2.0.0' }])
    tenant(<PromoteButton channel="staging" ext="geo" />)
    fireEvent.click(await screen.findByRole('button', { name: 'Promote…' }))
    fireEvent.click(screen.getByRole('button', { name: 'Promote' }))
    await screen.findByText('v2.0.0 promoted to prod')
    expect(calls.find((c) => c.path.endsWith('/releases'))?.search).toBe('?state=active&limit=500')
    expect(calls.at(-1)).toMatchObject({ method: 'POST', path: '/api/v1/tenants/acme/channels/prod/extensions/geo/releases/promote',
      body: { from_channel: 'staging', version: 'v2.0.0' } })
  })

  it('says so when the version is in the target already', async () => {
    promoting([{ version: 'v1.0.0' }], 200)
    tenant(<PromoteButton channel="staging" ext="geo" />)
    fireEvent.click(await screen.findByRole('button', { name: 'Promote…' }))
    fireEvent.click(screen.getByRole('button', { name: 'Promote' }))
    expect(await screen.findByText('v1.0.0 is in prod already')).toBeTruthy()
  })

  it('offers nothing without an active version', async () => {
    const calls = promoting([])
    tenant(<PromoteButton channel="staging" ext="geo" />)
    await waitFor(() => expect(calls.length).toBe(2))
    expect(screen.queryByRole('button', { name: 'Promote…' })).toBeNull()
  })

  it('offers nothing where no other channel takes the name', async () => {
    const calls = api((c) => (c.path.endsWith('/channels') ? json({ channels: [{ name: 'staging', kind: 'signed' }] }) : json({ releases: [{ version: 'v1' }] })))
    tenant(<PromoteButton channel="staging" ext="geo" />)
    await waitFor(() => expect(calls.length).toBe(2))
    expect(screen.queryByRole('button', { name: 'Promote…' })).toBeNull()
  })

  it('shows why the channels could not be read', async () => {
    api((c) => (c.path.endsWith('/channels') ? json({ type: 'about:blank', title: 'boom', status: 500, detail: 'the store is down' }, 500) : json({ releases: [] })))
    tenant(<PromoteButton channel="staging" ext="geo" />)
    expect(await screen.findByText(/the store is down/)).toBeTruthy()
  })

  it('reads nothing unless the caller may promote somewhere and publish here', () => {
    const calls = api(() => json({}))
    tenant(<PromoteButton channel="staging" ext="geo" />, new Perms(false, [g(['publish'], { channel: 'staging' })]))
    tenant(<PromoteButton channel="staging" ext="geo" />, new Perms(false, [g(['promote'], { channel: 'prod' })]))
    expect(calls).toEqual([])
    expect(screen.queryByRole('button', { name: 'Promote…' })).toBeNull()
  })

  it('needs promote somewhere and publish on the name in the source', () => {
    const promote = g(['promote'], { channel: 'prod' })
    expect(mayPromoteFrom(new Perms(true), 'staging', 'geo')).toBe(true)
    expect(mayPromoteFrom(new Perms(false, [promote]), 'staging', 'geo')).toBe(false)
    expect(mayPromoteFrom(new Perms(false, [promote, g(['publish'], { channel: 'staging', extension: 'geo' })]), 'staging', 'geo')).toBe(true)
    expect(mayPromoteFrom(new Perms(false, [promote, g(['publish'], { channel: 'staging', extension: 'other' })]), 'staging', 'geo')).toBe(false)
    expect(mayPromoteFrom(new Perms(false, [promote, g(['publish'], { channel: 'beta' })]), 'staging', 'geo')).toBe(false)
    expect(mayPromoteFrom(new Perms(false, [g(['publish'])]), 'staging', 'geo')).toBe(false)
  })

  it('follows the release pages to the end', async () => {
    const calls = api((c) => (c.search.includes('cursor=c1') ? json({ releases: [{ version: 'v2' }] }) : json({ releases: [{ version: 'v1' }], next: 'c1' })))
    expect((await allActive(tenantClient(), '/tenants/acme', 'staging', 'geo')).map((r) => r.version)).toEqual(['v1', 'v2'])
    expect(calls.map((c) => c.search)).toEqual(['?state=active&limit=500', '?state=active&limit=500&cursor=c1'])
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
    expect(calls).toMatchObject([{ method: 'PUT', path: '/api/v1/tenants/acme/publishers/ci/hold', search: '', body: { hold: '48h' } }])
    expect(calls[0].headers['If-Match']).toBeUndefined()
  })

  const mirror = { name: 'core', channel: 'prod', mode: 'mirror', hold: '24h0m0s' } as unknown as Upstream

  it("sets an eager mirror's hold with the upstream's ETag", async () => {
    const calls = api((c) => (c.method === 'GET' ? json({ name: 'prod', kind: 'signed' }) : json({ name: 'core', hold: '1h0m0s' })))
    const changed = vi.fn()
    tenant(<MirrorHold u={mirror} path="/tenants/acme/upstreams/core" etag={'"7"'} onChanged={changed} />)
    fireEvent.change(await screen.findByLabelText('new hold'), { target: { value: '1h' } })
    fireEvent.click(screen.getByRole('button', { name: 'Set hold' }))
    await waitFor(() => expect(changed).toHaveBeenCalled())
    expect(calls.at(-1)).toMatchObject({ method: 'PUT', path: '/api/v1/tenants/acme/upstreams/core/hold', body: { hold: '1h' } })
    expect(calls.at(-1)?.headers['If-Match']).toBe('"7"')
  })

  it("offers no hold for a passthrough channel's mirror", async () => {
    const calls = api(() => json({ name: 'prod', kind: 'passthrough' }))
    tenant(<MirrorHold u={mirror} path="/tenants/acme/upstreams/core" etag={'"7"'} onChanged={() => {}} />)
    await waitFor(() => expect(calls.length).toBe(1))
    expect(screen.queryByLabelText('new hold')).toBeNull()
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
    // nothing is kept from an earlier file: what this one does not tell is empty
    expect(fromFile('a.bin', null, { ...was, version: 'v9', platform: 'osx_arm64', visibility: 'public' }))
      .toEqual({ name: '', version: '', platform: '', visibility: 'public' })
  })

  it('refuses DuckDB-Wasm platforms', () => {
    expect(wasmPlatform('wasm_eh')).toBe(true)
    expect(wasmPlatform('linux_amd64')).toBe(false)
  })

  it('uploads to the catalogue route, public only when asked', () => {
    const a = { name: 'geo', version: 'v1.0.0', platform: 'linux_amd64', visibility: '' as const }
    expect(addBuildPath('/tenants/acme', a)).toBe('/tenants/acme/catalogue/extensions/geo/builds?version=v1.0.0&platform=linux_amd64')
    expect(addBuildPath('/tenants/acme', { ...a, visibility: 'public' })).toBe('/tenants/acme/catalogue/extensions/geo/builds?version=v1.0.0&platform=linux_amd64&visibility=public')
  })

  it('is offered to a caller with publish tenant-wide only', () => {
    expect(new Perms(true).mayPublishCatalogue).toBe(true)
    expect(new Perms(false, [g(['publish'])]).mayPublishCatalogue).toBe(true)
    expect(new Perms(false, [g(['publish'], { extension: 'geo' })]).mayPublishCatalogue).toBe(true)
    expect(new Perms(false, [g(['publish'], { channel: 'prod' })]).mayPublishCatalogue).toBe(false)
    expect(new Perms(false, [g(['admin'])]).mayPublishCatalogue).toBe(false) // admin never implies publish
  })

  function extension(name: string, fields: string[], head = new Uint8Array(64)) {
    return new File([head, footer(fields)], name)
  }

  async function choose(f: File) {
    fireEvent.click(await screen.findByRole('button', { name: 'Add a build…' }))
    fireEvent.change(screen.getByLabelText('Extension file'), { target: { files: [f] } })
  }

  const value = (label: string) => (screen.getByLabelText(label) as HTMLInputElement).value

  it('uploads the file itself, its fields from the footer', async () => {
    const calls = api(() => json({ name: 'geo' }, 201))
    const added = vi.fn()
    tenant(<AddBuildButton onAdded={added} />)
    const f = extension('geo.duckdb_extension', ['4', 'linux_amd64', 'v2.0.0', 'v1.4.2', 'CPP'])
    await choose(f)
    await waitFor(() => expect(value('Version')).toBe('v1.4.2'))
    expect(value('Name')).toBe('geo')
    expect(screen.getByLabelText('Extension file').getAttribute('accept')).toBe('.duckdb_extension')
    fireEvent.click(screen.getByRole('button', { name: 'Add' }))
    expect(await screen.findByText('geo v1.4.2 (linux_amd64) added')).toBeTruthy()
    expect(added).toHaveBeenCalled()
    expect(calls).toHaveLength(1)
    expect(calls[0]).toMatchObject({ method: 'POST', path: '/api/v1/tenants/acme/catalogue/extensions/geo/builds', search: '?version=v1.4.2&platform=linux_amd64' })
    expect(calls[0].body).toBe(f)
    expect(calls[0].headers['Content-Type']).toBe('application/octet-stream')
  })

  it('says so when the build is in the catalogue already', async () => {
    api(() => json({ name: 'geo' }, 200))
    tenant(<AddBuildButton onAdded={() => {}} />)
    await choose(extension('geo.duckdb_extension', ['4', 'linux_amd64', 'v2.0.0', 'v1', 'CPP']))
    await waitFor(() => expect(value('Version')).toBe('v1'))
    fireEvent.click(screen.getByRole('button', { name: 'Add' }))
    expect(await screen.findByText('geo v1 (linux_amd64) is in the catalogue already')).toBeTruthy()
  })

  it('asks for the uncompressed file', async () => {
    const calls = api(() => json({}))
    tenant(<AddBuildButton onAdded={() => {}} />)
    await choose(extension('geo.duckdb_extension', ['4', 'linux_amd64', 'v2.0.0', 'v1', 'CPP'], new Uint8Array([0x1f, 0x8b, 8, 0])))
    expect(await screen.findByText(/upload the uncompressed extension/)).toBeTruthy()
    expect((screen.getByRole('button', { name: 'Add' }) as HTMLButtonElement).disabled).toBe(true)
    expect(calls).toEqual([])
  })

  it('clears what an earlier file filled', async () => {
    api(() => json({}))
    tenant(<AddBuildButton onAdded={() => {}} />)
    await choose(extension('geo.duckdb_extension', ['4', 'linux_amd64', 'v2.0.0', 'v1', 'CPP']))
    await waitFor(() => expect(value('Version')).toBe('v1'))
    fireEvent.change(screen.getByLabelText('Extension file'), { target: { files: [new File([new Uint8Array(600)], 'other.bin')] } })
    expect(await screen.findByText(/footer is not an extension's/)).toBeTruthy()
    for (const l of ['Name', 'Version', 'Platform']) expect(value(l)).toBe('')
  })

  it('warns on a DuckDB-Wasm build and does not send it', async () => {
    api(() => json({}))
    tenant(<AddBuildButton onAdded={() => {}} />)
    await choose(extension('geo.duckdb_extension', ['4', 'wasm_eh', 'v2.0.0', 'v1', 'CPP']))
    expect(await screen.findByText(/does not take DuckDB-Wasm builds/)).toBeTruthy()
    expect((screen.getByRole('button', { name: 'Add' }) as HTMLButtonElement).disabled).toBe(true)
  })
})

describe("a channel's first key", () => {
  it('is active at once only when it is the first, and asked', () => {
    expect(firstActive(0, true)).toBe(true)
    expect(firstActive(0, false)).toBe(false)
    expect(firstActive(1, true)).toBe(false)
  })

  it('is sent active by default', async () => {
    const calls = api((c) => (c.method === 'POST' ? json({ id: 'k1' }, 201) : c.path.endsWith('/events') ? json({ events: [] }) : json({ keys: [] })))
    tenant(<Keys channel="prod" />)
    fireEvent.change(await screen.findByLabelText("Add the channel's first key"), { target: { value: 'file:/keys/prod.pem' } })
    expect((screen.getByRole('checkbox', { name: /active at once/ }) as HTMLInputElement).checked).toBe(true)
    fireEvent.click(screen.getByRole('button', { name: 'Add key' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    expect(calls.find((c) => c.method === 'POST')).toMatchObject({ path: '/api/v1/tenants/acme/channels/prod/keys', body: { signer_ref: 'file:/keys/prod.pem', active: true } })
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

  it('sends force only while it is offered', () => {
    expect(reserveBody('a b', 'core', true, false)).toEqual({ names: ['a', 'b'], kind: 'core', force: false })
    expect(reserveBody('a', 'core', true, true)).toEqual({ names: ['a'], kind: 'core', force: true })
  })

  it('names the tenants keeping releases by name', () => {
    expect(keptWords({ zeta: ['b'], geo: ['acme', 'beta'] })).toBe('geo: acme, beta; zeta: b')
    expect(keptWords(undefined)).toBe('')
  })

  it('reserves names and offers force on a conflict', async () => {
    let n = 0
    const calls = api((c) => {
      if (c.method === 'GET') return json({ names: [{ name: 'spatial', kind: 'core', added_at: '2026-10-01T00:00:00Z', sources: ['builtin'] }] })
      n++
      return n === 1 ? json({ type: 'urn:kista:problem:conflict', title: 'x', status: 409, detail: 'tenants have releases: acme' }, 409)
        : json({ added: ['geo'], tenants: { geo: ['acme', 'beta'] } })
    })
    render(<ReservedNames client={server()} />)
    expect(await screen.findByText('spatial')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Remove…' })).toBeNull() // builtin
    fireEvent.change(screen.getByLabelText('Names'), { target: { value: 'geo' } })
    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }))
    const force = await screen.findByRole('checkbox')
    fireEvent.click(force)
    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }))
    expect(await screen.findByText(/tenants keeping releases: geo: acme, beta/)).toBeTruthy()
    expect(screen.queryByRole('checkbox')).toBeNull() // force is offered again only by the next 409
    expect(calls.filter((c) => c.method === 'POST').map((c) => c.body)).toEqual([
      { names: ['geo'], kind: 'community', force: false },
      { names: ['geo'], kind: 'community', force: true },
    ])
  })

  it("keeps a removal's failure apart from the reserve form", async () => {
    api((c) => (c.method === 'GET' ? json({ names: [{ name: 'mine', kind: 'community', added_at: '2026-10-01T00:00:00Z', sources: ['admin:principal:ops'] }] })
      : json({ type: 'urn:kista:problem:conflict', title: 'x', status: 409, detail: 'a version brought it' }, 409)))
    render(<ReservedNames client={server()} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Remove…' }))
    fireEvent.click(screen.getByRole('button', { name: 'Remove' }))
    expect(await screen.findByText(/a version brought it/)).toBeTruthy()
    expect(screen.queryByRole('checkbox')).toBeNull() // no force for a removal
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
    const runs = sectionRuns(tabs)
    expect(runs.map((r) => r.section).filter((s) => s)).toEqual(['Organisation', 'Audit'])
    expect(runs.flatMap((r) => r.items)).toEqual(tabs)
    expect(tabs.find((t) => t.label === 'Channels')?.section).toBeUndefined()
    expect(tabs.find((t) => t.label === 'Access')?.section).toBe('Organisation')
    expect(tabs.find((t) => t.label === 'Events')?.section).toBe('Audit')
  })

  function Offer() {
    const items = tenantSections(new Perms(true), '/t/acme')
    useSections('tenant', { label: 'Tenant acme', items }, 'acme')
    return null
  }

  for (const embedded of [false, true]) {
    it(`gives each section a group its heading names (${embedded ? 'embedded' : 'standalone'})`, async () => {
      render(
        <MemoryRouter>
          <FrameProvider embedded={embedded}><Shell><Offer /></Shell></FrameProvider>
        </MemoryRouter>)
      const audit = await screen.findByRole('group', { name: 'Audit' })
      expect(within(audit).getByRole('link', { name: 'Events' })).toBeTruthy()
      expect(within(audit).queryByRole('link', { name: 'Access' })).toBeNull()
      expect(within(screen.getByRole('group', { name: 'Organisation' })).getByRole('link', { name: 'Access' })).toBeTruthy()
    })
  }
})
