import { describe, expect, it } from 'vitest'
import { fingerprintSeg } from '../lib/http'
import { countsLine, entryNames, upstreamBody, type UpstreamForm } from './Upstreams'

// Phase 2b's rules in the console.

describe('upstreams', () => {
  const form: UpstreamForm = { name: 'core', kind: 'duckdb-core', channel: 'prod', mode: 'mirror', visibility: 'private',
    prefix: 'https://x.example/', keys: 'sha256:aa', names: 'json, parquet json', credential: 'sub' }
  it("sends a repository's prefix, keys and credential only", () => {
    expect(upstreamBody(form, ['linux_amd64'])).toEqual({ name: 'core', kind: 'duckdb-core', channel: 'prod', mode: 'mirror', visibility: 'private',
      platforms: ['linux_amd64'], extensions: [{ name: 'json' }, { name: 'parquet' }] })
    const repo = upstreamBody({ ...form, kind: 'repository' }, [])
    expect(repo.prefix).toBe('https://x.example/')
    expect(repo.keys).toEqual(['sha256:aa'])
    expect(repo.credential).toBe('sub')
  })
  it('reads names once each, and a run as its counts above zero', () => {
    expect(entryNames('a, b  a\n*')).toEqual(['a', 'b', '*'])
    expect(entryNames('  ')).toEqual([])
    expect(countsLine({ started: '', ended: '', counts: { released: 2, unchanged: 0, missing: 1 } })).toBe('released 2 · missing 1')
    expect(countsLine({ started: '', ended: '', counts: { unchanged: 0 } })).toBe('nothing to do')
    expect(countsLine(undefined)).toBe('')
  })
  it('keeps a key fingerprint’s colon in a path: the API refuses escaped paths', () => {
    expect(fingerprintSeg('sha256:' + 'a'.repeat(64))).toBe('sha256:' + 'a'.repeat(64))
  })
})
