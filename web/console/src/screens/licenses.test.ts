import { describe, expect, it } from 'vitest'
import { declared, expiry, licenseKey, peek, replaced, sourcesLine, syncLine } from './Licenses'
import type { License } from './Licenses'
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
  it('reads the licence keys of live builds (spec 0021)', () => {
    const names = [{ name: 'tresor', keys: [{ kid: 'sha256:aa', builds: 2, owner: 'upstream:enterest', sources: [
      { kind: 'publisher' as const, name: 'tresor-ci' }, { kind: 'upstream' as const, name: 'enterest' }] }] }]
    expect(sourcesLine(names[0].keys[0].sources)).toBe('publisher tresor-ci · upstream enterest')
    expect(declared(names, { name: 'tresor', key: 'sha256:aa', owner: 'upstream:enterest' })).toBe(true)
    expect(declared(names, { name: 'tresor', key: 'sha256:bb', owner: 'upstream:enterest' })).toBe(false)
    expect(declared(names, { name: 'acl', key: 'sha256:aa', owner: 'upstream:enterest' })).toBe(false)
    // spec 0033 phase B: a source that brought the key (the owner, or another upstream relaying the
    // owner's very licence) is served; a source without it, a removed source's, unknown: not served
    expect(declared(names, { name: 'tresor', key: 'sha256:aa', owner: 'publisher:tresor-ci' })).toBe(true)
    expect(declared(names, { name: 'tresor', key: 'sha256:aa', owner: 'upstream:nightly' })).toBe(false)
    expect(declared(names, { name: 'tresor', key: 'sha256:aa', owner: 'publisher-id:0190' })).toBe(false)
    expect(declared(names, { name: 'tresor', key: 'sha256:aa', owner: 'unknown' })).toBe(false)
    // spec 0033 phase B: a licence per (name, source)
    expect(licenseKey({ name: 'tresor', owner: 'upstream:enterest' })).not.toBe(licenseKey({ name: 'tresor', owner: 'publisher:ci' }))
  })
  it('replaces the licence of the source that owns the key (spec 0033 phase B)', () => {
    const names = [{ name: 'tresor', keys: [
      { kid: 'sha256:aa', builds: 1, owner: 'upstream:enterest', sources: [] },
      { kid: 'sha256:bb', builds: 1, owner: 'publisher:ci', sources: [] },
      { kid: 'sha256:cc', builds: 1, sources: [] }] }]
    const l = (owner: string, key: string) => ({ name: 'tresor', owner, key, id: `${owner}/${key}` }) as License
    const ls = [l('publisher:ci', 'sha256:bb'), l('unknown', 'sha256:aa')]
    expect(replaced(ls, names, 'tresor', 'sha256:bb')?.id).toBe('publisher:ci/sha256:bb')
    expect(replaced(ls, names, 'tresor', 'sha256:aa')?.id).toBe('unknown/sha256:aa') // its owner holds none
    expect(replaced([...ls, l('upstream:enterest', 'sha256:dd')], names, 'tresor', 'sha256:aa')?.id).toBe('upstream:enterest/sha256:dd')
    expect(replaced(ls, names, 'tresor', 'sha256:cc')).toBeUndefined() // no owner: refused by the server
    expect(replaced(ls, names, 'tresor')).toBeUndefined()
    expect(replaced(ls, undefined, 'tresor', 'sha256:bb')).toBeUndefined()
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
