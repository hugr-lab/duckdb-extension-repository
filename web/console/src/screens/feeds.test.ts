import { describe, expect, it } from 'vitest'
import { feedBody, feedBundleLine, feedCounts, feedStatus, type Feed, feedKinds } from './Feeds'
import { originText } from './Reports'

// Spec 0011 phase 1b in the console: what a new feed sends, a run in a line, who stands behind a document.

describe('feeds', () => {
  const form = { name: 'enterest', url: 'https://enterest.example/api/v1/tenants/acme', credential: 'src', keys: 'sha256:a, sha256:b sha256:a',
    kinds: [...feedKinds], readers: false, offer: 'upstreams' as const, channels: ['prod'], mode: 'online' as const, export: false }
  it('sends every kind as none named, channels only when offered by channel', () => {
    expect(feedBody(form)).toEqual({ name: 'enterest', url: form.url, credential: 'src', keys: ['sha256:a', 'sha256:b'], readers: false, offer: 'upstreams' })
    expect(feedBody({ ...form, kinds: ['review'], offer: 'channels', readers: true })).toEqual({ name: 'enterest', url: form.url, credential: 'src',
      keys: ['sha256:a', 'sha256:b'], readers: true, offer: 'channels', kinds: ['review'], channels: ['prod'] })
  })
  it('sends a bundle-mode feed without a credential or export, and export only when set', () => {
    expect(feedBody({ ...form, mode: 'bundle', export: true })).toEqual({ name: 'enterest', url: form.url, keys: ['sha256:a', 'sha256:b'],
      readers: false, offer: 'upstreams', mode: 'bundle' })
    expect(feedBody({ ...form, export: true })).toMatchObject({ credential: 'src', export: true })
  })
  it('says what a feed keeps or brought from bundles', () => {
    expect(feedBundleLine({ mode: 'bundle' } as Feed)).toBe('no bundle imported yet')
    expect(feedBundleLine({ mode: 'bundle', bundle_id: 'b-1' } as Feed)).toBe('last bundle b-1')
    expect(feedBundleLine({ mode: 'online', export: true } as Feed)).toBe('kept for export from its next run')
    expect(feedBundleLine({ mode: 'online', export: false } as Feed)).toBe('')
    expect(feedBundleLine({ mode: 'online', export: true, keys_file_at: '2026-10-17T10:00:00Z' } as Feed)).toMatch(/^kept for export, keys file of /)
    expect(feedStatus({ mode: 'bundle', status: 'new' } as Feed)).toBe('no bundle yet')
    expect(feedStatus({ mode: 'online', status: 'new' } as Feed)).toBe('not run yet')
  })
  it('reads a run as its counts', () => {
    expect(feedCounts({ last_run: { counts: { offered: 40, received: 2, refused: 1, withdrawn: 0 } } } as unknown as Feed)).toBe('received 2 · refused 1')
  })
  it('names a feed document by its verified source', () => {
    expect(originText({ origin: 'feed', issuer: 'https://enterest.example/api/v1/tenants/acme', source_origin: 'tenant', attached_by: 'f-1' })).toBe(
      'the feed of https://enterest.example/api/v1/tenants/acme (signature verified)')
    expect(originText({ origin: 'feed', issuer: 'https://x.example/api/v1/tenants/a', source_origin: 'publisher:ci' })).toBe(
      'the feed of https://x.example/api/v1/tenants/a, by publisher:ci (signature verified)')
    expect(originText({ origin: 'tenant', attached_by: 'os:1:root' })).toBe('the tenant (os:1:root)')
  })
})
