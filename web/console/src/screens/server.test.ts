import { describe, expect, it } from 'vitest'
import { deletable } from './Server'

describe('deletable', () => {
  const now = new Date('2026-10-12T12:00:00Z')
  it('is a suspended tenant from deletable_at', () => {
    expect(deletable({ state: 'suspended', deletable_at: '2026-10-12T11:00:00Z' }, now)).toBe(true)
    expect(deletable({ state: 'suspended', deletable_at: '2026-10-13T11:00:00Z' }, now)).toBe(false)
  })
  it('is never an active, deleting or old-suspended tenant', () => {
    expect(deletable({ state: 'active', deletable_at: '2026-10-12T11:00:00Z' }, now)).toBe(false)
    expect(deletable({ state: 'deleting' }, now)).toBe(false)
    expect(deletable({ state: 'suspended' }, now)).toBe(false)
  })
})
