// The catalogue as the tenant's home and the decision queue (spec 0041), computed in the console from
// reads a tenant administrator has: the catalogue, each signed channel's rules, follows and hold, and
// the decisions kept in kista. Nothing here is the server's: the queue is what the console shows.
import { compareVersions, removedSource, type CatalogueEntry } from '../screens/Catalogue'
import type { FollowRow, SourceRow } from '../screens/Sources'
import type { OrderEdge } from './order'

/** A decision as the API answers it: a skip of a channel's waiting version, or a dismissal keyed to
 * its situation (a follow's skip reason, a channel's; the channels taking a name, the tenant's). */
export interface Decision {
  id: string
  kind: 'skip' | 'dismiss'
  channel?: string
  source: string
  name: string
  version?: string
  situation?: string | string[]
  decided_by: string
  decided_at: string
  package_type?: string
  etag: string
}

/** A signed channel of the default package type, as the home reads it. */
export interface ChannelReads {
  name: string
  hold: string
  follows: FollowRow[]
  sources: SourceRow[]
}

export interface HomeReads {
  entries: CatalogueEntry[]
  /** in the channels' order's sequence, then the others by name (spec 0043) */
  channels: ChannelReads[]
  decisions: Decision[]
  /** the channels' order of the default package type (spec 0043) */
  order: OrderEdge[]
}

/** Whether a channel follows a source: a follow's rules then place automatically. */
export const follows = (c: ChannelReads, source: string) => c.follows.some((f) => f.source === source)

/** Whether a rule places nothing new by itself: manual, or a follow's once the channel no longer
 * follows its source. */
export const actsManual = (c: ChannelReads, r: Pick<SourceRow, 'mode' | 'source'>) =>
  r.mode === 'manual' || (r.mode === 'follow' && !follows(c, r.source))

/** Whether a rule places automatically: auto, or a follow's while the channel follows its source. */
export const actsAuto = (c: ChannelReads, r: Pick<SourceRow, 'mode' | 'source'>) =>
  r.mode === 'auto' || (r.mode === 'follow' && follows(c, r.source))

/** Whether a skip covers a waiting version: the same (channel, source, name), and the version not
 * newer than the one skipped. */
export function skipped(ds: Decision[], channel: string, source: string, name: string, version: string) {
  return ds.some((d) => d.kind === 'skip' && d.channel === channel && d.source === source && d.name === name && !!d.version &&
    compareVersions(version, d.version) <= 0)
}

/** The skip of (channel, source, name), if any. */
export const skipOf = (ds: Decision[], channel: string, source: string, name: string) =>
  ds.find((d) => d.kind === 'skip' && d.channel === channel && d.source === source && d.name === name)

/** Whether two situations are one: a string, or the channels' names (the server keeps them sorted). */
function sameSituation(a: Decision['situation'], b: string | string[]) {
  if (typeof b === 'string') return a === b
  return Array.isArray(a) && a.length === b.length && a.every((x, i) => x === b[i])
}

/** Whether a dismissal covers an item: the same scope ('' the tenant's), source and name, and the
 * situation it was keyed to unchanged. */
export function dismissed(ds: Decision[], channel: string, source: string, name: string, situation: string | string[]) {
  return ds.some((d) => d.kind === 'dismiss' && (d.channel ?? '') === channel && d.source === source && d.name === name &&
    sameSituation(d.situation, situation))
}

/** The channels whose rules take an entry's source for its name, sorted: a tenant dismissal's
 * situation. */
export const taking = (e: Pick<CatalogueEntry, 'rules'>) => Object.keys(e.rules).sort()

/** What a channel's column shows for an entry. */
export type Cell =
  | { kind: 'rule'; mode?: string; current?: string; waiting?: string; waitsUntil?: string; outcome?: string }
  | { kind: 'other'; source: string } // the channel serves the name from another source
  | { kind: 'skipped'; reason: string } // a follow of the source skips the name
  | { kind: 'none' }

/** An entry's cell in a channel's column: the rule (its waiting version less a skip), another
 * source's rule, a follow's skip, or nothing. */
export function cellOf(e: Pick<CatalogueEntry, 'source' | 'name'>, c: ChannelReads, ds: Decision[]): Cell {
  const r = c.sources.find((s) => s.name === e.name)
  if (r?.source && r.source !== e.source) return { kind: 'other', source: r.source }
  if (r?.source) {
    const waiting = r.waiting && !skipped(ds, c.name, r.source, r.name, r.waiting) ? r.waiting : undefined
    return { kind: 'rule', mode: r.mode, current: r.current, waiting, waitsUntil: r.waits_until, outcome: r.outcome }
  }
  const reason = c.follows.find((f) => f.source === e.source)?.skips[e.name]
  return reason ? { kind: 'skipped', reason } : { kind: 'none' }
}

/** A queue item, by its group. */
export type Item =
  | { group: 'manual'; channel: string; source: string; name: string; waiting: string; current?: string; mode?: string }
  | { group: 'holds'; channel?: string; source: string; name: string; version?: string; until: string }
  | { group: 'skipped'; channel: string; source: string; name: string; reason: string }
  | { group: 'nochannel'; source: string; name: string; taking: string[] }
  | { group: 'failed'; channel: string; source: string; name: string; outcome: string; mode?: string }

export const groups: [Item['group'], string][] = [
  ['manual', 'New versions under manual rules'],
  ['holds', 'Holds ending within a day'],
  ['skipped', 'Names a follow skips'],
  ['nochannel', 'In no channel'],
  ['failed', 'Placements that failed'],
]

/** An item's key, unique in the queue. */
export const itemKey = (i: Item) => [i.group, 'channel' in i ? i.channel ?? '' : '', i.source, i.name, 'version' in i ? i.version ?? '' : ''].join('|')

const day = 24 * 3600 * 1000

/** The queue (spec 0041): what kista leaves to an administrator, less what the decisions cover. "Within
 * a day" is the console's choice. */
export function queue(reads: HomeReads, now = Date.now()): Item[] {
  const { entries, channels, decisions: ds } = reads
  const out: Item[] = []
  const soon = (at?: string) => {
    if (!at) return false
    const t = Date.parse(at)
    return t > now && t <= now + day
  }
  for (const c of channels) {
    for (const r of c.sources) {
      if (!r.source || removedSource(r.source)) continue
      if (r.waiting && actsManual(c, r) && !skipped(ds, c.name, r.source, r.name, r.waiting)) {
        out.push({ group: 'manual', channel: c.name, source: r.source, name: r.name, waiting: r.waiting, current: r.current, mode: r.mode })
      }
      if (soon(r.waits_until)) out.push({ group: 'holds', channel: c.name, source: r.source, name: r.name, version: r.waits_for, until: r.waits_until! })
      if (r.outcome) out.push({ group: 'failed', channel: c.name, source: r.source, name: r.name, outcome: r.outcome, mode: r.mode })
    }
    for (const f of c.follows) {
      for (const [name, reason] of Object.entries(f.skips).sort()) {
        if (!dismissed(ds, c.name, f.source, name, reason)) out.push({ group: 'skipped', channel: c.name, source: f.source, name, reason })
      }
    }
  }
  for (const e of entries) {
    if (removedSource(e.source)) continue
    // a held version of a name no automatic rule takes yet: the rules' waits_until say the rest
    const auto = channels.some((c) => c.sources.some((r) => r.name === e.name && r.source === e.source && actsAuto(c, r)))
    if (!auto) {
      for (const [v, until] of Object.entries(e.held_until ?? {})) {
        if (soon(until)) out.push({ group: 'holds', source: e.source, name: e.name, version: v, until })
      }
    }
    const t = taking(e)
    if (channels.some((c) => !(c.name in e.rules)) && !dismissed(ds, '', e.source, e.name, t)) {
      out.push({ group: 'nochannel', source: e.source, name: e.name, taking: t })
    }
  }
  return out
}

/** A placement's state in a channel, as an entry's builds answer it. */
interface Placed {
  channel: string
  state: string
}

/** The channel a version can be promoted from into target (spec 0041): one holding it as an active or
 * deprecated release that the caller may publish from (a promotion takes publish there), the target's
 * previous stage first (spec 0043), else the first by name; '' for none, and the version is placed
 * from the catalogue instead. */
export function promoteFrom(builds: { version: string; builds: { placed?: Placed[] }[] }[], version: string, target: string,
  mayPublish: (channel: string) => boolean = () => true, previous = '') {
  const holding = new Set<string>()
  for (const v of builds) {
    if (v.version !== version) continue
    for (const b of v.builds) {
      for (const p of b.placed ?? []) if (p.channel !== target && (p.state === 'active' || p.state === 'deprecated')) holding.add(p.channel)
    }
  }
  const sorted = [...holding].sort()
  return (holding.has(previous) ? [previous, ...sorted] : sorted).find(mayPublish) ?? ''
}

/** A decision's body (PUT …/decisions) for a queue item: Skip of a waiting version, or Dismiss of
 * a follow's skip or of an entry in no channel. */
export function decisionBody(i: Item): Record<string, unknown> | null {
  switch (i.group) {
    case 'manual':
      return { kind: 'skip', channel: i.channel, source: i.source, name: i.name, version: i.waiting }
    case 'skipped':
      return { kind: 'dismiss', channel: i.channel, source: i.source, name: i.name, situation: i.reason }
    case 'nochannel':
      return { kind: 'dismiss', source: i.source, name: i.name, situation: i.taking }
  }
  return null
}
