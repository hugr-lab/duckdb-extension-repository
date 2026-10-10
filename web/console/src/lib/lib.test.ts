import { describe, expect, it } from 'vitest'
import { renewAt } from './auth'
import { Perms } from './context'
import { apiPrefix, scopeOf } from './scope'
import { days } from '../screens/Stats'

describe('renewAt', () => {
  it('renews before the API refuses the token as stale, or before it expires', () => {
    // a token valid 8 hours, the API wanting it issued within an hour: renewed after 55 minutes
    expect(renewAt(1000, 1000 + 8 * 3600, 3600)).toBe((1000 + 3600 - 300) * 1000)
    // a token valid 10 minutes: a minute before it expires
    expect(renewAt(1000, 1600, 3600)).toBe(1540 * 1000)
    expect(renewAt(undefined, undefined, 3600)).toBe(Infinity)
    // a short limit: half of it, never at once
    expect(renewAt(1000, 1000 + 8 * 3600, 120)).toBe((1000 + 60) * 1000)
  })
})

describe('scopes', () => {
  it('come from the path', () => {
    expect(scopeOf('/ui/')).toEqual({ kind: 'landing' })
    expect(scopeOf('/ui/server/tenants/acme')).toEqual({ kind: 'server' })
    expect(scopeOf('/ui/t/acme/channels/prod')).toEqual({ kind: 'tenant', tenant: 'acme' })
    expect(scopeOf('/ui/t/Bad_Name/')).toEqual({ kind: 'landing' })
    expect(apiPrefix({ kind: 'tenant', tenant: 'acme' })).toBe('/tenants/acme/')
  })
})

describe('Perms', () => {
  const g = (verbs: string[], channel?: string, extension?: string) => ({ principal: 'subject:corp|x', verbs, channel, extension })
  it('reads what whoami grants', () => {
    const ext = new Perms(false, [g(['admin'], 'prod', 'tresor'), g(['install'])])
    expect(ext.tenantAdmin).toBe(false)
    expect(ext.channelAdmin('prod')).toBe(false)
    expect(ext.extensionsIn('prod')).toEqual(['tresor'])
    expect(ext.anyIn('staging')).toBe(false)
    const ch = new Perms(false, [g(['admin'], 'prod')])
    expect(ch.channelAdmin('prod') && !ch.channelAdmin('staging')).toBe(true)
    expect(new Perms(false, [g(['admin'])]).tenantAdmin).toBe(true)
    expect(new Perms(false, [], true).canAudit).toBe(true)
    expect(new Perms(false, [g(['install'])]).anything).toBe(false)
    expect(new Perms(true).channelAdmin('any')).toBe(true)
  })
})

describe('days', () => {
  it('lists every day of a range', () => {
    expect(days('2026-02-27', '2026-03-02')).toEqual(['2026-02-27', '2026-02-28', '2026-03-01', '2026-03-02'])
  })
})
