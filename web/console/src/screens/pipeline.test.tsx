import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { type Grant, Perms, TenantContext } from '../lib/context'
import { HomeProvider } from '../lib/home'
import { Client } from '../lib/http'
import { type IndexRelease, cellWaits, chainEdges, chainsOf, inOrder, previousOf, stageWait } from '../lib/order'
import { type ChannelReads, type Decision, promoteFrom } from '../lib/queue'
import { Channels } from './Channels'

// The channels' order and the pipeline (spec 0043): the stages from fixtures, what waits for a stage
// per cell from its previous stage's current, a skip leaving a version out, Promote filled in from the
// previous stage, and a channel administrator seeing the list only.

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

const rel = (version: string, platform: string, more: Partial<IndexRelease> = {}): IndexRelease =>
  ({ version, platform, state: 'active', serves: [{ duckdb_version: 'v2.0.0' }], current_for: [], ...more })

const skip = (d: Partial<Decision>): Decision => ({ id: 'd1', kind: 'skip', source: 'tenant', name: 'relay', decided_by: 'x', decided_at: '', etag: '"v1"', ...d })

describe('the order', () => {
  const order = [{ channel: 'staging', next: 'prod' }, { channel: 'nightly', next: 'staging' }, { channel: 'dev', next: 'qa' }]
  it('draws its chains from their first stages, and the columns in its sequence, then the others by name', () => {
    expect(chainsOf(order)).toEqual([['dev', 'qa'], ['nightly', 'staging', 'prod']])
    expect(inOrder(['prod', 'beta', 'staging', 'nightly', 'alpha', 'qa'], order)).toEqual(['qa', 'nightly', 'staging', 'prod', 'alpha', 'beta'])
    expect(previousOf(order, 'prod')).toBe('staging')
    expect(previousOf(order, 'nightly')).toBe('')
    expect(chainEdges(['nightly', 'staging', 'prod'])).toEqual([{ channel: 'nightly', next: 'staging' }, { channel: 'staging', next: 'prod' }])
  })

  it('promotes from the previous stage first when it holds the version', () => {
    const vs = [{ version: '1.0', builds: [{ placed: [{ channel: 'beta', state: 'active' }, { channel: 'staging', state: 'active' }] }] }]
    expect(promoteFrom(vs, '1.0', 'prod')).toBe('beta')
    expect(promoteFrom(vs, '1.0', 'prod', () => true, 'staging')).toBe('staging')
    expect(promoteFrom(vs, '1.0', 'prod', () => true, 'nightly')).toBe('beta') // the previous stage does not hold it
  })
})

describe('what waits for a stage', () => {
  const nightly = [rel('1.1', 'linux_amd64', { current_for: ['v2.0.0'] }), rel('1.0', 'linux_amd64'), rel('1.2', 'osx_arm64', { current_for: ['v2.0.0'] })]

  it("waits per cell where the previous stage's current is newer than everything the stage's cell has or had", () => {
    expect(cellWaits(nightly, [rel('1.0', 'linux_amd64', { current_for: ['v2.0.0'] })], ['v2.0.0'])).toEqual([
      { duckdb: 'v2.0.0', platform: 'linux_amd64', version: '1.1' },
      { duckdb: 'v2.0.0', platform: 'osx_arm64', version: '1.2' }, // the stage has nothing on this platform
    ])
    // the stage had 1.1, yanked (it would serve the cell), and 1.2 deprecated: neither waits again
    const had = [rel('1.1', 'linux_amd64', { state: 'yanked', serves: [], would_serve: ['v2.0.0'] }), rel('1.2', 'osx_arm64', { state: 'deprecated' })]
    expect(cellWaits(nightly, had, ['v2.0.0'])).toEqual([])
    // a cell of another DuckDB version is the stage's only when it serves it
    expect(cellWaits(nightly, [], ['v2.1.0'])).toEqual([])
  })

  it('a stage with no release of the name waits wherever the previous stage has a current', () => {
    expect(cellWaits(nightly, [], ['v2.0.0']).map((c) => c.version)).toEqual(['1.1', '1.2'])
  })

  it('offers the newest of the cells, and leaves out what a skip covers', () => {
    const cells = cellWaits(nightly, [], ['v2.0.0'])
    expect(stageWait(cells, [], 'staging', 'tenant', 'relay')?.version).toBe('1.2')
    // a skip of 1.1: 1.2 still waits; a skip of 1.2 covers both
    expect(stageWait(cells, [skip({ channel: 'staging', version: '1.1' })], 'staging', 'tenant', 'relay')).toEqual({ version: '1.2', cells: [cells[1]] })
    expect(stageWait(cells, [skip({ channel: 'staging', version: '1.2' })], 'staging', 'tenant', 'relay')).toBeNull()
    // another channel's skip, another source's: not this stage's
    expect(stageWait(cells, [skip({ channel: 'prod', version: '1.2' }), skip({ channel: 'staging', source: 'upstream:x', version: '1.2' })],
      'staging', 'tenant', 'relay')?.version).toBe('1.2')
  })
})

type Call = { method: string; path: string; body?: unknown; headers: Record<string, string> }

const json = (body: unknown, status = 200, etag = '') =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...(etag ? { ETag: etag } : {}) } })

// nightly → staging → prod; relay 1.0 is nightly's current, staging's rule manual with no release of
// it; beacon 2.0 is nightly's current too, staging's rule taking it from another source
function api(decisions: Decision[] = []) {
  const calls: Call[] = []
  const channels: ChannelReads[] = [
    { name: 'nightly', hold: '0s', follows: [], sources: [{ name: 'relay', source: 'tenant', current: '1.0', available: {} },
      { name: 'beacon', source: 'tenant', current: '2.0', available: {} }] },
    { name: 'prod', hold: '0s', follows: [], sources: [] },
    { name: 'staging', hold: '24h0m0s', follows: [], sources: [{ name: 'relay', source: 'tenant', mode: 'manual', available: {} },
      { name: 'beacon', source: 'upstream:core', mode: 'auto', available: {} }] },
  ]
  let order = [{ channel: 'nightly', next: 'staging' }, { channel: 'staging', next: 'prod' }]
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const u = new URL(input)
    const body = typeof init?.body === 'string' ? JSON.parse(init.body) : init?.body
    const c = { method: init?.method ?? 'GET', path: u.pathname.replace('/api/v1/tenants/acme', ''), body, headers: (init?.headers ?? {}) as Record<string, string> }
    calls.push(c)
    if (c.method === 'POST' && c.path.endsWith('/releases/promote')) return json({ releases: [] }, 201)
    if (c.method === 'PUT' && c.path === '/decisions') return json({ ...body, id: 'd9', etag: '"v1"' }, 201)
    if (c.method === 'PUT' && c.path === '/channel-order') {
      order = body.order
      return json({ order }, 200, '"o2"')
    }
    if (c.path === '/catalogue') return json({ entries: [] })
    if (c.path === '/licenses') return json({ licenses: [] })
    if (c.path === '/channel-order') return json({ order }, 200, '"o1"')
    if (c.path === '/channels') return json({ channels: channels.map((x) => ({ name: x.name, kind: 'signed' })) })
    if (c.path === '/decisions') return json({ decisions })
    let m = /^\/channels\/([^/]+)\/(sources|follows|hold)$/.exec(c.path)
    const ch = channels.find((x) => x.name === m?.[1])
    if (m && ch) return json(m[2] === 'sources' ? { sources: ch.sources } : m[2] === 'follows' ? { follows: ch.follows } : { hold: ch.hold })
    if ((m = /^\/channels\/([^/]+)$/.exec(c.path))) return json({ name: m[1], duckdb_versions: [{ duckdb_version: 'v2.0.0' }] })
    if ((m = /^\/channels\/nightly\/extensions\/(relay|beacon)$/.exec(c.path))) {
      const v = m[1] === 'relay' ? '1.0' : '2.0'
      return json({ releases: [{ version: v, platform: 'linux_amd64', state: 'active', serves: [{ duckdb_version: 'v2.0.0' }], current_for: ['v2.0.0'], body_hash: 'ab' }] })
    }
    if (/^\/channels\/[^/]+\/extensions\/[^/]+$/.test(c.path)) return json({ releases: [] })
    if (c.path.endsWith('/attachments')) return json({ attachments: [] })
    return json({}, 404)
  }))
  return calls
}

function page(grants: Grant[] | 'server', withHome = true) {
  const client = new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })
  const perms = grants === 'server' ? new Perms(true) : new Perms(false, grants)
  return render(
    <MemoryRouter initialEntries={['/t/acme/channels']}>
      <TenantContext.Provider value={{ client, tenant: 'acme', perms, api: '/tenants/acme', base: '/t/acme' }}>
        {withHome ? <HomeProvider><Channels /></HomeProvider> : <Channels />}
      </TenantContext.Provider>
    </MemoryRouter>)
}

describe('the pipeline', () => {
  it("draws the stages, what waits for staging, and promotes nightly's current with the Report & licence panel before it", async () => {
    const calls = api()
    page('server')
    fireEvent.click(await screen.findByRole('button', { name: 'Pipeline' }))
    const stages = await screen.findAllByRole('group', { name: /^stage / })
    expect(stages.map((s) => s.getAttribute('aria-label'))).toEqual(['stage nightly', 'stage staging', 'stage prod'])
    expect(within(stages[1]).getByText(/rules: 1 auto, 1 manual/)).toBeTruthy()
    expect(within(stages[1]).getByText(/hold 24h/)).toBeTruthy()
    expect(await screen.findByText('1 version waits for staging')).toBeTruthy()
    // beacon: staging serves it from another source, which is no wait
    expect(screen.getByText('upstream:core')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Report & licence' }))
    const panel = screen.getByRole('region', { name: 'relay report and licence' })
    expect(within(panel).getByText(/is nightly's current on v2.0.0 linux_amd64/)).toBeTruthy()
    fireEvent.click(within(panel).getByRole('button', { name: 'Promote…' }))
    const dialog = screen.getAllByRole('dialog', { hidden: true }).find((d) => d.hasAttribute('open'))!
    expect(within(dialog).getByText('Promote relay from nightly')).toBeTruthy()
    expect((within(dialog).getByLabelText('To channel') as HTMLSelectElement).value).toBe('staging')
    expect((within(dialog).getByLabelText('Version') as HTMLInputElement).value).toBe('1.0')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Promote' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    expect(calls.find((c) => c.method === 'POST')).toMatchObject({ path: '/channels/staging/extensions/relay/releases/promote',
      body: { from_channel: 'nightly', version: '1.0' } })
    expect(await screen.findByText('1.0 promoted to staging')).toBeTruthy()
  })

  it('skips a waiting version under a manual rule, and a skipped version is left out of the count', async () => {
    const calls = api()
    page('server')
    fireEvent.click(await screen.findByRole('button', { name: 'Pipeline' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Report & licence' }))
    fireEvent.click(within(screen.getByRole('region', { name: 'relay report and licence' })).getByRole('button', { name: 'Skip' }))
    const dialog = screen.getAllByRole('dialog', { hidden: true }).find((d) => d.hasAttribute('open'))!
    fireEvent.click(within(dialog).getByRole('button', { name: 'Skip' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true))
    expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ kind: 'skip', channel: 'staging', source: 'tenant', name: 'relay', version: '1.0' })
    cleanup()
    api([skip({ channel: 'staging', version: '1.0' })])
    page('server')
    fireEvent.click(await screen.findByRole('button', { name: 'Pipeline' }))
    expect(await screen.findByText('Nothing waits for staging')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Report & licence' })).toBeNull()
  })

  it('offers no Promote without promote in the stage and publish in the previous one', async () => {
    api()
    page([{ principal: 'p', verbs: ['admin'] }])
    fireEvent.click(await screen.findByRole('button', { name: 'Pipeline' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Report & licence' }))
    const panel = screen.getByRole('region', { name: 'relay report and licence' })
    expect(within(panel).queryByRole('button', { name: 'Promote…' })).toBeNull()
    expect(within(panel).getByRole('button', { name: 'Skip' })).toBeTruthy()
  })

  it('sets the order as each channel names its next, at the ETag read', async () => {
    const calls = api()
    page('server')
    fireEvent.click(await screen.findByRole('button', { name: 'Pipeline' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Edit the order…' }))
    const next = await screen.findByLabelText('next of staging')
    fireEvent.change(next, { target: { value: '' } })
    const dialog = screen.getAllByRole('dialog', { hidden: true }).find((d) => d.hasAttribute('open'))!
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT' && c.path === '/channel-order')).toBe(true))
    const put = calls.find((c) => c.method === 'PUT' && c.path === '/channel-order')!
    expect(put.body).toEqual({ order: [{ channel: 'nightly', next: 'staging' }] })
    expect(put.headers['If-Match']).toBe('"o1"')
  })

  it("is a tenant administrator's: a channel's administrator sees the list of its channels only", async () => {
    api()
    page([{ principal: 'p', verbs: ['admin'], channel: 'prod' }], false)
    expect(await screen.findByRole('link', { name: 'prod' })).toBeTruthy()
    expect(screen.queryByRole('link', { name: 'staging' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Pipeline' })).toBeNull()
    expect(screen.queryByRole('button', { name: /order/ })).toBeNull()
  })
})
