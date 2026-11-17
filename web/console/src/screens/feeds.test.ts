import { describe, expect, it } from 'vitest'
import { feedBody, feedBundleLine, feedCounts, feedExporterOlder, feedOlder, feedStatus, type Feed, feedKinds } from './Feeds'
import { upstreamOlder } from './Upstreams'
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
  it('says a source is newer than this kista (spec 0046)', () => {
    expect(feedOlder({})).toBe('')
    expect(feedOlder({ waiting: { 'unknown kind': 0 } })).toBe('')
    expect(feedOlder({ waiting: { 'unknown kind': 2, newer: 1 } })).toBe(
      'This kista is older than its source: documents wait for an upgrade (newer, unknown kind).')
    expect(feedExporterOlder({})).toBe('')
    expect(feedExporterOlder({ waiting_without_content: { 'unknown kind': 2, 'media type': 1 } })).toBe(
      'The exporting kista is older than its source: 3 statements arrived without content (media type, unknown kind).')
    // the upstream's line comes from the record's member, never from the run's error text
    expect(upstreamOlder({})).toBe('')
    expect(upstreamOlder({ older_than_source: { revocations_unknown: 2 } })).toMatch(/2 revocations in a state it does not know/)
    expect(upstreamOlder({ older_than_source: { api: true } })).toMatch(/not read until an upgrade/)
  })
  it('names a feed document by its verified source', () => {
    expect(originText({ origin: 'feed', issuer: 'https://enterest.example/api/v1/tenants/acme', source_origin: 'tenant', attached_by: 'f-1' })).toBe(
      'the feed of https://enterest.example/api/v1/tenants/acme (signature verified)')
    expect(originText({ origin: 'feed', issuer: 'https://x.example/api/v1/tenants/a', source_origin: 'publisher:ci' })).toBe(
      'the feed of https://x.example/api/v1/tenants/a, by publisher:ci (signature verified)')
    expect(originText({ origin: 'tenant', attached_by: 'os:1:root' })).toBe('the tenant (os:1:root)')
  })
})
