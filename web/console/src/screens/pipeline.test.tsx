import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { type Grant, Perms, TenantContext } from '../lib/context'
import { HomeProvider } from '../lib/home'
import { Client } from '../lib/http'
import { type IndexRelease, cellWaits, chainsOf, inOrder, orderBody, previousOf, slotOf, stageWait } from '../lib/order'
import { type ChannelReads, type Decision, promoteFrom } from '../lib/queue'
import { Channels } from './Channels'
import { Decisions } from './Decisions'
import { rulesByMode } from './Pipeline'

// The channels' order and the pipeline (spec 0043): the stages from fixtures, what waits for a stage
// per cell from its previous stage's current (as the placement pass finds it), a skip leaving a
// version out, the chain's matrix, Promote filled in from the previous stage, the order dialog, and a
// channel administrator seeing the list only.

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
  ({ version, platform, state: 'active', abi: 'cpp', build_duckdb_version: 'v2.0.0', serves: [{ duckdb_version: 'v2.0.0' }], current_for: [], ...more })

const skip = (d: Partial<Decision>): Decision => ({ id: 'd1', kind: 'skip', source: 'tenant', name: 'relay', decided_by: 'x', decided_at: '', etag: '"v1"', ...d })

const openDialog = () => screen.getAllByRole('dialog', { hidden: true }).find((d) => d.hasAttribute('open'))!

describe('the order', () => {
  const order = [{ channel: 'staging', next: 'prod' }, { channel: 'nightly', next: 'staging' }, { channel: 'dev', next: 'qa' }]
  it('draws its chains from their first stages, and the columns in its sequence, then the others by name', () => {
    expect(chainsOf(order)).toEqual([['dev', 'qa'], ['nightly', 'staging', 'prod']])
    expect(inOrder(['prod', 'beta', 'staging', 'nightly', 'alpha', 'qa'], order)).toEqual(['qa', 'nightly', 'staging', 'prod', 'alpha', 'beta'])
    expect(previousOf(order, 'prod')).toBe('staging')
    expect(previousOf(order, 'nightly')).toBe('')
  })

  it("keeps the edges of channels the dialog does not list (another type's, one it never read)", () => {
    expect(orderBody(order, ['nightly', 'staging', 'prod'], { nightly: 'staging', staging: '' }))
      .toEqual([{ channel: 'dev', next: 'qa' }, { channel: 'nightly', next: 'staging' }])
  })

  it('promotes from the previous stage first when it holds the version', () => {
    const vs = [{ version: '1.0', builds: [{ placed: [{ channel: 'beta', state: 'active' }, { channel: 'staging', state: 'active' }] }] }]
    expect(promoteFrom(vs, '1.0', 'prod')).toBe('beta')
    expect(promoteFrom(vs, '1.0', 'prod', () => true, 'staging')).toBe('staging')
    expect(promoteFrom(vs, '1.0', 'prod', () => true, 'nightly')).toBe('beta') // the previous stage does not hold it
  })

  it('counts a stage\'s rules by mode, "as it lands" ones too', () => {
    const ch: ChannelReads = { name: 'staging', hold: '0s', follows: [], sources: [{ name: 'a', source: 'tenant', mode: 'manual', available: {} },
      { name: 'b', source: 'tenant', available: {} }, { name: 'c', source: '', available: {} }] }
    expect(rulesByMode(ch)).toBe('1 as it lands, 1 manual')
    expect(rulesByMode({ ...ch, sources: [] })).toBe('none')
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
    // the stage purged 1.1 on linux_amd64 (a cell's), and 1.2 of another DuckDB version: 1.1 waits there no more
    const purged = [{ version: '1.1', platform: 'linux_amd64', slot: 'v2.0.0', duckdb_version: 'v2.0.0' },
      { version: '1.2', platform: 'osx_arm64', slot: 'v2.1.0', duckdb_version: 'v2.1.0' }]
    expect(cellWaits(nightly, [], ['v2.0.0'], purged)).toEqual([{ duckdb: 'v2.0.0', platform: 'osx_arm64', version: '1.2' }])
    // a cell of another DuckDB version is the stage's only when it serves it
    expect(cellWaits(nightly, [], ['v2.1.0'])).toEqual([])
  })

  it('a purged C API build is had in its slot: the same version never waits again there', () => {
    const capi = (v: string, more: Partial<IndexRelease> = {}) => rel(v, 'linux_amd64', { abi: 'c_struct', build_duckdb_version: undefined, build_c_api: 'v1.2.0', ...more })
    expect(slotOf(capi('2.0'))).toBe('capi:1')
    expect(slotOf(rel('2.0', 'linux_amd64'))).toBe('v2.0.0')
    const prev = [capi('2.0', { current_for: ['v2.0.0'] })]
    expect(cellWaits(prev, [], ['v2.0.0']).map((c) => c.version)).toEqual(['2.0'])
    const purged = [{ version: '2.0', platform: 'linux_amd64', slot: 'capi:1', duckdb_version: '' }]
    expect(cellWaits(prev, [], ['v2.0.0'], purged)).toEqual([])
    // another major's slot was never had
    expect(cellWaits(prev, [], ['v2.0.0'], [{ ...purged[0], slot: 'capi:2' }]).map((c) => c.version)).toEqual(['2.0'])
  })

  it("a C API build waits nowhere when one of the stage's cells it serves has or had a newer version", () => {
    const capi = rel('1.5', 'linux_amd64', { abi: 'c_struct', build_duckdb_version: undefined, build_c_api: 'v1.2.0',
      serves: [{ duckdb_version: 'v1.4.0' }, { duckdb_version: 'v1.5.0' }], current_for: ['v1.4.0', 'v1.5.0'] })
    const staging = [rel('1.6', 'linux_amd64', { build_duckdb_version: 'v1.5.0', serves: [{ duckdb_version: 'v1.5.0' }], current_for: ['v1.5.0'] })]
    expect(cellWaits([capi], staging, ['v1.4.0', 'v1.5.0'])).toEqual([])
    // with staging's 1.6 gone, both cells wait; with staging serving v1.4.0 only, v1.5.0 of no stage cell still counts
    expect(cellWaits([capi], [], ['v1.4.0', 'v1.5.0']).map((c) => c.duckdb)).toEqual(['v1.4.0', 'v1.5.0'])
    expect(cellWaits([capi], staging, ['v1.4.0']).map((c) => c.duckdb)).toEqual(['v1.4.0'])
  })

  it('a version the stage holds (an active release on any cell) never waits: the pass completes it', () => {
    // 1.2 active on linux_amd64 (newer than nightly's 1.1 there): osx_arm64's 1.2 is the pass's to complete
    const held = [rel('1.2', 'linux_amd64', { current_for: ['v2.0.0'] })]
    expect(cellWaits(nightly, held, ['v2.0.0'])).toEqual([])
    // a withdrawn or deprecated release holds it for no one: it still waits on the other platform
    expect(cellWaits(nightly, [{ ...held[0], withdrawn: true }], ['v2.0.0']).map((c) => c.version)).toEqual(['1.2'])
    expect(cellWaits(nightly, [{ ...held[0], state: 'deprecated' }], ['v2.0.0']).map((c) => c.version)).toEqual(['1.2'])
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

type Call = { method: string; path: string; search: string; body?: unknown; headers: Record<string, string> }

const json = (body: unknown, status = 200, etag = '') =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...(etag ? { ETag: etag } : {}) } })

interface Fake {
  decisions?: Decision[]
  stagingPurged?: unknown[]
  /** the staging index of relay fails */
  failIndex?: boolean
  /** relay's build in nightly has no body hash */
  noBody?: boolean
  /** the order's PUT answers 412 once */
  stale?: boolean
  /** the order's PUT answers 400 once */
  refuse?: boolean
  /** more edges in the order than the home's channels name */
  extra?: { channel: string; next: string }[]
}

// nightly → staging → prod; relay 1.0 is nightly's current, staging's rule manual with no release of
// it (the server's waiting says 1.0 too); beacon 2.0 is nightly's current too, staging's rule taking
// it from another source; the catalogue's relay 1.0 is placed in beta and nightly
function api(f: Fake = {}) {
  const calls: Call[] = []
  const channels: ChannelReads[] = [
    { name: 'beta', hold: '0s', follows: [], sources: [] },
    { name: 'nightly', hold: '0s', follows: [], sources: [{ name: 'relay', source: 'tenant', current: '1.0', available: {} },
      { name: 'beacon', source: 'tenant', current: '2.0', available: {} }] },
    { name: 'prod', hold: '0s', follows: [], sources: [] },
    { name: 'staging', hold: '24h0m0s', follows: [], sources: [{ name: 'relay', source: 'tenant', mode: 'manual', waiting: '1.0', available: {} },
      { name: 'beacon', source: 'upstream:core', mode: 'auto', available: {} }, { name: 'lantern', source: 'tenant', available: {} }] },
  ]
  let order = [{ channel: 'nightly', next: 'staging' }, { channel: 'staging', next: 'prod' }, ...(f.extra ?? [])]
  let etag = 1
  let stale = f.stale ?? false
  let refuse = f.refuse ?? false
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const u = new URL(input)
    const body = typeof init?.body === 'string' ? JSON.parse(init.body) : init?.body
    const c = { method: init?.method ?? 'GET', path: u.pathname.replace('/api/v1/tenants/acme', ''), search: u.search, body, headers: (init?.headers ?? {}) as Record<string, string> }
    calls.push(c)
    if (c.method === 'POST' && c.path.endsWith('/releases/promote')) return json({ releases: [] }, 201)
    if (c.method === 'PUT' && c.path === '/decisions') return json({ ...body, id: 'd9', etag: '"v1"' }, 201)
    if (c.method === 'PUT' && c.path === '/channel-order') {
      if (refuse) {
        refuse = false
        return json({ status: 400, title: 'Bad Request', detail: 'qa is not a channel of the tenant' }, 400)
      }
      if (stale) {
        stale = false
        etag++
        return json({ status: 412, title: 'Precondition Failed' }, 412)
      }
      order = body.order
      return json({ order }, 200, `"o${++etag}"`)
    }
    if (c.path === '/catalogue') return json({ entries: [] })
    if (c.path === '/catalogue/tenant/extensions/relay') {
      return json({ versions: [{ version: '1.0', builds: [{ placed: [{ channel: 'beta', state: 'active' }, { channel: 'nightly', state: 'active' }] }] }] })
    }
    if (c.path === '/licenses') return json({ licenses: [] })
    if (c.path === '/channel-order') return json({ order }, 200, `"o${etag}"`)
    if (c.path === '/channels') return json({ channels: channels.map((x) => ({ name: x.name, kind: 'signed' })) })
    if (c.path === '/decisions') return json({ decisions: f.decisions ?? [] })
    // spec 0042: staging keeps 3 (the tenant's) with its own grace; the others have no rule
    const rt = /^\/channels\/([^/]+)\/retention$/.exec(c.path)
    if (rt) {
      return json({ channel: rt[1], keep: null, grace: rt[1] === 'staging' ? '720h' : null, checked_at: null, next_run: null,
        in_force: rt[1] === 'staging' ? { keep: { value: 3, from: 'tenant' }, grace: { value: '720h', from: 'channel' }, missing: [] }
          : { keep: null, grace: null, missing: ['keep', 'grace'] } })
    }
    let m = /^\/channels\/([^/]+)\/(sources|follows|hold)$/.exec(c.path)
    const ch = channels.find((x) => x.name === m?.[1])
    if (m && ch) return json(m[2] === 'sources' ? { sources: ch.sources } : m[2] === 'follows' ? { follows: ch.follows } : { hold: ch.hold })
    if ((m = /^\/channels\/([^/]+)$/.exec(c.path))) return json({ name: m[1], duckdb_versions: [{ duckdb_version: 'v2.0.0' }] })
    if ((m = /^\/channels\/nightly\/extensions\/(relay|beacon)$/.exec(c.path))) {
      const v = m[1] === 'relay' ? '1.0' : '2.0'
      // relay 1.0 on osx_arm64 too, for v1.9.0, which staging does not serve: current in no waiting cell
      return json({ releases: [{ version: v, platform: 'linux_amd64', state: 'active', abi: 'cpp', build_duckdb_version: 'v2.0.0',
        serves: [{ duckdb_version: 'v2.0.0' }], current_for: ['v2.0.0'], body_hash: m[1] === 'relay' && !f.noBody ? 'ab' : undefined },
      ...(m[1] === 'relay' ? [{ version: v, platform: 'osx_arm64', state: 'active', abi: 'cpp', build_duckdb_version: 'v1.9.0',
        serves: [{ duckdb_version: 'v1.9.0' }], current_for: ['v1.9.0'], body_hash: f.noBody ? undefined : 'cd' }] : [])], purged: [] })
    }
    if (c.path === '/channels/staging/extensions/relay') {
      return f.failIndex ? json({ status: 503, title: 'Unavailable' }, 503) : json({ releases: [], purged: f.stagingPurged ?? [] })
    }
    if (/^\/channels\/[^/]+\/extensions\/[^/]+$/.test(c.path)) return json({ releases: [] })
    if (c.path.endsWith('/attachments')) return json({ attachments: [] })
    return json({}, 404)
  }))
  return calls
}

function page(grants: Grant[] | 'server', ui: 'channels' | 'decisions' | 'bare' = 'channels') {
  const client = new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })
  const perms = grants === 'server' ? new Perms(true) : new Perms(false, grants)
  return render(
    <MemoryRouter initialEntries={['/t/acme/channels']}>
      <TenantContext.Provider value={{ client, tenant: 'acme', perms, api: '/tenants/acme', base: '/t/acme' }}>
        {ui === 'bare' ? <Channels /> : <HomeProvider>{ui === 'decisions' ? <Decisions /> : <Channels />}</HomeProvider>}
      </TenantContext.Provider>
    </MemoryRouter>)
}

const pipeline = async () => fireEvent.click(within(await screen.findByRole('group', { name: 'Channels view' })).getByRole('button', { name: 'Pipeline' }))
const relayPanel = async () => {
  fireEvent.click(await screen.findByRole('button', { name: 'Report & licence of relay for staging' }))
  return screen.getByRole('region', { name: 'relay report and licence' })
}

describe('the pipeline', () => {
  it("draws the stages and one matrix per chain, and promotes nightly's current after the Report & licence panel", async () => {
    const calls = api()
    page('server')
    await pipeline()
    const stages = await screen.findAllByRole('group', { name: /^stage / })
    expect(stages.map((s) => s.getAttribute('aria-label'))).toEqual(['stage nightly', 'stage staging', 'stage prod'])
    expect(within(stages[1]).getByText(/rules: 1 as it lands, 1 auto, 1 manual/)).toBeTruthy()
    expect(within(stages[1]).getByText(/hold 24h/)).toBeTruthy()
    // spec 0042: each stage's retention in force
    expect(await within(stages[1]).findByText('retention: keep 3 · grace 720h')).toBeTruthy()
    expect(within(stages[2]).getByText('retention: no retention (no keep and no grace set)')).toBeTruthy()
    expect(await within(stages[1]).findByText('1 version waits for staging')).toBeTruthy()
    expect(within(stages[2]).getByText('Nothing waits for prod')).toBeTruthy()
    // one row per name, a column per stage: relay's progress in one row
    const relay = screen.getByRole('row', { name: /^relay/ })
    expect(within(relay).getAllByRole('cell').map((c) => c.textContent?.replace(/ /g, ' '))).toEqual([
      'relay', 'as it lands1.0', 'manual→ waits 1.0 from nightlyReport & licence', 'no rule'])
    // beacon: staging serves it from another source, which is no wait
    expect(within(screen.getByRole('row', { name: /^beacon/ })).getByText('upstream:core')).toBeTruthy()
    const panel = await relayPanel()
    expect(document.activeElement).toBe(panel) // focus moves to the panel
    expect(within(panel).getByText(/is nightly's current on v2.0.0 linux_amd64/)).toBeTruthy()
    // the builds current in the waiting cells only, each under its platform and slot
    expect(within(panel).getAllByRole('region', { name: /^reports of / }).map((r) => r.getAttribute('aria-label'))).toEqual(['reports of linux_amd64 · v2.0.0'])
    // the button names what it opens
    expect(screen.getByRole('button', { name: 'Report & licence of relay for staging' }).getAttribute('aria-controls')).toBe(panel.id)
    expect(within(panel).getByRole('link', { name: 'Every report of relay, on its page' }).getAttribute('href')).toBe('/t/acme/catalogue/relay')
    fireEvent.click(within(panel).getByRole('button', { name: 'Promote…' }))
    const dialog = openDialog()
    expect(within(dialog).getByText('Promote relay from nightly')).toBeTruthy()
    expect((within(dialog).getByLabelText('To channel') as HTMLSelectElement).value).toBe('staging')
    expect((within(dialog).getByLabelText('Version') as HTMLInputElement).value).toBe('1.0')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Promote' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    expect(calls.find((c) => c.method === 'POST')).toMatchObject({ path: '/channels/staging/extensions/relay/releases/promote',
      body: { from_channel: 'nightly', version: '1.0' } })
    expect(await screen.findByText('1.0 promoted to staging')).toBeTruthy()
  })

  it("reads a version's reports by its bodies only, none without one, and Not now closes the panel", async () => {
    let calls = api()
    page('server')
    await pipeline()
    let panel = await relayPanel()
    await waitFor(() => expect(calls.some((c) => c.path.endsWith('/attachments'))).toBe(true))
    expect(calls.filter((c) => c.path.endsWith('/attachments')).map((c) => c.path + new URLSearchParams(c.search).get('body_hash'))).toEqual(['/extensions/relay/attachmentsab'])
    fireEvent.click(within(panel).getByRole('button', { name: 'Not now' }))
    expect(screen.queryByRole('region', { name: 'relay report and licence' })).toBeNull()
    // focus returns to the button that opened it
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Report & licence of relay for staging' })))
    cleanup()
    // no body known: no report read, never every report of the extension
    calls = api({ noBody: true })
    page('server')
    await pipeline()
    panel = await relayPanel()
    await within(panel).findByText('No licence in this entry.')
    expect(within(panel).queryByRole('region', { name: /^reports of / })).toBeNull()
    expect(calls.some((c) => c.path.endsWith('/attachments'))).toBe(false)
  })

  it('skips a waiting version under a manual rule, and a skipped version is left out of the count', async () => {
    const calls = api()
    page('server')
    await pipeline()
    fireEvent.click(within(await relayPanel()).getByRole('button', { name: 'Skip' }))
    fireEvent.click(within(openDialog()).getByRole('button', { name: 'Skip' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true))
    expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ kind: 'skip', channel: 'staging', source: 'tenant', name: 'relay', version: '1.0' })
    cleanup()
    api({ decisions: [skip({ channel: 'staging', version: '1.0' })] })
    page('server')
    await pipeline()
    expect(await screen.findByText('Nothing waits for staging')).toBeTruthy()
    expect(screen.queryByRole('button', { name: /^Report & licence of/ })).toBeNull()
  })

  it('counts a version the stage purged as had: it never waits again', async () => {
    api({ stagingPurged: [{ version: '1.0', platform: 'linux_amd64', slot: 'v2.0.0', duckdb_version: 'v2.0.0' }] })
    page('server')
    await pipeline()
    expect(await screen.findByText('Nothing waits for staging')).toBeTruthy()
  })

  it('says it is reading what waits, and that it could not be read, never "Nothing waits"', async () => {
    api({ failIndex: true })
    page('server')
    await pipeline()
    expect(await screen.findAllByText('What waits could not be read')).toHaveLength(2)
    expect(screen.queryByText(/^Nothing waits/)).toBeNull()
    expect(screen.getByRole('alert')).toBeTruthy()
  })

  it('names the right a Promote lacks', async () => {
    api()
    page([{ principal: 'p', verbs: ['admin'] }, { principal: 'p', verbs: ['publish'], channel: 'nightly' }])
    await pipeline()
    const panel = await relayPanel()
    expect(within(panel).queryByRole('button', { name: 'Promote…' })).toBeNull()
    expect(within(panel).getByText('Promote needs promote on relay in staging, which you do not hold.')).toBeTruthy()
    expect(within(panel).getByRole('button', { name: 'Skip' })).toBeTruthy()
  })

  it('sets the order as each channel names its next, at the ETag read, and reads it again when it changed meanwhile', async () => {
    const calls = api({ stale: true })
    page('server')
    await pipeline()
    fireEvent.click(await screen.findByRole('button', { name: 'Edit the order…' }))
    fireEvent.change(await screen.findByLabelText('next of staging'), { target: { value: '' } })
    fireEvent.click(within(openDialog()).getByRole('button', { name: 'Save' }))
    // 412: said, and read again with the new ETag
    expect(await screen.findByText(/The order changed meanwhile/)).toBeTruthy()
    await waitFor(() => expect(calls.filter((c) => c.method === 'GET' && c.path === '/channel-order').length).toBeGreaterThanOrEqual(3))
    fireEvent.change(await screen.findByLabelText('next of staging'), { target: { value: '' } })
    fireEvent.click(within(openDialog()).getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(calls.filter((c) => c.method === 'PUT' && c.path === '/channel-order').length).toBe(2))
    const put = calls.filter((c) => c.method === 'PUT' && c.path === '/channel-order')
    expect(put[0].headers['If-Match']).toBe('"o1"')
    expect(put[1].headers['If-Match']).toBe('"o2"')
    expect(put[1].body).toEqual({ order: [{ channel: 'nightly', next: 'staging' }] })
  })

  it('lists the channels the order read names beside the home\'s, and reads the home again on opening, on a 412 and on a 400', async () => {
    const calls = api({ extra: [{ channel: 'dev', next: 'qa' }], refuse: true, stale: true })
    page('server')
    await pipeline()
    const homeReads = () => calls.filter((c) => c.method === 'GET' && c.path === '/catalogue').length
    await screen.findByRole('button', { name: 'Edit the order…' })
    const before = homeReads()
    fireEvent.click(screen.getByRole('button', { name: 'Edit the order…' }))
    await waitFor(() => expect(homeReads()).toBe(before + 1)) // on opening
    // dev and qa, which the home's channels do not name, are rows and choices too
    const dev = await screen.findByLabelText('next of dev') as HTMLSelectElement
    expect(dev.value).toBe('qa')
    expect([...(screen.getByLabelText('next of nightly') as HTMLSelectElement).options].map((o) => o.value)).toEqual(
      ['', 'staging', 'prod', 'beta', 'dev', 'qa']) // the home's, in the order's sequence, then the order's others
    fireEvent.click(within(openDialog()).getByRole('button', { name: 'Save' }))
    expect(await screen.findByText(/qa is not a channel of the tenant/)).toBeTruthy() // 400
    await waitFor(() => expect(homeReads()).toBe(before + 2))
    fireEvent.click(within(openDialog()).getByRole('button', { name: 'Save' }))
    expect(await screen.findByText(/The order changed meanwhile/)).toBeTruthy() // 412
    await waitFor(() => expect(homeReads()).toBe(before + 3))
  })

  it("is a tenant administrator's: a channel's administrator sees the list of its channels only", async () => {
    api()
    page([{ principal: 'p', verbs: ['admin'], channel: 'prod' }], 'bare')
    expect(await screen.findByRole('link', { name: 'prod' })).toBeTruthy()
    expect(screen.queryByRole('link', { name: 'staging' })).toBeNull()
    expect(screen.queryByRole('group', { name: 'Channels view' })).toBeNull()
    expect(screen.queryByRole('button', { name: /order/ })).toBeNull()
  })
})

describe('the queue with an order', () => {
  it("promotes from the previous stage when it and another channel hold the version", async () => {
    api()
    page('server', 'decisions')
    const group = await screen.findByRole('group', { name: 'relay staging actions' })
    fireEvent.click(await within(group).findByRole('button', { name: 'Promote…' }))
    expect(within(openDialog()).getByText('Promote relay 1.0 from nightly to staging?')).toBeTruthy() // beta comes first by name
  })
})
