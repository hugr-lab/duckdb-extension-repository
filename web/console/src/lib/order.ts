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

/** The edges of a chain of channels. */
export const chainEdges = (chain: string[]): OrderEdge[] => chain.slice(1).map((next, i) => ({ channel: chain[i], next }))

/** A release of a name in a channel as the channel's index answers it (GET …/channels/{c}/extensions/{name}):
 * the DuckDB versions its versioned path serves, those whose flat path it is current for, and for a
 * yanked or withdrawn one those it would serve. */
export interface IndexRelease {
  version: string
  platform: string
  state: string
  serves: { duckdb_version: string }[]
  current_for: string[]
  would_serve?: string[]
  body_hash?: string
}

/** A cell (a DuckDB version and a platform) and the version that waits there. */
export interface CellWait {
  duckdb: string
  platform: string
  version: string
}

/** What waits for a stage S of a name from its previous stage P (spec 0043), per cell: one of S's
 * DuckDB versions on a platform where P's flat path serves a current (P's rows are those of the
 * source S's rule takes the name from), the cell waiting when that current is newer than every
 * version S's rows have or had there (in any state: active, deprecated, yanked, withdrawn). S with
 * no release of the name has had nothing anywhere. The console reads no purged slot: a version S
 * purged is not seen as had. */
export function cellWaits(prev: IndexRelease[], stage: IndexRelease[], stageVersions: string[]): CellWait[] {
  const out: CellWait[] = []
  const platforms = [...new Set(prev.map((r) => r.platform))].sort()
  for (const duckdb of [...stageVersions].sort()) {
    for (const platform of platforms) {
      const cur = prev.find((r) => r.platform === platform && r.current_for.includes(duckdb))
      if (!cur) continue
      const had = stage.filter((r) => r.platform === platform &&
        (r.serves.some((s) => s.duckdb_version === duckdb) || (r.would_serve ?? []).includes(duckdb)))
      if (had.every((r) => compareVersions(cur.version, r.version) > 0)) out.push({ duckdb, platform, version: cur.version })
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
