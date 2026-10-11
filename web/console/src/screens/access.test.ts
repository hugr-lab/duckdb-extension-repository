import { describe, expect, it } from 'vitest'
import { Perms } from '../lib/context'
import { principalKind } from './Access'
import { claimPath } from './Identity'

// Phase 2a's rules in the console (the API decides; these only hide and word things).

describe('Perms with grants on extension groups (spec 0017)', () => {
  it('a group grant is never tenant- or channel-wide', () => {
    const p = new Perms(false, [{ principal: 'role:kc|t', verbs: ['admin'], extension_group: 'platform' },
      { principal: 'role:kc|t', verbs: ['install'], extension_group: 'geo' }])
    expect(p.tenantAdmin).toBe(false)
    expect(p.channelAdmin('prod')).toBe(false)
    expect(p.groups).toEqual(['platform'])
    expect(p.anything).toBe(true)
  })
})

describe('principalKind', () => {
  it('words the kind; a person and a service are named as such', () => {
    expect(principalKind('subject:kc|eve')).toBe('a person')
    expect(principalKind('client:kc|node')).toBe('a service')
    expect(principalKind('role:kc|team')).toBe('a role')
    expect(principalKind('publisher:ci')).toBe('a publisher')
  })
})

describe('claimPath', () => {
  it('takes keys joined by dots, or a JSON array when a key has dots', () => {
    expect(claimPath('realm_access.roles')).toEqual(['realm_access', 'roles'])
    expect(claimPath('["https://x.example/roles"]')).toEqual(['https://x.example/roles'])
    expect(claimPath('  ')).toBeUndefined()
  })
})
