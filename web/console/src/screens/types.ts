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
  shadows?: string
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
