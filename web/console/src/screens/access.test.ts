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

describe('the grant form', () => {
  it('sends channel only when one is chosen, the name under its target', async () => {
    const { grantBody, offeredVerbs } = await import('./Access')
    expect(grantBody('role:kc|t', 'extension_group', 'platform', '', ['install'])).toEqual({ principal: 'role:kc|t', verbs: ['install'], extension_group: 'platform' })
    expect(grantBody('role:kc|t', 'extension', 'acl', 'prod', ['admin'])).toEqual({ principal: 'role:kc|t', verbs: ['admin'], extension: 'acl', channel: 'prod' })
    expect(grantBody('role:kc|t', 'tenant', '', 'prod', ['audit'])).toEqual({ principal: 'role:kc|t', verbs: ['audit'] })
    expect(offeredVerbs('tenant', 'role:kc|t')).toContain('audit')
    expect(offeredVerbs('extension_group', 'role:kc|t')).not.toContain('audit')
    expect(offeredVerbs('channel', 'role:kc|t')).toContain('feed') // spec 0011: on the tenant or a channel
    expect(offeredVerbs('extension', 'role:kc|t')).not.toContain('feed')
    expect(offeredVerbs('tenant', 'issuer:kc')).toEqual(['install'])
    expect(offeredVerbs('channel', 'publisher:ci', ['publish', 'promote'])).toEqual(['publish', 'promote'])
  })
  it('words a string with no kind', () => {
    expect(principalKind('foo')).toMatch(/not a principal/)
  })
})

describe('forms input', () => {
  it('splits member names once each; reads required claims; refuses a bad claim path', async () => {
    const { splitNames } = await import('./Groups')
    const { requiredClaims } = await import('./Identity')
    expect(splitNames('acl, tresor  acl\nhttpfs')).toEqual(['acl', 'tresor', 'httpfs'])
    expect(requiredClaims('tid=t1, azp=kista-console')).toEqual({ tid: 't1', azp: 'kista-console' })
    expect(() => requiredClaims('tid')).toThrow(/claim=value/)
    expect(() => claimPath('[1]')).toThrow(/claim path/)
    expect(() => claimPath('[nope')).toThrow(/claim path/)
  })
})
