import { useRef, useState } from 'react'
import { Button, Confirm, ErrorBox, Field, Mono, Note } from '../components/ui'
import { useTenant } from '../lib/context'
import { useAction } from '../lib/action'
import { type Footer, gzipped, nameFromFile, readFooter } from '../lib/footer'
import { seg } from '../lib/http'

// Adding a build to the catalogue (spec 0033): POST …/catalogue/extensions/{ext}/builds?version=
// &platform=[&visibility=] with the file as the body, by a caller with publish tenant-wide. The
// build lands as the tenant's own (source tenant); channels take it by their rules, or a channel
// administrator places it. The footer is read here only to fill the form: kista reads it again.

export interface Adding {
  name: string
  version: string
  platform: string
  visibility: '' | 'public' | 'private'
}

/** The upload's path and query for a build to add. */
export function addBuildPath(api: string, a: Adding) {
  const q = new URLSearchParams({ version: a.version.trim(), platform: a.platform.trim() })
  if (a.visibility) q.set('visibility', a.visibility)
  return `${api}/catalogue/extensions/${seg(a.name.trim())}/builds?${q}`
}

/** The form's fields from a chosen file: the name from its file name, the version and platform
 * from its footer; each empty when the file does not tell (nothing is kept from an earlier file). */
export function fromFile(file: string, f: Footer | null, was: Adding): Adding {
  return { ...was, name: nameFromFile(file) ?? '', version: f?.version ?? '', platform: f?.platform ?? '' }
}

/** Whether a platform is DuckDB-Wasm's: kista refuses those builds (their signature is a custom
 * section, spec 0008). */
export function wasmPlatform(p: string) {
  return p.trim().startsWith('wasm')
}

/** What a chosen file is: compressed, or an extension's file with its footer (null: none). */
type Read = { gzip: true } | { gzip: false; footer: Footer | null }

const empty: Adding = { name: '', version: '', platform: '', visibility: '' }

/** An "Add a build…" button and its dialog, on the catalogue's page. */
export function AddBuildButton({ onAdded }: { onAdded: () => void }) {
  const { client, api, perms } = useTenant()
  const [open, setOpen] = useState(false)
  const [opened, setOpened] = useState(0) // a fresh file input for each opening
  const [file, setFile] = useState<File | null>(null)
  const [read, setRead] = useState<Read | undefined>(undefined)
  const [a, setA] = useState<Adding>(empty)
  const [done, setDone] = useState('')
  const latest = useRef(0) // the last choice: a read of an earlier file, still running, is dropped
  const { busy, failure, act } = useAction('The build')
  if (!perms.mayPublishCatalogue) return null
  const gzip = read?.gzip === true
  const footer = read && !read.gzip ? read.footer : undefined
  const wasm = wasmPlatform(a.platform)
  const ready = file !== null && read !== undefined && !gzip && !wasm && a.name.trim() !== '' && a.version.trim() !== '' && a.platform.trim() !== ''
  const choose = async (f: File | null) => {
    const n = ++latest.current
    setFile(f)
    setRead(undefined)
    setA((was) => ({ ...empty, visibility: was.visibility }))
    if (!f) return
    let r: Read
    try {
      r = (await gzipped(f)) ? { gzip: true } : { gzip: false, footer: await readFooter(f) }
    } catch {
      r = { gzip: false, footer: null } // an unreadable file: no footer, as one that is not an extension's
    }
    if (n !== latest.current) return
    setRead(r)
    setA((was) => fromFile(f.name, r.gzip ? null : r.footer, was))
  }
  const submit = () => {
    if (!ready) return
    const add = a
    const body = file!
    setOpen(false)
    act(async () => {
      const r = await client.post(addBuildPath(api, add), { raw: { body, type: 'application/octet-stream' } })
      const what = `${add.name} ${add.version} (${add.platform})`
      setDone(r.status === 200 ? `${what} is in the catalogue already` : `${what} added`)
    }).then((ok) => ok && onAdded())
  }
  return (
    <>
      <span className="flex items-center gap-2">
        {done && <span className="text-[13px] text-ink-muted" role="status">{done}</span>}
        <Button disabled={busy} onClick={() => {
          latest.current++
          setDone('')
          setFile(null)
          setRead(undefined)
          setA(empty)
          setOpened((o) => o + 1)
          setOpen(true)
        }}>{busy ? 'Adding…' : 'Add a build…'}</Button>
      </span>
      {failure != null && <ErrorBox error={failure} />}
      <Confirm open={open} title="Add a build to the catalogue" confirm="Add" disabled={!ready} onConfirm={submit} onClose={() => setOpen(false)}>
        <p>The build lands in the catalogue as the tenant's own (source <Mono>tenant</Mono>): channels take it by their rules, or a channel
          administrator places it.</p>
        <Field label="Extension file" hint="a .duckdb_extension file; its footer fills the version and platform">
          <input key={opened} type="file" accept=".duckdb_extension" className="field" onChange={(e) => choose(e.target.files?.[0] ?? null)} />
        </Field>
        {gzip && <p className="text-[13px] text-warning" role="alert">The file is compressed (gzip): upload the uncompressed extension.</p>}
        {footer === null && <p className="text-[13px] text-warning">Its footer is not an extension's: kista will refuse it unless the fields are right.</p>}
        {footer && <p className="text-[13px] text-ink-muted">Footer: <Mono>{footer.platform}</Mono>, {footer.abi} {footer.duckdb}</p>}
        <Field label="Name"><input className="field font-mono" value={a.name} onChange={(e) => setA({ ...a, name: e.target.value })} /></Field>
        <Field label="Version"><input className="field font-mono" value={a.version} onChange={(e) => setA({ ...a, version: e.target.value })} /></Field>
        <Field label="Platform"><input className="field font-mono" value={a.platform} onChange={(e) => setA({ ...a, platform: e.target.value })} /></Field>
        {wasm && <p className="text-[13px] text-warning" role="alert">kista does not take DuckDB-Wasm builds (wasm_* platforms).</p>}
        <Field label="Visibility">
          <select className="field" value={a.visibility} onChange={(e) => setA({ ...a, visibility: e.target.value as Adding['visibility'] })}>
            <option value="">private (only callers granted install)</option>
            <option value="public">public: anyone may install it</option>
          </select>
        </Field>
        <Note>It needs publish on the name tenant-wide. A name DuckDB reserves needs a grant naming it.</Note>
      </Confirm>
    </>
  )
}
