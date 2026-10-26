import { describe, expect, it } from 'vitest'
import { declared, entriesFor, entryKind, expiry, importPath, licenseKey, licensePath, peek, preselect, replaced, sourcesLine, syncLine } from './Licenses'
import type { License, NameKeys } from './Licenses'
import { usageLines } from './Stats'

// Spec 0012 phase 1d: the Licences screen and the Usage tab.

const b64 = (s: string) => btoa(String.fromCharCode(...new TextEncoder().encode(s))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')

describe('licences', () => {
  it('names what a licence file holds, without verifying it', () => {
    const tok = `${b64('{"alg":"RS256","kid":"sha256:aa"}')}.${b64('{"ext":"tresor","plan":"pro","holder":"Acme Ü","exp":1900000000}')}.sig`
    expect(peek(tok)).toEqual({ ext: 'tresor', kid: 'sha256:aa', plan: 'pro', holder: 'Acme Ü', exp: 1900000000 })
    expect(peek(`!!!.${b64('{"ext":"tresor"}')}.sig`)).toEqual({ ext: 'tresor' }) // a header unread: no kid
    expect(peek(`  ${tok}\n`)?.ext).toBe('tresor')
    expect(peek('not a licence')).toBeNull()
    expect(peek(`${b64('{}')}.${b64('{"plan":"pro"}')}.sig`)).toBeNull() // no ext
    expect(peek(`${b64('{}')}.!!!.sig`)).toBeNull()
  })
  it('flags a licence expiring within 30 days, and one in grace', () => {
    const now = Date.parse('2026-10-18T00:00:00Z')
    const l = (status: string, exp: string, grace = exp) => ({ status, expires_at: exp, grace_until: grace })
    expect(expiry(l('valid', '2026-12-31T00:00:00Z'), now)).toBe('')
    expect(expiry(l('valid', '2026-11-10T00:00:00Z'), now)).toBe('expires in 23 days')
    expect(expiry(l('valid', '2026-10-19T00:00:00Z'), now)).toBe('expires in 1 day')
    expect(expiry(l('grace', '2026-10-10T00:00:00Z', '2026-10-25T00:00:00Z'), now)).toBe('in grace: 7 days left')
    expect(expiry(l('expired', '2026-10-01T00:00:00Z'), now)).toBe('')
  })
  // spec 0035: a key with the entries it is live for, two of them here
  const names: NameKeys[] = [{ name: 'tresor', keys: [
    { kid: 'sha256:aa', sources: [{ kind: 'publisher', name: 'enterest-ci', distributor: 'enterest', builds: 2 }, { kind: 'upstream', name: 'org', builds: 1 }] },
    { kid: 'sha256:bb', sources: [{ kind: 'publisher', name: 'ci', builds: 1 }] }] }]
  it('reads the licence keys of live builds, each with its entries (spec 0021, 0035)', () => {
    expect(sourcesLine(names[0].keys[0].sources)).toBe('publisher enterest-ci (2 builds, distributor enterest) · upstream org (1 build)')
    expect(entryKind(names[0].keys[0].sources[0])).toBe("enterest's publisher (a distributor)")
    expect(entryKind(names[0].keys[0].sources[1])).toBe('upstream')
    expect(entryKind(names[0].keys[1].sources[0])).toBe('publisher')
    // the flag: a licence whose entry's live builds declare its key is not flagged; another entry
    // declaring the key, a removed source's entry, another key or name: flagged
    expect(declared(names, { name: 'tresor', key: 'sha256:aa', entry: 'publisher:enterest-ci' })).toBe(true)
    expect(declared(names, { name: 'tresor', key: 'sha256:aa', entry: 'upstream:org' })).toBe(true)
    expect(declared(names, { name: 'tresor', key: 'sha256:aa', entry: 'publisher:ci' })).toBe(false)
    expect(declared(names, { name: 'tresor', key: 'sha256:aa', entry: 'publisher-id:0190' })).toBe(false)
    expect(declared(names, { name: 'tresor', key: 'sha256:cc', entry: 'upstream:org' })).toBe(false)
    expect(declared(names, { name: 'acl', key: 'sha256:aa', entry: 'upstream:org' })).toBe(false)
    // a licence per entry; the entry in the query, after an action's path
    expect(licenseKey({ name: 'tresor', entry: 'upstream:org' })).not.toBe(licenseKey({ name: 'tresor', entry: 'publisher:ci' }))
    expect(licensePath('/api/v1/tenants/acme', { name: 'tresor', entry: 'publisher:ci' })).toBe('/api/v1/tenants/acme/licenses/tresor?source=publisher%3Aci')
    expect(licensePath('/api/v1/tenants/acme', { name: 'tresor', entry: 'publisher:ci' }, 'public'))
      .toBe('/api/v1/tenants/acme/licenses/tresor/public?source=publisher%3Aci')
  })
  it("chooses an import's entry: listed by its key, preselected only when it is the only one (spec 0035)", () => {
    expect(entriesFor(names, 'tresor', 'sha256:aa').map((s) => s.name)).toEqual(['enterest-ci', 'org'])
    expect(preselect(entriesFor(names, 'tresor', 'sha256:aa'))).toBe('') // never among several
    expect(preselect(entriesFor(names, 'tresor', 'sha256:bb'))).toBe('publisher:ci')
    expect(entriesFor(names, 'tresor', 'sha256:cc')).toEqual([]) // none: the user names one, kista decides
    expect(entriesFor(names, 'tresor')).toEqual([])
    expect(entriesFor(undefined, 'tresor', 'sha256:aa')).toEqual([])
    expect(preselect([])).toBe('')
    // the import always names its entry
    expect(importPath('/api/v1/tenants/acme', 'publisher:enterest-ci')).toBe('/api/v1/tenants/acme/licenses?source=publisher%3Aenterest-ci')
    // it replaces the name's licence in that entry only
    const l = (entry: string, name = 'tresor') => ({ name, entry, id: `${name}@${entry}` }) as License
    const ls = [l('publisher:ci'), l('upstream:org'), l('upstream:org', 'acl')]
    expect(replaced(ls, 'tresor', 'upstream:org')?.id).toBe('tresor@upstream:org')
    expect(replaced(ls, 'tresor', 'publisher:enterest-ci')).toBeUndefined()
    expect(replaced(ls, 'tresor', '')).toBeUndefined()
  })
  it('reads a relay run in a line', () => {
    expect(syncLine({ counts: { received: 3, stored: 1, unchanged: 2, refused: 0 } })).toBe('received 3 · stored 1 · unchanged 2')
    expect(syncLine({ counts: {} })).toBe('nothing received')
    expect(syncLine(null)).toBe('')
  })
})

describe('usage', () => {
  it('shows use without a licence and received counts apart', () => {
    const { events, users } = usageLines(
      [
        { extension: 'tresor', status: 'valid', event: 'attach', value: 3 },
        { extension: 'tresor', status: 'grace', event: 'attach', value: 1 },
        { extension: 'tresor', status: 'missing', event: 'attach', value: 4 },
        { extension: 'tresor', status: 'expired', event: 'attach', value: 1 },
        { extension: 'tresor', status: 'received', event: 'attach', value: 10 },
        { extension: 'tresor', status: 'valid', event: 'attach', counter: 'sources', value: 9 },
        { extension: 'acl', status: 'valid', event: 'load', value: 7 },
      ],
      [{ extension: 'tresor', users: 3 }, { extension: 'tresor', status: 'received', users: 6 }, { extension: 'acl', users: 5 }],
    )
    expect(events.map((e) => [e.extension, e.event, e.counter, e.licensed, e.unlicensed, e.received])).toEqual([
      ['acl', 'load', undefined, 7, 0, 0],
      ['tresor', 'attach', undefined, 4, 5, 10],
      ['tresor', 'attach', 'sources', 9, 0, 0],
    ])
    expect(users.map((u) => [u.extension, u.users, u.received])).toEqual([['acl', 5, 0], ['tresor', 3, 6]])
  })
})
