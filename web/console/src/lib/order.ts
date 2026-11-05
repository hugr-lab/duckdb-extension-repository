// The channels' order (spec 0043): each signed channel may name its next channel, a strict chain
// (nightly → staging → prod), kept in kista for every administrator. It is for display only: the
// pipeline's stages, the columns' sequence and the default source of a promotion. Nothing here is
// the server's: what waits for a stage is computed from reads an administrator already has.
import { compareVersions } from '../screens/Catalogue'
import { type Decision, skipped } from './queue'

/** An edge of the order, as GET …/channel-order answers it. */
export interface OrderEdge {
  channel: string
  next: string
}

/** The order's chains, each from its first stage (a channel with a next and no previous), by the
 * first stage's name. */
export function chainsOf(edges: OrderEdge[]): string[][] {
  const next = new Map(edges.map((e) => [e.channel, e.next]))
  const prev = new Set(edges.map((e) => e.next))
  return edges.map((e) => e.channel).filter((c) => !prev.has(c)).sort().map((first) => {
    const chain = [first]
    for (let n = next.get(first); n !== undefined && !chain.includes(n); n = next.get(n)) chain.push(n)
    return chain
  })
}

/** Channel names in the order's sequence (its chains, one after another), then the others by name. */
export function inOrder(names: string[], edges: OrderEdge[]): string[] {
  const out = chainsOf(edges).flat().filter((c) => names.includes(c))
  return [...out, ...names.filter((c) => !out.includes(c)).sort()]
}

/** A channel's previous stage, '' for none. */
export const previousOf = (edges: OrderEdge[], channel: string) => edges.find((e) => e.next === channel)?.channel ?? ''

/** The order the dialog sends (the whole order): the next each listed channel names now ('' none),
 * and the edges read of the channels it does not list, kept as they are. */
export function orderBody(read: OrderEdge[], listed: string[], next: Record<string, string>): OrderEdge[] {
  return [
    ...read.filter((e) => !listed.includes(e.channel)),
    ...listed.filter((c) => next[c]).map((c) => ({ channel: c, next: next[c] })),
  ]
}

/** A release of a name in a channel as the channel's index answers it (GET …/channels/{c}/extensions/{name}):
 * its build's ABI and DuckDB version or C API, the DuckDB versions its versioned path serves, those
 * whose flat path it is current for, and for a yanked, retired (spec 0042) or withdrawn one those it would serve. */
export interface IndexRelease {
  version: string
  platform: string
  state: string
  abi?: string
  build_duckdb_version?: string
  build_c_api?: string
  serves: { duckdb_version: string }[]
  current_for: string[]
  would_serve?: string[]
  withdrawn?: boolean
  body_hash?: string
}

/** A release the channel purged (the index's purged member, for the name's administrators): its
 * version, platform and slot, and the DuckDB version the slot is ('' for a C API build's). */
export interface PurgedRelease {
  version: string
  platform: string
  slot: string
  duckdb_version: string
}

/** A release's slot, as the server computes a build's (store.Build.Slot): capi:<major> for a C API
 * build, else its DuckDB version. */
export function slotOf(r: Pick<IndexRelease, 'abi' | 'build_c_api' | 'build_duckdb_version'>) {
  const major = /^v?(\d+)\./.exec(r.build_c_api ?? '')?.[1]
  return r.abi === 'c_struct' && major !== undefined ? `capi:${Number(major)}` : r.build_duckdb_version ?? ''
}

/** A cell (a DuckDB version and a platform) and the version that waits there. */
export interface CellWait {
  duckdb: string
  platform: string
  version: string
}

/** What waits for a stage S of a name from its previous stage P (spec 0043), per cell, as the
 * placement pass finds a rule's waiting version (spec 0041, internal/release/place.go): one of S's
 * DuckDB versions on a platform where P's flat path serves a current (P's rows are those of the
 * source S's rule takes the name from), the cell waiting when that current is
 * - of a version S does not hold (an active release, not withdrawn, on any cell): the pass
 *   completes a held version by itself;
 * - in a slot (its version, platform and slot) S never had, purged slots included;
 * - newer than every version S has or had, in any state (active, deprecated, yanked, retired, withdrawn,
 *   and the releases it purged), on every cell of S its build serves (a C API build's are several),
 *   as the pass's newer() requires.
 * S with no release of the name has had nothing anywhere. */
export function cellWaits(prev: IndexRelease[], stage: IndexRelease[], stageVersions: string[], purged: PurgedRelease[] = []): CellWait[] {
  const out: CellWait[] = []
  const held = new Set(stage.filter((r) => r.state === 'active' && !r.withdrawn).map((r) => r.version))
  const platforms = [...new Set(prev.map((r) => r.platform))].sort()
  for (const duckdb of [...stageVersions].sort()) {
    for (const platform of platforms) {
      const cur = prev.find((r) => r.platform === platform && r.current_for.includes(duckdb))
      if (!cur || held.has(cur.version)) continue
      const slot = slotOf(cur)
      const same = (x: { version: string; platform: string }) => x.version === cur.version && x.platform === platform
      if (stage.some((r) => same(r) && slotOf(r) === slot) || purged.some((p) => same(p) && p.slot === slot)) continue
      const had = (d: string) => [
        ...stage.filter((r) => r.platform === platform &&
          (r.serves.some((s) => s.duckdb_version === d) || (r.would_serve ?? []).includes(d))),
        ...purged.filter((p) => p.platform === platform && p.duckdb_version === d),
      ]
      // newer than what every cell the build serves in the stage has or had (the pass's newer(): a C
      // API build serves several DuckDB versions, and one of them having a newer version is enough)
      const cells = cur.serves.map((s) => s.duckdb_version).filter((d) => stageVersions.includes(d))
      if ([...new Set([duckdb, ...cells])].every((d) => had(d).every((r) => compareVersions(cur.version, r.version) > 0))) {
        out.push({ duckdb, platform, version: cur.version })
      }
    }
  }
  return out
}

/** A stage's wait for a name: the version to promote (the newest of its cells' waiting versions)
 * and the cells waiting, less the versions a skip in the stage covers ((S, source, name), and not
 * newer than the skipped version); null when nothing waits. */
export interface StageWait {
  version: string
  cells: CellWait[]
}

export function stageWait(cells: CellWait[], ds: Decision[], stage: string, source: string, name: string): StageWait | null {
  const left = cells.filter((c) => !skipped(ds, stage, source, name, c.version))
  if (left.length === 0) return null
  const version = left.map((c) => c.version).sort(compareVersions).at(-1)!
  return { version, cells: left }
}
