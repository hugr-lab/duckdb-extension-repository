import { describe, expect, it } from 'vitest'
import { fingerprintSeg } from '../lib/http'
import { countsLine, entryNames, modeWords, outcomeWords, revocationWords, takesRevocations, upstreamBody, type UpstreamForm } from './Upstreams'

// Phase 2b's rules in the console.

describe('upstreams', () => {
  const form: UpstreamForm = { name: 'core', kind: 'duckdb-core', channel: 'prod', mode: 'mirror', fetch: 'eager', visibility: 'private',
    prefix: 'https://x.example/', keys: 'sha256:aa', names: 'json, parquet json', credential: 'sub', revocations: false }
  it("sends a repository's prefix, keys and credential only", () => {
    expect(upstreamBody(form, ['linux_amd64'])).toEqual({ name: 'core', kind: 'duckdb-core', channel: 'prod', mode: 'mirror', fetch: 'eager', visibility: 'private',
      platforms: ['linux_amd64'], extensions: [{ name: 'json' }, { name: 'parquet' }] })
    const repo = upstreamBody({ ...form, kind: 'repository' }, [])
    expect(repo.prefix).toBe('https://x.example/')
    expect(repo.keys).toEqual(['sha256:aa'])
    expect(repo.credential).toBe('sub')
  })
  it("sends a mirror's fetch mode only (spec 0032), and says a lazy mirror is one", () => {
    expect(upstreamBody({ ...form, fetch: 'lazy' }, []).fetch).toBe('lazy')
    expect(upstreamBody({ ...form, mode: 'pull-through', fetch: 'lazy' }, []).fetch).toBeUndefined()
    expect(upstreamBody({ ...form, fetch: 'lazy' }, [], true).fetch).toBe('eager') // a passthrough channel takes no lazy mirror
    expect(outcomeWords('not_asked')).toBe('not asked yet')
    expect(outcomeWords('released')).toBe('released')
    expect(modeWords({ mode: 'mirror', fetch: 'lazy' })).toBe('mirror, lazy')
    expect(modeWords({ mode: 'mirror', fetch: 'eager' })).toBe('mirror')
    expect(modeWords({ mode: 'pull-through' })).toBe('pull-through')
  })
  it('sends the revocations switch only where it applies (spec 0039), and words a stored one', () => {
    const on = { ...form, revocations: true }
    expect(upstreamBody(on, []).revocations).toBeUndefined() // a mirror of DuckDB core
    expect(upstreamBody({ ...on, kind: 'repository' }, []).revocations).toBe(true)
    expect(upstreamBody({ ...on, kind: 'repository', mode: 'pull-through' }, []).revocations).toBeUndefined()
    expect(upstreamBody({ ...on, mode: 'bundle' }, []).revocations).toBe(true)
    expect(upstreamBody({ ...form, kind: 'repository' }, []).revocations).toBeUndefined()
    expect(takesRevocations({ kind: 'repository', mode: 'mirror' })).toBe(true)
    expect(takesRevocations({ kind: 'duckdb-community', mode: 'mirror' })).toBe(false)
    expect(revocationWords({ status: 'applied', state: 'withdrawn', applied_state: 'withdrawn' })).toBe('applied as a block')
    expect(revocationWords({ status: 'applied', state: 'yanked', applied_state: 'yanked' })).toBe('applied as a yank')
    expect(revocationWords({ status: 'waiting', state: 'withdrawn', applied_state: 'yanked' })).toBe('withdrawn waits (yanked applied)')
    expect(revocationWords({ status: 'waiting', state: 'yanked' })).toBe('waits')
    expect(revocationWords({ status: 'lifted', state: 'yanked', lifted_state: 'yanked' })).toBe('yanked lifted')
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
