import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Perms, TenantContext } from '../lib/context'
import { HomeProvider } from '../lib/home'
import { Client } from '../lib/http'
import { type ChannelReads, type Decision, type HomeReads, cellOf, decisionBody, promoteFrom, queue } from '../lib/queue'
import type { CatalogueEntry } from './Catalogue'
import { Catalogue, ruleChanges, whatHappens } from './Catalogue'
import { Decisions } from './Decisions'
import { Events, kindParam, families } from './Events'
import { servingKinds } from './Channel'
import { whatItIs } from './Blocks'
import { tenantSections } from './TenantArea'

// The catalogue as the tenant's home and the decision queue (spec 0041): the matrix's cells and the
// queue's groups from fixtures, Skip and Dismiss as decisions, Promote where a channel holds the
// version, the home by the caller's grants.

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

const now = Date.parse('2026-11-05T10:00:00Z')
const hours = (h: number) => new Date(now + h * 3600_000).toISOString()

const entry = (source: string, name: string, more: Partial<CatalogueEntry> = {}): CatalogueEntry =>
  ({ source, name, versions: ['1.0'], rules: {}, ...more })

const channel = (name: string, more: Partial<ChannelReads> = {}): ChannelReads => ({ name, hold: '0s', follows: [], sources: [], ...more })

const decision = (d: Partial<Decision>): Decision => ({ id: 'd1', kind: 'skip', source: 'publisher:ci', name: 'acl', decided_by: 'x', decided_at: '', etag: '"v1"', ...d })

// prod: acl manual with 0.4.0 waiting, geo auto held, h3 from another source; nightly follows ci,
// skipping hello; a failed placement of maps; the catalogue: acl, geo (taken by prod only), maps
// (taken nowhere), a removed source's entry
function reads(decisions: Decision[] = []): HomeReads {
  return {
    entries: [
      entry('publisher:ci', 'acl', { rules: { prod: 'manual', nightly: 'follow' } }),
      entry('publisher:ci', 'geo', { rules: { prod: 'auto' }, held: ['2.0'], held_until: { '2.0': hours(30) } }),
      entry('tenant', 'maps', { held: ['1.1'], held_until: { '1.1': hours(5) } }),
      entry('upstream:(removed)', 'old'),
    ],
    channels: [
      channel('nightly', {
        follows: [{ source: 'publisher:ci', set_by: 'x', set_at: '', skips: { hello: 'the tenant uses it from another source' } }],
        sources: [{ name: 'acl', source: 'publisher:ci', mode: 'follow', current: '0.3.0', available: {} }],
      }),
      channel('prod', {
        hold: '24h0m0s',
        sources: [
          { name: 'acl', source: 'publisher:ci', mode: 'manual', current: '0.3.0', waiting: '0.4.0', available: {} },
          { name: 'geo', source: 'publisher:ci', mode: 'auto', waits_until: hours(3), available: {} },
          { name: 'h3', source: 'upstream:community', mode: 'auto', available: {} },
          { name: 'maps', source: 'tenant', mode: 'manual', outcome: '1.0 linux_amd64 v2.0.0: the body is blocked in this tenant', available: {} },
        ],
      }),
    ],
    decisions,
  }
}

describe('the matrix', () => {
  it("shows a channel's rule, what waits less a skip, another source's rule, a follow's skip and nothing", () => {
    const r = reads()
    const [nightly, prod] = r.channels
    expect(cellOf(entry('publisher:ci', 'acl'), prod, [])).toEqual({ kind: 'rule', mode: 'manual', current: '0.3.0', waiting: '0.4.0', waitsUntil: undefined, outcome: undefined })
    expect(cellOf(entry('publisher:ci', 'acl'), prod, [decision({ channel: 'prod', version: '0.4.0' })])).toMatchObject({ kind: 'rule', waiting: undefined })
    expect(cellOf(entry('publisher:ci', 'h3'), prod, [])).toEqual({ kind: 'other', source: 'upstream:community' })
    expect(cellOf(entry('publisher:ci', 'hello'), nightly, [])).toEqual({ kind: 'skipped', reason: 'the tenant uses it from another source' })
    expect(cellOf(entry('tenant', 'zzz'), nightly, [])).toEqual({ kind: 'none' })
  })

  it('lists the changes a rule editor saves, one per changed channel', () => {
    const rules = { prod: { source: 'publisher:ci', mode: 'manual' }, nightly: undefined, dev: { source: 'tenant', mode: 'manual' } }
    expect(ruleChanges(rules, { prod: { source: 'publisher:ci', mode: 'auto' }, nightly: { source: '', mode: '' }, dev: { source: 'tenant', mode: 'manual' } }))
      .toEqual([{ channel: 'prod', body: { source: 'publisher:ci', mode: 'auto' }, from: 'publisher:ci manual', to: 'publisher:ci auto' }])
    expect(ruleChanges(rules, { nightly: { source: 'publisher:ci', mode: '' } }))
      .toEqual([{ channel: 'nightly', body: { source: 'publisher:ci' }, from: 'no rule', to: 'publisher:ci as it lands' }])
  })
})

describe('the queue', () => {
  const keys = (r: HomeReads) => queue(r, now).map((i) => `${i.group} ${'channel' in i ? i.channel ?? '' : ''} ${i.name}`.replace(/ +/g, ' '))

  it('gathers what waits: manual rules, holds ending within a day, follow skips, names in no channel, failures', () => {
    // nightly follows ci: its acl rule acts as auto; acl is taken by every channel; geo's catalogue hold
    // ends in 30 hours, and an auto rule takes it anyway; the removed source's entry is left out
    expect(keys(reads())).toEqual([
      'skipped nightly hello',
      'manual prod acl',
      'holds prod geo', // the rule's waits_until, within a day
      'failed prod maps',
      'nochannel geo', // taken by prod only
      'holds maps', // held 5 hours, no automatic rule takes it
      'nochannel maps',
    ])
  })

  it("a follow's rule acts as manual once the channel no longer follows the source", () => {
    const r = reads()
    r.channels[0].follows = []
    r.channels[0].sources[0].waiting = '0.4.0'
    expect(keys(r)).toContain('manual nightly acl')
  })

  it('a skip hides a version until a newer one waits', () => {
    const skip = decision({ channel: 'prod', version: '0.4.0' })
    expect(keys(reads([skip]))).not.toContain('manual prod acl')
    const newer = reads([skip])
    newer.channels[1].sources[0].waiting = '0.4.1'
    expect(keys(newer)).toContain('manual prod acl')
    // another channel's skip, or the tenant's, hides nothing
    expect(keys(reads([decision({ channel: 'nightly', version: '0.4.0' })]))).toContain('manual prod acl')
  })

  it('a dismissal hides an item until its situation changes', () => {
    const follow = decision({ kind: 'dismiss', channel: 'nightly', name: 'hello', situation: 'the tenant uses it from another source' })
    expect(keys(reads([follow]))).not.toContain('skipped nightly hello')
    expect(keys(reads([{ ...follow, situation: 'reserved' }]))).toContain('skipped nightly hello')
    const tenant = decision({ kind: 'dismiss', name: 'geo', situation: ['prod'] })
    expect(keys(reads([tenant]))).not.toContain('nochannel geo')
    const taken = reads([tenant])
    taken.entries[1].rules = { prod: 'auto', nightly: 'auto' }
    expect(keys(taken)).not.toContain('nochannel geo') // taken by every channel now
    taken.entries[1].rules = {}
    expect(keys(taken)).toContain('nochannel geo') // taken by none: another situation
  })

  it("writes Skip as the waiting version, Dismiss as the situation", () => {
    const items = queue(reads(), now)
    expect(decisionBody(items.find((i) => i.group === 'manual')!)).toEqual({ kind: 'skip', channel: 'prod', source: 'publisher:ci', name: 'acl', version: '0.4.0' })
    expect(decisionBody(items.find((i) => i.group === 'skipped')!)).toEqual({ kind: 'dismiss', channel: 'nightly', source: 'publisher:ci', name: 'hello',
      situation: 'the tenant uses it from another source' })
    expect(decisionBody(items.find((i) => i.group === 'nochannel' && i.name === 'geo')!)).toEqual({ kind: 'dismiss', source: 'publisher:ci', name: 'geo', situation: ['prod'] })
    expect(decisionBody(items.find((i) => i.group === 'failed')!)).toBeNull()
  })

  it("says what a rule does next: nothing by itself under manual, the newest version once the later of its holds passed", () => {
    const e = { versions: ['1.2', '1.1'], held_until: { '1.2': '2026-11-06T09:00:00Z' } }
    expect(whatHappens('manual', e, '1.1', '0s')).toBe('nothing is placed by itself: 1.2 waits for a placement by hand.')
    expect(whatHappens('auto', e, '1.2', '0s')).toMatch(/^nothing newer than 1.2 yet/)
    expect(whatHappens('auto', { versions: ['1.2'] }, undefined, '0s')).toBe("1.2 is placed at the placement pass's next look (within a minute).")
    const both = whatHappens('auto', e, '1.1', '24h0m0s')
    expect(both).toMatch(/its source's hold ends .* and the channel's hold, 24h after each build's arrival, has passed \(the later of the two\)/)
    expect(whatHappens('auto', e, '1.1', '24h0m0s', '2026-11-06T10:00:00Z')).toMatch(/^1.2 is placed at the placement pass's first look after /)
  })

  it('promotes from a channel holding the version as an active or deprecated release, places otherwise', () => {
    const vs = [{ version: '0.4.0', builds: [{ placed: [{ channel: 'staging', state: 'yanked' }, { channel: 'nightly', state: 'deprecated' }] }] },
      { version: '0.3.0', builds: [{ placed: [{ channel: 'dev', state: 'active' }] }] }]
    expect(promoteFrom(vs, '0.4.0', 'prod')).toBe('nightly')
    expect(promoteFrom(vs, '0.4.0', 'nightly')).toBe('')
    expect(promoteFrom(vs, '0.5.0', 'prod')).toBe('')
    // the first holding channel the caller may publish from
    const two = [{ version: '0.4.0', builds: [{ placed: [{ channel: 'beta', state: 'active' }, { channel: 'nightly', state: 'active' }] }] }]
    expect(promoteFrom(two, '0.4.0', 'prod', (c) => c === 'nightly')).toBe('nightly')
    expect(promoteFrom(two, '0.4.0', 'prod', () => false)).toBe('')
  })

  it("a rule's hold names its version (waits_for); without one, nothing is guessed", () => {
    const r = reads()
    r.channels[1].sources[1].waits_for = '2.0'
    const hold = queue(r, now).find((i) => i.group === 'holds' && 'channel' in i && i.channel === 'prod')
    expect(hold).toMatchObject({ name: 'geo', version: '2.0' })
    const unnamed = queue(reads(), now).find((i) => i.group === 'holds' && 'channel' in i && i.channel === 'prod')
    expect(unnamed && 'version' in unnamed ? unnamed.version : 'none').toBeUndefined()
  })
})

describe('the home', () => {
  const g = (verbs: string[], more: object = {}) => ({ principal: 'p', verbs, ...more })
  it("is the catalogue, with the decisions that wait, for a tenant administrator; a channel administrator's is Channels", () => {
    const admin = tenantSections(new Perms(false, [g(['admin'])]), '/t/acme', 3)
    expect(admin.slice(0, 3).map((t) => t.label)).toEqual(['Catalogue', 'Needs a decision', 'Channels'])
    expect(admin[1].badge).toBe(3)
    const chan = tenantSections(new Perms(false, [g(['admin'], { channel: 'prod' })]), '/t/acme')
    expect(chan[0].label).toBe('Channels')
    expect(chan.some((t) => t.label === 'Needs a decision' || t.label === 'Catalogue')).toBe(false)
  })

  it("names the events' families, and what a blocked body is", () => {
    expect(families(['release.add', 'release.yank', 'install', 'decision.set'])).toEqual(['decision.*', 'release.*'])
    expect(kindParam(['release.*'], 'release.yank')).toBe('release.*')
    expect(kindParam(['release.*'], 'block.add')).toBe('release.*,block.add')
    expect(kindParam([], '')).toBe('')
    expect(whatItIs([{ source: 'publisher:ci', name: 'tresor', version: '1.3.0', platform: 'linux_amd64', slot: 'v2.0.0' },
      { source: 'tenant', name: 'tresor', version: '1.3.0', platform: 'linux_amd64', slot: 'v2.0.0' }])).toBe('tresor 1.3.0, linux_amd64, publisher:ci (+1)')
    expect(whatItIs([])).toBe('')
  })
})

type Call = { method: string; path: string; search: string; body?: unknown; headers: Record<string, string> }

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

// the API of reads() and its decisions, kept as the server keeps them
function api(r: HomeReads, deleted = 204) {
  const calls: Call[] = []
  let ds = [...r.decisions]
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const u = new URL(input)
    const body = typeof init?.body === 'string' ? JSON.parse(init.body) : init?.body
    const c = { method: init?.method ?? 'GET', path: u.pathname.replace('/api/v1/tenants/acme', ''), search: u.search, body,
      headers: (init?.headers ?? {}) as Record<string, string> }
    calls.push(c)
    if (c.method === 'PUT' && c.path === '/decisions') {
      const d = { ...(body as object), id: `d${ds.length + 1}`, decided_by: 'tom', decided_at: '', etag: '"v1"' } as Decision
      ds = [...ds, d]
      return json(d, 201)
    }
    if (c.method === 'DELETE' && c.path.startsWith('/decisions/')) {
      if (deleted !== 204) return json({ status: deleted, title: 'Precondition Failed' }, deleted)
      ds = ds.filter((d) => `/decisions/${d.id}` !== c.path)
      return new Response(null, { status: 204 })
    }
    if (c.path === '/catalogue') return json({ entries: r.entries })
    if (c.path === '/channels') return json({ channels: r.channels.map((x) => ({ name: x.name, kind: 'signed' })) })
    if (c.path === '/decisions') return json({ decisions: ds })
    const m = /^\/channels\/([^/]+)\/(sources|follows|hold)$/.exec(c.path)
    const ch = r.channels.find((x) => x.name === m?.[1])
    if (m && ch) return json(m[2] === 'sources' ? { sources: ch.sources } : m[2] === 'follows' ? { follows: ch.follows } : { hold: ch.hold })
    return json({}, 404)
  }))
  return calls
}

function home(ui: React.ReactNode) {
  const client = new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })
  return render(
    <MemoryRouter>
      <TenantContext.Provider value={{ client, tenant: 'acme', perms: new Perms(false, [{ principal: 'p', verbs: ['admin'] }]), api: '/tenants/acme', base: '/t/acme' }}>
        <HomeProvider>{ui}</HomeProvider>
      </TenantContext.Provider>
    </MemoryRouter>)
}

describe('the pages', () => {
  it("draws the matrix: a column per signed channel, its follows and hold, each entry's cells", async () => {
    api(reads())
    home(<Catalogue />)
    const row = (await screen.findByRole('link', { name: 'acl' })).closest('tr')!
    expect(within(row).getByText('→ waits')).toBeTruthy()
    expect(within(row).getByText('0.4.0')).toBeTruthy()
    expect(screen.getByText(/follows publisher:ci · hold none/)).toBeTruthy()
    expect(screen.getByRole('link', { name: /decisions wait/ })).toBeTruthy()
    const maps = screen.getByRole('link', { name: 'maps' }).closest('tr')!
    expect(within(maps).getAllByRole('button', { name: /^Add a rule for maps in / }).map((b) => b.getAttribute('aria-label')))
      .toEqual(['Add a rule for maps in nightly']) // prod has a rule: Set rule there
    expect(within(maps).getByRole('button', { name: 'Set the rule for maps in prod' })).toBeTruthy()
    expect(screen.getByText(/follows nothing · hold 24h/)).toBeTruthy() // prod's day-long hold
    // the banner counts each group
    expect(screen.getByText(/new versions under manual rules 1, holds ending within a day 2/)).toBeTruthy()
  })

  it('skips a waiting version as a decision, and undoes it with its ETag', async () => {
    const calls = api(reads())
    home(<Decisions />)
    const acl = await screen.findByRole('group', { name: 'acl prod actions' })
    expect(within(acl).queryByRole('button', { name: /Promote|Place/ })).toBeNull() // no promote: never offered
    fireEvent.click(within(acl).getByRole('button', { name: 'Skip' }))
    await waitFor(() => expect(screen.queryByRole('group', { name: 'acl prod actions' })).toBeNull())
    expect(calls.find((c) => c.method === 'PUT')).toMatchObject({ path: '/decisions', body: { kind: 'skip', channel: 'prod', source: 'publisher:ci', name: 'acl', version: '0.4.0' } })
    fireEvent.click(screen.getByRole('button', { name: 'Undo (1)' }))
    await screen.findByRole('group', { name: 'acl prod actions' })
    const del = calls.find((c) => c.method === 'DELETE')!
    expect(del.path).toBe('/decisions/d1')
    expect(del.headers['If-Match']).toBe('"v1"')
  })

  it('an Undo another administrator overtook (412) is dropped and said so', async () => {
    const calls = api(reads(), 412)
    home(<Decisions />)
    fireEvent.click(within(await screen.findByRole('group', { name: 'acl prod actions' })).getByRole('button', { name: 'Skip' }))
    await waitFor(() => expect(screen.queryByRole('group', { name: 'acl prod actions' })).toBeNull())
    fireEvent.click(screen.getByRole('button', { name: 'Undo (1)' }))
    expect(await screen.findByText(/Another administrator changed skip acl since/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: /^Undo/ })).toBeNull()
    expect(calls.filter((c) => c.method === 'DELETE').length).toBe(1)
  })

  it("offers no Place early nor Purge for a hold whose version is not known; the drawer's toggle says it is open", async () => {
    api(reads())
    home(<Decisions />)
    const geo = await screen.findByRole('group', { name: 'geo prod actions' })
    expect(within(geo).queryByRole('button', { name: /Place early|Purge/ })).toBeNull()
    const toggle = within(geo).getByRole('button', { name: 'Report & licence' })
    expect(toggle.getAttribute('aria-expanded')).toBe('false')
    fireEvent.click(toggle)
    expect(toggle.getAttribute('aria-expanded')).toBe('true')
  })

  it("offers a channel's administrator no serving-plane kind, and reads its channel's subject", async () => {
    const asked: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (input: string) => {
      const u = new URL(input)
      asked.push(u.pathname + u.search)
      if (u.pathname.endsWith('/event-kinds')) return json({ kinds: [{ kind: 'install' }, { kind: 'extension.use' }, { kind: 'usage.receive' }, { kind: 'release.add' }] })
      return json({ events: [] })
    }))
    const client = new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })
    render(<Events client={client} path="/tenants/acme/events" title="Events" subject="channel:prod" hidden={servingKinds} />)
    await screen.findByRole('option', { name: 'release.add' })
    expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual(['any kind', 'release.add', 'any outcome', 'ok', 'refused', 'failed'])
    expect(within(screen.getByRole('group', { name: 'event families' })).getAllByRole('button').map((b) => b.textContent)).toEqual(['release.*'])
    expect(asked.some((q) => q.includes('subject=channel%3Aprod'))).toBe(true)
  })

  it("dismisses a follow's skip with its reason", async () => {
    const calls = api(reads())
    home(<Decisions />)
    fireEvent.click(within(await screen.findByRole('group', { name: 'hello nightly actions' })).getByRole('button', { name: 'Dismiss' }))
    await waitFor(() => expect(screen.queryByRole('group', { name: 'hello nightly actions' })).toBeNull())
    expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ kind: 'dismiss', channel: 'nightly', source: 'publisher:ci', name: 'hello',
      situation: 'the tenant uses it from another source' })
  })
})
