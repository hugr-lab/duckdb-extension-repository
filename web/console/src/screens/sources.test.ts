import { describe, expect, it } from 'vitest'
import { sourceChoices } from './Sources'

describe('sourceChoices', () => {
  it('offers the sources with releases, the current one and the tenant, never a removed one', () => {
    expect(sourceChoices({ name: 'x', source: 'upstream:core', available: { 'publisher:ci': 2, 'upstream:(removed)': 1 } }))
      .toEqual(['publisher:ci', 'tenant', 'upstream:core'])
    expect(sourceChoices({ name: 'x', source: '', available: {} })).toEqual(['tenant'])
    // a removed source cannot be chosen again, even the current one
    expect(sourceChoices({ name: 'x', source: 'upstream:(removed)', available: { 'upstream:(removed)': 3 } })).toEqual(['tenant'])
  })
})
