export interface Release {
  id: string
  name: string
  version: string
  platform: string
  slot: string
  abi: string
  build_duckdb_version?: string
  build_c_api?: string
  state: string
  visibility: string
  seq: number
  body_hash: string
  created_at: string
  created_by?: string
  state_changed_at?: string
  state_changed_by?: string
  origin: string
  provenance?: unknown
  /** a publisher's signed request, as it came (spec 0023): the statement, and the SSH signature in base64 */
  publisher_statement?: string
  publisher_signature?: string
  shadows?: string
  /** whose release it is, and whether it is the channel's source of the name (spec 0031): only those are served */
  source: string
  selected: boolean
  etag: string
}

export interface Key {
  id: string
  fingerprint: string
  state: string
  trusted_since?: string
  state_changed_at: string
  state_changed_by: string
  created_at: string
  created_by: string
  etag: string
}

export interface KeyView {
  keys: Key[]
  serving_key?: string
  active_key?: string
  unsigned_by_active?: number
  resigner?: { working: boolean; last_held_at: string; holder?: string }
}

export interface Event {
  id: string
  at: string
  kind: string
  outcome: string
  actor: string
  actor_name?: string
  subject: string
  data: unknown
  client?: string
  request: string
}

/** A document about a body (spec 0011), as the management API shows it. */
export interface Attachment {
  id: string
  body_hash: string
  kind: string
  media_type: string
  title: string
  size: number
  summary: Record<string, unknown>
  origin: string
  issuer?: string // a feed's document: its source, whose pinned key signed it
  source_origin?: string
  key?: string
  audience?: string // a feed's document: who its source lets subscribers show it to
  issued_at: string
  valid_until?: string
  expired?: boolean
  attached_by?: string
  supersedes?: string
  head?: boolean
  visibility?: string
  feedable?: boolean
  withdrawn?: boolean
  etag?: string
}
