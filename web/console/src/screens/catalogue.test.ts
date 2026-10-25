import { describe, expect, it } from 'vitest'
import { Perms } from '../lib/context'
import { sourceSeg } from '../lib/http'
import { modeWords, placeBody, purgeable, versionSeg, versionState } from './Catalogue'
import { holdInput, holdWords } from './Sources'

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
})
