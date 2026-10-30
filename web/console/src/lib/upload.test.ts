import { afterEach, describe, expect, it } from 'vitest'
import { ApiError, type Client } from './http'
import { type BundleUpload, checkUpload, contentDigest, loadResume, missing, normalSHA, resumeKey, sameFile, saveResume, sleepFor, transient, uploadBundle } from './upload'
import { importLines, importProblem, uploadStatus } from '../screens/BundleUploads'
import { sizeText } from './size'

// Spec 0014 phase 2 in the console: a bundle's chunks, their digests, retries and resuming.

type Call = { method: string; path: string; digest?: string; size?: number }

/** A server for one upload: chunk failures scripted by number, an existing upload to resume. */
function fakeServer(o: { chunkSize: number; existing?: BundleUpload; fail?: Record<number, (number | ApiError)[]>; hang?: number; answer?: Partial<BundleUpload> }) {
  const calls: Call[] = []
  let created: BundleUpload | null = null
  const fail = { ...(o.fail ?? {}) }
  const client = {
    async get<T>(path: string) {
      calls.push({ method: 'GET', path })
      if (o.existing && path.endsWith(`/${o.existing.id}`)) return { status: 200, data: o.existing as T, etag: '' }
      throw new ApiError(404, 'not found')
    },
    async post<T>(path: string, opts: { body?: { size: number } } = {}) {
      calls.push({ method: 'POST', path })
      if (path.endsWith('/complete')) return { status: 202, data: { ...(created ?? o.existing)!, state: 'queued' } as T, etag: '' }
      const size = opts.body!.size
      created = { id: 'u-new', size, chunk_size: o.chunkSize, chunks: Math.ceil(size / o.chunkSize), state: 'uploading', options: {}, attempts: 0,
        created_by: 'x', created_at: '', updated_at: '', ...o.answer }
      return { status: 201, data: created as T, etag: '' }
    },
    async put<T>(path: string, opts: { raw: { body: Blob }; headers: Record<string, string>; signal?: AbortSignal }) {
      const n = Number(path.split('/').pop())
      calls.push({ method: 'PUT', path, digest: opts.headers['Content-Digest'], size: opts.raw.body.size })
      if (n === o.hang) {
        await new Promise((_, reject) => opts.signal?.addEventListener('abort', () => reject(opts.signal?.reason)))
      }
      const f = fail[n]?.shift()
      if (f instanceof ApiError) throw f
      if (f) throw f === -1 ? new TypeError('network') : new ApiError(f, 'no')
      return { status: 204, data: undefined as T, etag: '' }
    },
  }
  return { client: client as unknown as Client, calls }
}

const file = (n: number) => new File([new Uint8Array(n).map((_, i) => i % 251)], 'acme.kbundle', { lastModified: 7 })
const noSleep = async () => {}
const key = resumeKey('/api/v1', 'acme')

afterEach(() => localStorage.clear())

describe('bundle uploads', () => {
  it('lists the chunks missing; reads a pasted SHA-256', () => {
    expect(missing(4, [2, 0])).toEqual([1, 3])
    expect(missing(2)).toEqual([0, 1])
    expect(normalSHA('')).toBe('')
    expect(normalSHA(' ' + 'AB'.repeat(32) + '\n')).toBe('ab'.repeat(32))
    expect(normalSHA('sha256 ' + 'ab'.repeat(32))).toBe('ab'.repeat(32))
    expect(normalSHA('sha256:' + 'ab'.repeat(32))).toBe('ab'.repeat(32))
    expect(normalSHA('abc')).toBeNull()
  })

  it('writes a chunk digest as RFC 9530 does', async () => {
    expect(await contentDigest(new Blob(['abc']))).toBe('sha-256=:ungWv48Bz+pBQUDeXa4iI7ADYaOWF3qctBD/YfIAFa0=:')
  })

  it('sends every chunk with its digest, retries what is transient, then completes', async () => {
    const s = fakeServer({ chunkSize: 1000, fail: { 1: [429, 503, -1] } })
    const progress: number[] = []
    const u = await uploadBundle({ client: s.client, api: '/tenants/acme', file: file(2500), sha256: 'ab'.repeat(32),
      options: { upstreams: ['core'], dry_run: false }, sleep: noSleep, onProgress: (sent) => progress.push(sent) })
    expect(u.state).toBe('queued')
    const puts = s.calls.filter((c) => c.method === 'PUT')
    expect(puts.filter((c) => c.path.endsWith('/chunks/1'))).toHaveLength(4)
    expect(puts.map((c) => c.size).sort()).toEqual([1000, 1000, 1000, 1000, 1000, 500].sort())
    expect(puts.every((c) => c.digest?.startsWith('sha-256=:'))).toBe(true)
    expect(s.calls.at(-1)).toEqual({ method: 'POST', path: '/tenants/acme/bundle-uploads/u-new/complete' })
    expect(progress.at(-1)).toBe(2500)
  })

  it('stops at a refusal, never completing', async () => {
    const s = fakeServer({ chunkSize: 1000, fail: { 0: [409] } })
    await expect(uploadBundle({ client: s.client, api: '/tenants/acme', file: file(2500), sha256: '', options: { upstreams: [], dry_run: false },
      sleep: noSleep, parallel: 1 })).rejects.toMatchObject({ status: 409 })
    expect(s.calls.some((c) => c.path.endsWith('/complete'))).toBe(false)
    expect(transient(new ApiError(400, ''))).toBe(false)
    expect(transient(new ApiError(429, ''))).toBe(true)
  })

  it('resumes an upload with the chunks the server lacks; another size starts anew', async () => {
    const existing: BundleUpload = { id: 'u-old', size: 2500, chunk_size: 1000, chunks: 3, received: [0, 2], state: 'uploading', options: {},
      attempts: 0, created_by: 'x', created_at: '', updated_at: '' }
    const s = fakeServer({ chunkSize: 1000, existing })
    const progress: number[] = []
    await uploadBundle({ client: s.client, api: '/tenants/acme', file: file(2500), sha256: '', options: { upstreams: [], dry_run: false },
      resumeId: 'u-old', sleep: noSleep, onProgress: (sent) => progress.push(sent) })
    expect(s.calls.filter((c) => c.method === 'PUT').map((c) => c.path)).toEqual(['/tenants/acme/bundle-uploads/u-old/chunks/1'])
    expect(progress[0]).toBe(1500)
    const t = fakeServer({ chunkSize: 1000, existing })
    await uploadBundle({ client: t.client, api: '/tenants/acme', file: file(3000), sha256: '', options: { upstreams: [], dry_run: false },
      resumeId: 'u-old', sleep: noSleep })
    expect(t.calls.filter((c) => c.method === 'PUT')).toHaveLength(3)
    expect(t.calls.at(-1)?.path).toBe('/tenants/acme/bundle-uploads/u-new/complete')
  })

  it('keeps an upload to resume per tenant, for the same file only', () => {
    const f = file(10)
    saveResume(key, { id: 'u1', name: f.name, size: f.size, modified: f.lastModified })
    expect(loadResume(key)?.id).toBe('u1')
    expect(loadResume(resumeKey('/api/v1', 'other'))).toBeNull()
    expect(loadResume(resumeKey('https://other.example/api/v1', 'acme'))).toBeNull()
    expect(sameFile(loadResume(key)!, f)).toBe(true)
    expect(sameFile(loadResume(key)!, file(11))).toBe(false)
    localStorage.setItem(key, JSON.stringify({ id: '..', name: 'x', size: 1, modified: 1 }))
    expect(loadResume(key)).toBeNull()
    saveResume(key, null)
    expect(loadResume(key)).toBeNull()
  })

  it('says an upload in words', () => {
    expect(sizeText(512)).toBe('512 B')
    expect(sizeText(16 * 1024 * 1024)).toBe('16 MiB')
    expect(sizeText(1.5 * 1024 ** 3)).toBe('1.5 GiB')
    expect(uploadStatus({ state: 'failed', error: 'sha256', attempts: 1 })).toBe('failed: sha256')
    expect(uploadStatus({ state: 'queued', attempts: 1 })).toBe('queued again (attempt 2)')
    // the import record's counts as the server writes them (internal/upstream/import.go)
    const rec = { counts: { targets: { core: { status: 'imported', counts: { released: 2, unchanged: 0 } }, 'feed:enterest': { status: 'busy', counts: null } },
      unplanned: [], skipped_bytes: 0, problem: '', gaps: 1 } }
    expect(importLines(rec)).toEqual(['core: imported · released 2', 'feed:enterest: busy', 'gaps 1'])
    expect(importProblem(rec)).toBe('')
    expect(importProblem({ counts: { problem: 'bundle: truncated' } })).toBe('bundle: truncated')
    expect(importLines({})).toEqual([])
    // spec 0021: the licences a bundle carried
    const lic = { counts: { licenses: { counts: { received: 2, stored: 1, refused: 1 }, refused: [{ name: 'vault', rule: 'kid_untrusted', reason: 'x' }] } } }
    expect(importLines(lic)).toEqual(['licences: refused 1 · stored 1', 'licence vault: kid_untrusted'])
  })

  it('answers an upload completed before, never uploading it again', async () => {
    for (const state of ['queued', 'importing', 'imported'] as const) {
      const existing: BundleUpload = { id: 'u-old', size: 2500, chunk_size: 1000, chunks: 3, state, options: {}, attempts: 1, created_by: 'x',
        created_at: '', updated_at: '' }
      const s = fakeServer({ chunkSize: 1000, existing })
      const u = await uploadBundle({ client: s.client, api: '/tenants/acme', file: file(2500), sha256: '', options: { upstreams: [], dry_run: false },
        resumeId: 'u-old', sleep: noSleep })
      expect(u.id).toBe('u-old')
      expect(s.calls.map((c) => c.method)).toEqual(['GET'])
    }
  })

  it('refuses a server answer that does not match the file', async () => {
    for (const answer of [{ chunk_size: 0 }, { chunk_size: -1000 }, { chunks: 1e12 }, { size: 1 }, { id: '..' }, { received: [7] }]) {
      const s = fakeServer({ chunkSize: 1000, answer })
      await expect(uploadBundle({ client: s.client, api: '/tenants/acme', file: file(2500), sha256: '', options: { upstreams: [], dry_run: false },
        sleep: noSleep })).rejects.toThrow(/does not match the file/)
      expect(s.calls.some((c) => c.method === 'PUT')).toBe(false)
    }
    expect(checkUpload({ id: 'u1', size: 2500, chunk_size: 1000, chunks: 3, received: [2] } as BundleUpload, 2500).id).toBe('u1')
  })

  it('waits for a busy server as long as it says, beyond the tries for failures', async () => {
    const waits: number[] = []
    const s = fakeServer({ chunkSize: 1000, fail: { 0: Array.from({ length: 8 }, () => new ApiError(429, 'busy', 3)) } })
    await uploadBundle({ client: s.client, api: '/tenants/acme', file: file(1000), sha256: '', options: { upstreams: [], dry_run: false },
      tries: 2, sleep: async (ms) => { waits.push(ms) } })
    expect(waits).toHaveLength(8)
    expect(waits.every((w) => w >= 3000 && w < 3500)).toBe(true)
    const t = fakeServer({ chunkSize: 1000, fail: { 0: [503, 503, 503] } })
    await expect(uploadBundle({ client: t.client, api: '/tenants/acme', file: file(1000), sha256: '', options: { upstreams: [], dry_run: false },
      tries: 2, sleep: noSleep })).rejects.toMatchObject({ status: 503 })
  })

  it('stops at once, a chunk in flight too; never completes', async () => {
    const s = fakeServer({ chunkSize: 1000, hang: 1 })
    const ctl = new AbortController()
    const run = uploadBundle({ client: s.client, api: '/tenants/acme', file: file(2500), sha256: '', options: { upstreams: [], dry_run: false },
      signal: ctl.signal, sleep: noSleep, parallel: 3 })
    await new Promise((r) => setTimeout(r, 20))
    ctl.abort(new Error('stopped'))
    await expect(run).rejects.toThrow('stopped')
    expect(s.calls.some((c) => c.path.endsWith('/complete'))).toBe(false)
  })

  it('ends a wait when stopped', async () => {
    const ctl = new AbortController()
    const w = sleepFor(60_000, ctl.signal)
    ctl.abort(new Error('stopped'))
    await expect(w).rejects.toThrow('stopped')
  })
})
