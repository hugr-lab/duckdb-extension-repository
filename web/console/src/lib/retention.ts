// Version retention in channels (spec 0042): a rule (the tenant's default, a channel's own) keeps the
// newest N versions of each name per DuckDB version (a line) the channel serves, always the current
// ones and the pinned ones; the retention pass retires the others and purges them after a grace. What
// the console words and computes here comes from the API's answers; the server decides.
import { compareVersions } from '../screens/Catalogue'
import type { Release } from '../screens/types'

/** A field of a rule in force: its value and where it comes from. */
export interface InForceField {
  value: number | string
  from: 'channel' | 'tenant'
  capped?: boolean
}

/** A channel's rule in force (the API's in_force): a field with no value anywhere is null, and named
 * in missing; no rule is in force then. */
export interface InForce {
  keep: InForceField | null
  grace: InForceField | null
  missing: string[]
}

/** GET …/channels/{c}/retention: the channel's own rule (keep a number, "none", or null for the
 * tenant's; grace null for the tenant's), the rule in force, the pass's last look and next one. */
export interface ChannelRetention {
  channel: string
  keep: number | 'none' | null
  grace: string | null
  in_force: InForce
  checked_at: string | null
  next_run: string | null
}

/** GET …/tenants/{t}/retention: the tenant's default (null: not set). */
export interface TenantRetention {
  keep: number | null
  grace: string | null
}

export interface PlanRelease {
  id: string
  platform: string
  slot: string
  body_hash: string
  bytes: number
  shared: boolean
  shared_by: string[]
  others: number
  lines?: string[]
}

/** A (name, version) the next pass retires. */
export interface PlanRetire {
  source: string
  name: string
  version: string
  lines: string[]
  platforms: string[]
  releases: PlanRelease[]
  bytes: number
  kept: Record<string, string[]>
  kept_by: PlanRelease[]
}

/** A retired release: past its grace (purged by the next pass unless pinned), or in it. */
export interface PlanRetired {
  source: string
  name: string
  version: string
  release: PlanRelease
  retired_at: string | null
  purge_at: string | null
  pinned: boolean
}

export interface Plan {
  in_force: InForce
  next_run: string | null
  purge_off: boolean
  retire: PlanRetire[]
  purge: PlanRetired[]
  retired: PlanRetired[]
  next?: string
}

export interface Pin {
  id: string
  name: string
  version: string
  duckdb_version?: string
  reason: string
  pinned_by: string
  pinned_at: string
}

/** A channel's DuckDB version as its info answers it, with its C API maxima. */
export interface ChannelVersion {
  duckdb_version: string
  c_apis?: string[]
}

/** Whether a rule is in force: a keep that is not none, and a grace. */
export const inForce = (f: InForce) => f.keep !== null && f.keep.value !== 'none' && f.grace !== null

/** A grace as the console words it: 0s purges at the next pass. */
export function graceWords(g: string) {
  if (/^0[a-z]*$/.test(g)) return '0 (purged at the next pass)'
  return g.replace(/([hm])0s$/, '$1').replace(/h0m$/, 'h')
}

/** A rule in force in words, as a stage card and the Retention tab show it. */
export function retentionWords(f?: InForce): string {
  if (!f) return '…'
  if (f.keep?.value === 'none') return 'no retention (none in this channel)'
  if (!inForce(f)) return `no retention (no ${f.missing.join(' and no ')} set)`
  return `keep ${f.keep!.value} · grace ${graceWords(String(f.grace!.value))}`
}

/** A field in force with where it comes from ("5, the channel's"). */
export function fieldWords(f: InForceField | null, grace = false): string {
  if (!f) return 'not set'
  const v = grace ? graceWords(String(f.value)) : String(f.value)
  return `${v}, the ${f.from}'s${f.capped ? ' (capped by the server)' : ''}`
}

/** A PUT's rule from the form's words: keep "" (left out), "none" or a number; grace "" (left out) or
 * a duration, days as hours. */
export function ruleBody(keep: string, grace: string): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  const k = keep.trim()
  if (k === 'none') out.keep = 'none'
  else if (k !== '') out.keep = Number(k)
  const g = grace.trim()
  const d = /^(\d+)d$/.exec(g)
  if (g !== '') out.grace = d ? `${Number(d[1]) * 24}h` : g
  return out
}

const capi = (s?: string) => {
  const m = /^v?(\d+)\.(\d+)\.(\d+)$/.exec(s ?? '')
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null
}

/** The channel's lines a release serves: a C API build's, those whose maximum for its major accepts
 * it; another build's, its DuckDB version. */
export function linesOf(r: Pick<Release, 'abi' | 'build_duckdb_version' | 'build_c_api'>, versions: ChannelVersion[]): string[] {
  if (r.abi !== 'c_struct') return versions.some((v) => v.duckdb_version === r.build_duckdb_version) ? [r.build_duckdb_version!] : []
  const b = capi(r.build_c_api)
  if (!b) return []
  return versions.filter((v) => (v.c_apis ?? []).some((m) => {
    const x = capi(m)
    return x !== null && x[0] === b[0] && (b[1] < x[1] || b[1] === x[1] && b[2] <= x[2])
  })).map((v) => v.duckdb_version)
}

/** A name's versions in a line: the current ones (the flat path's on each platform), those the next
 * pass keeps, those it retires, and those retired. */
export interface LineRow {
  name: string
  line: string
  current: string[]
  kept: string[]
  retire: string[]
  retired: string[]
}

/** The rows of the Retention tab's versions by line, from the channel's releases (its selected
 * sources', every state) and the plan's retire list, by name and line. */
export function lineRows(releases: Release[], versions: ChannelVersion[], retire: PlanRetire[]): LineRow[] {
  const rows = new Map<string, LineRow>()
  const add = (set: string[], v: string) => { if (!set.includes(v)) set.push(v) }
  const row = (name: string, line: string) => {
    const k = `${name}|${line}`
    if (!rows.has(k)) rows.set(k, { name, line, current: [], kept: [], retire: [], retired: [] })
    return rows.get(k)!
  }
  for (const p of retire) for (const l of p.lines) add(row(p.name, l).retire, p.version)
  const best = new Map<string, Release>()
  for (const r of releases) {
    if (r.selected === false || r.withdrawn) continue
    for (const l of linesOf(r, versions)) {
      const x = row(r.name, l)
      if (r.state === 'retired') add(x.retired, r.version)
      if (r.state !== 'active' && r.state !== 'deprecated') continue
      if (!x.retire.includes(r.version)) add(x.kept, r.version)
      const k = `${r.name}|${l}|${r.platform}`
      if (r.state === 'active' && r.seq > 0 && (best.get(k)?.seq ?? 0) < r.seq) best.set(k, r)
    }
  }
  for (const [k, r] of best) {
    const [name, line] = k.split('|')
    add(row(name, line).current, r.version)
  }
  const desc = (a: string, b: string) => compareVersions(b, a)
  return [...rows.values()].map((x) => ({ ...x, current: x.current.sort(desc), kept: x.kept.sort(desc), retire: x.retire.sort(desc),
    retired: x.retired.sort(desc) })).sort((a, b) => a.name.localeCompare(b.name) || compareVersions(b.line, a.line))
}
