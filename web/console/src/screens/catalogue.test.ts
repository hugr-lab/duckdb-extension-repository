import { describe, expect, it } from 'vitest'
import { Perms } from '../lib/context'
import { sourceSeg } from '../lib/http'
import { buildsState, compareVersions, modeWords, placeBody, purgeable, utf8Bytes, versionSeg, versionState } from './Catalogue'
import { changeTo, holdInput, holdWords, keptMode, newer } from './Sources'

// Spec 0033 phase C: the Catalogue screen and the channel's Sources tab.

describe('catalogue', () => {
  it("names a version's state, the one that matters most first", () => {
    const e = { held: ['1.1', '1.0'], revoked: ['1.0'], withdrawn: ['0.9'] }
    expect(versionState(e, '0.9')).toBe('withdrawn')
    expect(versionState(e, '1.0')).toBe('revoked')
    expect(versionState(e, '1.1')).toBe('held')
    expect(versionState({}, '1.2')).toBe('available')
  })
  it('offers a purge for a build placed nowhere and not withdrawn', () => {
    expect(purgeable({ state: 'held', placed: [] })).toBe(true)
    expect(purgeable({ state: 'available', placed: [{ channel: 'prod', state: 'yanked' }] })).toBe(false)
    expect(purgeable({ state: 'withdrawn', placed: [] })).toBe(false)
  })
  it("words a rule's mode, and sends a placement's choices only when made", () => {
    expect(modeWords('auto')).toBe('auto')
    expect(modeWords('')).toBe('as it lands')
    const p = { channel: 'prod', version: '1.0', narrow: false, notCurrent: false, early: false }
    expect(placeBody('publisher:ci', p)).toEqual({ source: 'publisher:ci', version: '1.0' })
    expect(placeBody('tenant', { ...p, narrow: true, notCurrent: true, early: true }))
      .toEqual({ source: 'tenant', version: '1.0', visibility: 'private', current: false, early: true })
  })
  it('keeps the colon of a source and the plus of a version in a path: the API refuses escaped paths', () => {
    expect(sourceSeg('publisher:enterest-ci')).toBe('publisher:enterest-ci')
    expect(versionSeg('1.0.0+build.7')).toBe('1.0.0+build.7')
  })
  it('places only with promote, which admin does not imply', () => {
    expect(new Perms(true).mayPromote('prod', 'x')).toBe(true)
    expect(new Perms(false, [{ principal: 'role:kc|a', verbs: ['admin'] }]).mayPromote('prod', 'x')).toBe(false)
    const p = new Perms(false, [{ principal: 'role:kc|a', verbs: ['promote'], channel: 'prod', extension: 'x' }])
    expect(p.mayPromote('prod', 'x')).toBe(true)
    expect(p.mayPromote('prod', 'y')).toBe(false)
    expect(p.mayPromote('dev', 'x')).toBe(false)
  })
  it('reads and writes a hold', () => {
    expect(holdWords('0s')).toBe('none')
    expect(holdWords(undefined)).toBe('none')
    expect(holdWords('24h0m0s')).toBe('24h')
    expect(holdWords('1h30m0s')).toBe('1h30m')
    expect(holdWords('10m0s')).toBe('10m')
    expect(holdWords('10s')).toBe('10s')
    expect(holdInput(' 2d ')).toBe('48h')
    expect(holdInput('90m')).toBe('90m')
    expect(holdInput('0')).toBe('0')
  })
  it("reads a version's state from its builds", () => {
    expect(buildsState([{ state: 'available' }, { state: 'held' }])).toBe('held')
    expect(buildsState([{ state: 'held', revoked_at: '2026-10-25T00:00:00Z' }])).toBe('revoked')
    expect(buildsState([{ state: 'withdrawn', revoked_at: '2026-10-25T00:00:00Z' }])).toBe('withdrawn')
    expect(buildsState([{ state: 'available' }])).toBe('available')
  })
  it('orders versions as the server does', () => {
    expect(compareVersions('1.9.0', '1.10.0')).toBe(-1)
    expect(compareVersions('v1.010', 'v1.10')).toBe(-1) // equal numbers: bytewise
    expect(compareVersions('1.0', '1.0-rc1')).toBe(-1)
    expect(compareVersions('e2e-dist-2', 'e2e-dist')).toBe(1)
    expect(compareVersions('2.0', '2.0')).toBe(0)
  })
  it('flags a newer version only when the catalogue has one past what the channel holds', () => {
    expect(newer({ current: '1.0', newest: '1.1' })).toBe(true)
    expect(newer({ newest: '1.1' })).toBe(true)
    expect(newer({ current: '1.2', newest: '1.1' })).toBe(false) // a held version placed early
    expect(newer({ current: '1.1', newest: '1.1' })).toBe(false)
    expect(newer({ current: '1.1' })).toBe(false)
  })
  it("says what a change without a mode leaves, as the server does, and never starts from a removed source", () => {
    expect(keptMode({ source: '', mode: '' }, 'tenant')).toBe('none (as it lands)')
    expect(keptMode({ source: 'publisher:ci', mode: 'auto' }, 'tenant')).toMatch(/^manual/)
    expect(keptMode({ source: 'publisher:ci', mode: 'follow' }, 'upstream:org')).toMatch(/^manual/)
    expect(keptMode({ source: 'publisher:ci', mode: 'follow' }, 'publisher:ci')).toBe('keep it (follow)')
    expect(keptMode({ source: 'upstream:org', mode: '' }, 'tenant')).toBe('keep it (as it lands)')
    expect(changeTo({ name: 'x', source: 'upstream:(removed)', available: { 'publisher:ci': 1 } })).toBe('publisher:ci')
    expect(changeTo({ name: 'x', source: 'upstream:org', available: {} })).toBe('upstream:org')
  })
  it('counts a reason in bytes', () => {
    expect(utf8Bytes('CVE')).toBe(3)
    expect(utf8Bytes('Ü')).toBe(2)
  })
})
