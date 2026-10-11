import { describe, expect, it } from 'vitest'
import { mediaTypes, stale, summaryText } from './Reports'
import type { Attachment } from './types'

// Spec 0011's summaries in the console: text, by kind.

describe('reports', () => {
  it('reads each kind of summary as a line of text', () => {
    expect(summaryText('sbom', { format: 'cyclonedx 1.6', components: 3, licences: 2, crypto_assets: 1 })).toBe(
      'cyclonedx 1.6, 3 components, 2 licences, 1 crypto assets')
    expect(summaryText('vulnerabilities', { findings: 3, critical: 1, low: 2, not_affected: 1 })).toBe(
      '3 findings: 1 critical, 2 low; 1 not affected')
    expect(summaryText('vulnerabilities', { findings: 0 })).toBe('no findings')
    expect(summaryText('review', { verdict: 'conditional', analyser: 'Enterest', findings: 1, high: 1 })).toBe(
      'conditional by Enterest; 1 findings: 1 high')
    expect(summaryText('provenance', { builder: 'gh', repository: 'https://github.com/x/y', ref: 'v1', verified: false })).toBe(
      'built by gh, from https://github.com/x/y@v1, not verified')
    expect(summaryText('notes', { first_line: '<b>hi</b>' })).toBe('<b>hi</b>') // text: React escapes it
  })
  it('flags a scan older than 30 days', () => {
    const a = { kind: 'vulnerabilities', summary: { scanned_at: '2026-01-01T00:00:00Z' }, issued_at: '2026-10-01T00:00:00Z' } as unknown as Attachment
    expect(stale(a, Date.parse('2026-01-20T00:00:00Z'))).toBe(false)
    expect(stale(a, Date.parse('2026-02-20T00:00:00Z'))).toBe(true)
    expect(stale({ ...a, kind: 'sbom' }, Date.parse('2027-01-01T00:00:00Z'))).toBe(false)
  })
  it('offers the media types the API takes', () => {
    expect(Object.keys(mediaTypes)).toEqual(['sbom', 'vulnerabilities', 'review', 'provenance', 'notes'])
  })
})
