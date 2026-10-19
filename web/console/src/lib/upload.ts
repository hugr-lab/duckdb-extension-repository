import { ApiError, type Client, seg } from './http'

// A bundle uploaded in chunks (spec 0014 phase 2): each chunk sent with its RFC 9530
// Content-Digest, a few at once, sent again when the server is busy or the network fails; a broken
// upload resumes with the chunks the server lacks, its id kept in this browser's storage.

export interface BundleUpload {
  id: string
  size: number
  sha256?: string
  chunk_size: number
  chunks: number
  received?: number[]
  state: 'uploading' | 'queued' | 'importing' | 'imported' | 'failed' | 'aborted' | 'expired'
  options: { dry_run?: boolean; upstreams?: string[]; feeds?: string[]; any_tenant?: boolean }
  import_id?: string
  input_sha256?: string
  attempts: number
  error?: string
  created_by: string
  completed_by?: string
  created_at: string
  updated_at: string
  expires_at?: string
}

/** An import target's outcome: an upstream's, or a feed's ("feed:<name>"). */
export interface ImportTarget {
  status: string
  counts?: Record<string, number> | null
}

export interface BundleImport {
  id: string
  manifest: string
  manifest_created_at: string
  bundle_key: string
  input_sha256?: string
  dry_run: boolean
  state: string
  counts?: {
    targets?: Record<string, ImportTarget>
    problem?: string
    gaps?: number
    skipped_bytes?: number
    unplanned?: unknown
    /** the licences the bundle carried (spec 0021) */
    licenses?: { counts?: Record<string, number> | null; refused?: { name?: string; rule: string; reason: string }[]; error?: string }
  }
  started_at: string
  finished_at?: string
  actor: string
}

/** An upload's states that change no more. */
export const finalStates = new Set(['imported', 'failed', 'aborted', 'expired'])

/** The states of an upload completed: its import is (or was) under way. */
export const completedStates = new Set(['queued', 'importing', 'imported', 'failed'])

/** The import's options of a new upload. */
export interface UploadOptions {
  upstreams: string[]
  dry_run: boolean
}

/** The chunk numbers not received yet, in order. */
export function missing(chunks: number, received: number[] = []): number[] {
  const have = new Set(received)
  const out: number[] = []
  for (let n = 0; n < chunks; n++) if (!have.has(n)) out.push(n)
  return out
}

/** Chunk n's bytes of a file: from n × chunkSize. */
export const chunkOf = (file: Blob, chunkSize: number, n: number) => file.slice(n * chunkSize, Math.min(file.size, (n + 1) * chunkSize))

/** A blob's bytes (FileReader where Blob.arrayBuffer is missing: older engines, jsdom). */
function bytesOf(b: Blob): Promise<ArrayBuffer> {
  if (typeof b.arrayBuffer === 'function') return b.arrayBuffer()
  return new Promise((resolve, reject) => {
    const r = new FileReader()
    r.onload = () => resolve(r.result as ArrayBuffer)
    r.onerror = () => reject(r.error)
    r.readAsArrayBuffer(b)
  })
}

/** A chunk's Content-Digest: its SHA-256 as structured-field bytes. */
export async function contentDigest(b: Blob): Promise<string> {
  const h = new Uint8Array(await crypto.subtle.digest('SHA-256', new Uint8Array(await bytesOf(b))))
  let bin = ''
  for (const x of h) bin += String.fromCharCode(x)
  return `sha-256=:${btoa(bin)}:`
}

/** A SHA-256 as the export prints it ("sha256 <hex>", or the hex alone, any case); "" when empty,
 * null when it is none. */
export function normalSHA(s: string): string | null {
  const t = s.trim().toLowerCase().replace(/^(sha-?256)\s*[:=]?\s*/, '')
  if (t === '') return ''
  return /^[0-9a-f]{64}$/.test(t) ? t : null
}

const idRE = /^[A-Za-z0-9_-]{1,64}$/

/** An upload as the server answered it, checked before a byte is sent by it: its id, sizes and
 * chunks agree with the file's (a server's answer never makes the console loop or send other
 * bytes). */
export function checkUpload(u: BundleUpload, size: number): BundleUpload {
  const ok =
    typeof u?.id === 'string' && idRE.test(u.id) && u.size === size && Number.isSafeInteger(u.chunk_size) && u.chunk_size > 0 &&
    u.chunks === Math.ceil(size / u.chunk_size) && (u.received ?? []).every((n) => Number.isSafeInteger(n) && n >= 0 && n < u.chunks)
  if (!ok) throw new Error('kista console: the server answered an upload that does not match the file')
  return u
}

/** What resumes an upload: its id and the file it came from. */
export interface Resume {
  id: string
  name: string
  size: number
  modified: number
}

/** The storage key of a tenant's upload, per API base: two kista servers in one shell keep theirs apart. */
export const resumeKey = (base: string, tenant: string) => `kista.bundle-upload.${base}.${tenant}`

/** The upload this browser kept for a key, if any. */
export function loadResume(key: string): Resume | null {
  try {
    const r = JSON.parse(localStorage.getItem(key) ?? 'null')
    return r && typeof r.id === 'string' && idRE.test(r.id) && typeof r.name === 'string' && Number.isSafeInteger(r.size) &&
      typeof r.modified === 'number'
      ? { id: r.id, name: r.name, size: r.size, modified: r.modified }
      : null
  } catch {
    return null
  }
}

export function saveResume(key: string, r: Resume | null) {
  try {
    if (r) localStorage.setItem(key, JSON.stringify(r))
    else localStorage.removeItem(key)
  } catch {
    // no storage: the upload simply does not resume after a reload
  }
}

/** Whether a picked file is the one an upload came from. */
export const sameFile = (r: Resume, f: File) => r.name === f.name && r.size === f.size && r.modified === f.lastModified

export interface UploadRun {
  client: Client
  /** The tenant's API path, "/tenants/acme". */
  api: string
  file: File
  sha256: string
  options: UploadOptions
  /** An upload to resume: taken while it is uploading the same size with the same SHA-256 (if
   * one is given); one already completed is answered as it is, never uploaded again. */
  resumeId?: string
  /** Chunks at once (the server takes at most 4 of an upload). */
  parallel?: number
  /** Tries per chunk for a network failure or a server error; a busy server is waited for while
   * the upload runs. */
  tries?: number
  /** The least rate a chunk is sent at before it is sent again (bytes per second). */
  minRate?: number
  signal?: AbortSignal
  onCreated?: (u: BundleUpload) => void
  onProgress?: (sent: number, total: number) => void
  sleep?: (ms: number, signal: AbortSignal) => Promise<void>
}

/** A wait that ends early when signal does. */
export function sleepFor(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) return reject(signal.reason)
    const t = setTimeout(() => {
      signal.removeEventListener('abort', stop)
      resolve()
    }, ms)
    const stop = () => {
      clearTimeout(t)
      reject(signal.reason)
    }
    signal.addEventListener('abort', stop, { once: true })
  })
}

/** A failure worth sending a chunk again for: the server busy or failing, a chunk it could not
 * read whole (a slow link), the network gone, a chunk timed out. */
export function transient(e: unknown) {
  if (e instanceof ApiError) return e.status === 429 || e.status === 408 || e.status >= 500 || (e.status === 400 && /could not be read whole/.test(e.message))
  if (e instanceof DOMException) return e.name === 'TimeoutError'
  return e instanceof TypeError // fetch's network failure
}

/** Uploads a file as a bundle (or resumes an upload of it) and completes it; it answers the
 * completed upload. */
export async function uploadBundle(o: UploadRun): Promise<BundleUpload> {
  const base = `${o.api}/bundle-uploads`
  const sleep = o.sleep ?? sleepFor
  let u: BundleUpload | null = null
  if (o.resumeId) {
    try {
      const r = (await o.client.get<BundleUpload>(`${base}/${seg(o.resumeId)}`, { signal: o.signal })).data
      if (r.size === o.file.size && (!o.sha256 || !r.sha256 || r.sha256 === o.sha256)) {
        if (completedStates.has(r.state) && r.state !== 'failed') return r // completed before: never uploaded twice
        if (r.state === 'uploading') u = checkUpload(r, o.file.size)
      }
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 404)) throw e
    }
  }
  if (!u) {
    const body: Record<string, unknown> = { size: o.file.size }
    if (o.sha256) body.sha256 = o.sha256
    if (o.options.upstreams.length) body.upstreams = o.options.upstreams
    if (o.options.dry_run) body.dry_run = true
    u = checkUpload((await o.client.post<BundleUpload>(base, { body, signal: o.signal })).data, o.file.size)
  }
  const up = u
  o.onCreated?.(up)
  const todo = missing(up.chunks, up.received)
  const size = (n: number) => Math.min(o.file.size, (n + 1) * up.chunk_size) - n * up.chunk_size
  let sent = o.file.size - todo.reduce((s, n) => s + size(n), 0)
  o.onProgress?.(sent, o.file.size)
  // one signal for the pool: the caller's stop, or the first chunk that fails for good
  const pool = new AbortController()
  const onStop = () => pool.abort(o.signal?.reason)
  o.signal?.addEventListener('abort', onStop, { once: true })
  if (o.signal?.aborted) onStop()
  const minRate = o.minRate ?? 16 * 1024
  const send = async (n: number) => {
    const b = chunkOf(o.file, up.chunk_size, n)
    const digest = await contentDigest(b)
    const tries = o.tries ?? 6
    for (let i = 1; ; i++) {
      pool.signal.throwIfAborted()
      const timeout = AbortSignal.timeout(60_000 + (b.size * 1000) / minRate)
      try {
        await o.client.put(`${base}/${seg(up.id)}/chunks/${n}`, {
          raw: { body: b, type: 'application/octet-stream' }, headers: { 'Content-Digest': digest },
          signal: AbortSignal.any([pool.signal, timeout]),
        })
        return
      } catch (e) {
        pool.signal.throwIfAborted()
        const err = timeout.aborted ? new DOMException('a chunk timed out', 'TimeoutError') : e
        const busy = err instanceof ApiError && err.status === 429
        if (!transient(err) || (!busy && i >= tries)) throw err
        const after = busy && err instanceof ApiError && err.retryAfter ? err.retryAfter * 1000 : Math.min(15_000, 500 * 2 ** Math.min(i, 5))
        await sleep(after + Math.random() * 500, pool.signal)
      }
    }
  }
  let next = 0
  let failed: unknown = null
  const worker = async () => {
    while (failed === null && next < todo.length) {
      const n = todo[next++]
      try {
        await send(n)
      } catch (e) {
        if (failed === null) {
          failed = e
          pool.abort(e)
        }
        return
      }
      sent += size(n)
      o.onProgress?.(sent, o.file.size)
    }
  }
  try {
    await Promise.all(Array.from({ length: Math.max(1, Math.min(o.parallel ?? 3, 4, todo.length)) }, worker))
  } finally {
    o.signal?.removeEventListener('abort', onStop)
  }
  if (failed !== null) throw o.signal?.aborted ? o.signal.reason : failed
  o.signal?.throwIfAborted()
  return (await o.client.post<BundleUpload>(`${base}/${seg(up.id)}/complete`)).data
}
