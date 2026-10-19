import { useEffect, useRef, useState } from 'react'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, Table, when } from '../components/ui'
import { useAction } from '../lib/action'
import { useLoad, useTenant } from '../lib/context'
import { ApiError, seg } from '../lib/http'
import { entryNames } from '../lib/names'
import {
  type BundleImport, type BundleUpload, completedStates, finalStates, loadResume, normalSHA, type Resume, resumeKey, sameFile, saveResume, uploadBundle,
} from '../lib/upload'

// Uploading a bundle (spec 0014 phase 2), on a tenant's Upstreams page in the server
// administrator's scope: a file sent in chunks, the import a serving replica makes of it, and the
// tenant's recent uploads.

/** A size in words: 1.5 GiB, 16 MiB, 512 B. */
export function sizeText(n: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return i === 0 ? `${n} B` : `${v >= 10 ? Math.round(v) : Math.round(v * 10) / 10} ${units[i]}`
}

/** An upload's state in words, with why it failed. */
export function uploadStatus(u: Pick<BundleUpload, 'state' | 'error' | 'attempts'>): string {
  switch (u.state) {
    case 'uploading':
      return 'waiting for chunks'
    case 'queued':
      return u.attempts > 0 ? `queued again (attempt ${u.attempts + 1})` : 'queued for import'
    case 'failed':
      return `failed: ${u.error ?? 'unknown'}`
    default:
      return u.state
  }
}

/** An import's outcome, a line per target: "core: imported · released 2", "feed:enterest: busy". */
export function importLines(i?: Pick<BundleImport, 'counts'>): string[] {
  const out = Object.entries(i?.counts?.targets ?? {}).map(([name, t]) => {
    const c = Object.entries(t?.counts ?? {}).filter(([, n]) => typeof n === 'number' && n > 0).map(([k, n]) => `${k} ${n}`)
    return `${name}: ${t?.status ?? '?'}${c.length ? ` · ${c.join(' · ')}` : ''}`
  })
  if (i?.counts?.gaps) out.push(`gaps ${i.counts.gaps}`)
  const lic = i?.counts?.licenses
  if (lic) {
    const c = Object.entries(lic.counts ?? {}).filter(([k, n]) => k !== 'received' && n > 0).sort(([a], [b]) => a.localeCompare(b)).map(([k, n]) => `${k} ${n}`)
    out.push(`licences: ${c.length ? c.join(' · ') : 'none'}`)
    for (const r of lic.refused ?? []) out.push(`licence ${r.name ?? '?'}: ${r.rule}`)
    if (lic.error) out.push(`licences not stored: ${lic.error}`)
  }
  return out
}

/** Why an import stopped, if it did. */
export const importProblem = (i?: Pick<BundleImport, 'counts'>) => i?.counts?.problem || ''

export function BundleUploads() {
  const { client, api, tenant } = useTenant()
  const base = `${api}/bundle-uploads`
  const key = resumeKey(client.base, tenant)
  const [file, setFile] = useState<File | null>(null)
  const [sha, setSha] = useState('')
  const [upstreams, setUpstreams] = useState('')
  const [dry, setDry] = useState(false)
  const [resume, setResume] = useState<Resume | null>(() => loadResume(key))
  // the kept upload as the server has it: what a resume sends with
  const [kept, setKept] = useState<BundleUpload | null>(null)
  const [progress, setProgress] = useState<[number, number] | null>(null)
  const [stopped, setStopped] = useState(false)
  const [current, setCurrent] = useState<BundleUpload | null>(null)
  const [imp, setImp] = useState<BundleImport | null>(null)
  const [followErr, setFollowErr] = useState<unknown>(null)
  const [aborting, setAborting] = useState('')
  const stop = useRef<AbortController | null>(null)
  const { busy, failure, act, setFailure } = useAction()
  const list = useLoad(() => client.get<{ uploads: BundleUpload[] }>(base), [base])
  const shaOk = normalSHA(sha)
  const resuming = resume !== null && kept !== null && file !== null && sameFile(resume, file)

  const forget = () => {
    saveResume(key, null)
    setResume(null)
    setKept(null)
  }
  // the kept upload is checked with the server: one completed meanwhile is followed, one gone forgotten
  useEffect(() => {
    if (!resume) return
    let alive = true
    client.get<BundleUpload>(`${base}/${seg(resume.id)}`).then(
      (r) => {
        if (!alive) return
        if (r.data.state === 'uploading') return setKept(r.data)
        forget()
        if (completedStates.has(r.data.state)) setCurrent(r.data)
      },
      (e) => alive && e instanceof ApiError && e.status === 404 && forget(),
    )
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [resume?.id, base])
  // leaving the page stops sending: the upload resumes when the file is picked again
  useEffect(() => () => stop.current?.abort(), [])

  // the upload completed is followed until its import ends; a lasting refusal ends that
  useEffect(() => {
    if (!current || current.state === 'uploading' || finalStates.has(current.state)) return
    let alive = true
    const t = setTimeout(async () => {
      try {
        const r = await client.get<BundleUpload>(`${base}/${seg(current.id)}`)
        if (alive) setCurrent(r.data)
      } catch (e) {
        if (!alive) return
        if (e instanceof ApiError && e.status < 500 && e.status !== 429 && e.status !== 408) setFollowErr(e)
        else setCurrent({ ...current }) // looked at again
      }
    }, 2000)
    return () => {
      alive = false
      clearTimeout(t)
    }
  }, [current, client, base])
  useEffect(() => {
    if (!current || !finalStates.has(current.state)) return
    list.reload()
    if (!current.import_id) return
    client.get<BundleImport>(`${api}/bundle-imports/${seg(current.import_id)}`).then((r) => setImp(r.data), (e) => setFollowErr(e))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [current?.id, current?.state, current?.import_id])

  const follow = (u: BundleUpload) => {
    setImp(null)
    setFollowErr(null)
    setCurrent(u)
  }
  const start = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!file || shaOk === null) return
    const ctl = new AbortController()
    stop.current = ctl
    setImp(null)
    setCurrent(null)
    setFollowErr(null)
    setProgress(null)
    setStopped(false)
    const f = file
    await act(async () => {
      try {
        const u = await uploadBundle({
          client, api, file: f, sha256: shaOk, options: { upstreams: entryNames(upstreams), dry_run: dry },
          resumeId: resuming ? resume!.id : undefined, signal: ctl.signal,
          onCreated: (u) => {
            const r = { id: u.id, name: f.name, size: f.size, modified: f.lastModified }
            saveResume(key, r)
            setResume(r)
            setKept(u)
            list.reload()
          },
          onProgress: (s, t) => setProgress([s, t]),
        })
        forget()
        follow(u)
      } catch (err) {
        if (ctl.signal.aborted) {
          setStopped(true)
          return
        }
        throw err
      } finally {
        stop.current = null
        list.reload()
      }
    })
  }
  const abort = async (id: string) => {
    setAborting('')
    let gone = false
    await act(async () => {
      try {
        await client.del(`${base}/${seg(id)}`)
        gone = true
      } catch (err) {
        // gone already, or past aborting (expired, imported): nothing to resume either way
        if (!(err instanceof ApiError && (err.status === 404 || err.status === 409))) throw err
        gone = true
        if (resume?.id !== id) throw err
      }
    })
    if (gone && resume?.id === id) forget()
    list.reload()
  }
  const again = (id: string) =>
    act(async () => {
      follow((await client.post<BundleUpload>(`${base}/${seg(id)}/complete`)).data)
      list.reload()
    })
  const pct = progress ? Math.floor((progress[0] * 100) / Math.max(1, progress[1])) : 0
  const progressText = progress ? `${pct}% · ${sizeText(progress[0])} of ${sizeText(progress[1])}` : ''
  const replacing = resume !== null && kept !== null && file !== null && !sameFile(resume, file)
  return (
    <Card title="Upload a bundle">
      <div className="flex flex-col gap-4">
        <Note>
          A bundle exported for this tenant, sent in chunks and imported by the server as <Mono>kista admin bundle import</Mono> does, with the
          bundle keys pinned for the tenant. An upload broken off resumes in this browser: pick the same file again.
        </Note>
        {resume && kept && !busy && (
          <div className="flex flex-wrap items-center gap-3 rounded border border-line px-3 py-2 text-[13px]">
            <span>
              An upload of <Mono>{resume.name}</Mono> ({sizeText(resume.size)}, {kept.received?.length ?? 0} of {kept.chunks} chunks received) did not
              finish{resuming ? ': it resumes with the chunks missing.' : '. Pick the same file to resume it.'}
            </span>
            <Button tone="danger-outline" onClick={() => setAborting(resume.id)}>Abort it…</Button>
          </div>
        )}
        <form className="flex flex-col gap-4" onSubmit={start}>
          <div className="grid grid-cols-2 gap-4">
            <Field label="Bundle file" hint={replacing ? 'another file: the earlier upload stays until aborted or expired, and counts against the tenant’s uploads' : undefined}>
              <input className="field" type="file" required disabled={busy} onChange={(e) => { setFile(e.target.files?.[0] ?? null); setFailure(null); setStopped(false) }} />
            </Field>
            <Field label="SHA-256" hint={resuming ? 'the upload’s own, given when it started' : shaOk === null ? 'not a SHA-256: the 64 hex digits the export printed' : 'optional: the one the export printed; a bundle that differs is not imported'}>
              <input className="field font-mono" aria-invalid={!resuming && shaOk === null} value={resuming ? kept.sha256 ?? '' : sha} disabled={busy || resuming}
                onChange={(e) => setSha(e.target.value)} />
            </Field>
            <Field label="Upstreams" hint="names separated by commas; empty: every upstream the bundle targets">
              <input className="field font-mono" value={resuming ? (kept.options.upstreams ?? []).join(', ') : upstreams} disabled={busy || resuming}
                onChange={(e) => setUpstreams(e.target.value)} />
            </Field>
            <label className="flex items-center gap-2 pt-6 text-[13px]">
              <input type="checkbox" checked={resuming ? !!kept.options.dry_run : dry} disabled={busy || resuming} onChange={(e) => setDry(e.target.checked)} />
              dry run: check what it brings, release nothing
            </label>
          </div>
          <div className="flex items-center gap-3">
            <Button type="submit" tone="brand" disabled={busy || !file || (!resuming && shaOk === null)}>{resuming ? 'Resume upload' : 'Upload'}</Button>
            {busy && <Button tone="outline" onClick={() => stop.current?.abort()}>Stop</Button>}
            {file && !busy && <span className="text-[13px] text-ink-muted">{sizeText(file.size)}</span>}
          </div>
        </form>
        {progress && (busy || current || stopped) && (
          <progress aria-label="upload progress" aria-valuetext={progressText} className="h-2 w-80" value={progress[0]} max={progress[1]} />
        )}
        <div role="status" aria-live="polite" className="flex flex-col gap-1 text-[13px]">
          {progress && (busy || current || stopped) && <span>{progressText}</span>}
          {stopped && <span>Stopped. <strong>Resume upload</strong> sends the chunks missing.</span>}
          {current && <span>Upload <Mono>{current.id}</Mono>: <strong>{uploadStatus(current)}</strong></span>}
          {imp && (
            <>
              <span>Import <Mono>{imp.id}</Mono>: {imp.state}{imp.dry_run ? ' (dry run)' : ''}</span>
              {importLines(imp).map((l) => <span key={l} className="font-mono">{l}</span>)}
              {importProblem(imp) && <span>Stopped by: {importProblem(imp)}</span>}
            </>
          )}
        </div>
        {failure != null && <ErrorBox error={failure} />}
        {followErr != null && <ErrorBox error={followErr} />}
        <h3 className="m-0 text-[13px] font-semibold">Recent uploads</h3>
        {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : (list.data?.data.uploads ?? []).length === 0 ? <Empty>No uploads.</Empty> : (
          <Table rows={list.data!.data.uploads} rowKey={(u) => u.id} columns={[
            ['Upload', (u) => <Mono>{u.id}</Mono>],
            ['Size', (u) => sizeText(u.size)],
            ['State', (u) => <span className="flex flex-col"><Badge>{u.state}</Badge>{['uploading', 'queued', 'failed'].includes(u.state) && <span className="text-[13px] text-ink-muted">{uploadStatus(u)}</span>}</span>],
            ['Created', (u) => <span className="text-[13px]">{when(u.created_at)}<div className="text-ink-muted">{u.created_by}</div></span>],
            ['Expires', (u) => <span className="text-[13px] text-ink-muted">{u.expires_at ? when(u.expires_at) : ''}</span>],
            ['', (u) => (
              <span className="flex gap-2">
                {u.state === 'failed' && <Button tone="outline" disabled={busy} onClick={() => again(u.id)}>Import again</Button>}
                {['uploading', 'queued', 'failed'].includes(u.state) && <Button tone="danger-outline" disabled={busy} onClick={() => setAborting(u.id)}>Abort…</Button>}
              </span>
            )],
          ]} />
        )}
      </div>
      <Confirm open={aborting !== ''} title="Abort this upload?" confirm="Abort" danger onConfirm={() => abort(aborting)} onClose={() => setAborting('')}>
        <p>It is not imported (again); what an interrupted import of it already released stays. Its chunks are deleted by the collector.</p>
      </Confirm>
    </Card>
  )
}
