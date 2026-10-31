import { useState } from 'react'
import { Button, Confirm, ErrorBox, Field, Mono, Note } from '../components/ui'
import { useTenant } from '../lib/context'
import { useAction } from '../lib/action'
import { type Footer, nameFromFile, readFooter } from '../lib/footer'
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

/** The form's fields from a chosen file: the name from its file name, the rest from its footer. */
export function fromFile(file: string, f: Footer | null, was: Adding): Adding {
  return { ...was, name: nameFromFile(file) ?? was.name, version: f?.version ?? was.version, platform: f?.platform ?? was.platform }
}

/** An "Add a build…" button and its dialog, on the catalogue's page. */
export function AddBuildButton({ onAdded }: { onAdded: () => void }) {
  const { client, api, perms } = useTenant()
  const [open, setOpen] = useState(false)
  const [file, setFile] = useState<File | null>(null)
  const [footer, setFooter] = useState<Footer | null | undefined>(undefined)
  const [a, setA] = useState<Adding>({ name: '', version: '', platform: '', visibility: '' })
  const [done, setDone] = useState('')
  const { busy, failure, act } = useAction('The build')
  if (!perms.mayPublishCatalogue) return null
  const ready = file !== null && a.name.trim() !== '' && a.version.trim() !== '' && a.platform.trim() !== ''
  const choose = async (f: File | null) => {
    setFile(f)
    if (!f) return setFooter(undefined)
    const ft = await readFooter(f)
    setFooter(ft)
    setA((was) => fromFile(f.name, ft, was))
  }
  const submit = () => {
    if (!ready) return
    const add = a
    setOpen(false)
    act(() => client.post(addBuildPath(api, add), { raw: { body: file!, type: 'application/octet-stream' } })).then((ok) => {
      if (ok) {
        setDone(`${add.name} ${add.version} (${add.platform}) added`)
        onAdded()
      }
    })
  }
  return (
    <>
      <span className="flex items-center gap-2">
        {done && <span className="text-[13px] text-ink-muted" role="status">{done}</span>}
        <Button disabled={busy} onClick={() => {
          setDone('')
          setFile(null)
          setFooter(undefined)
          setA({ name: '', version: '', platform: '', visibility: '' })
          setOpen(true)
        }}>{busy ? 'Adding…' : 'Add a build…'}</Button>
      </span>
      {failure != null && <ErrorBox error={failure} />}
      <Confirm open={open} title="Add a build to the catalogue" confirm="Add" disabled={!ready} onConfirm={submit} onClose={() => setOpen(false)}>
        <p>The build lands in the catalogue as the tenant's own (source <Mono>tenant</Mono>): channels take it by their rules, or a channel
          administrator places it.</p>
        <Field label="Extension file" hint="a .duckdb_extension file; its footer fills the version and platform">
          <input type="file" className="field" onChange={(e) => choose(e.target.files?.[0] ?? null)} />
        </Field>
        {footer === null && <p className="text-[13px] text-warning">Its footer is not an extension's: kista will refuse it unless the fields are right.</p>}
        {footer && <p className="text-[13px] text-ink-muted">Footer: <Mono>{footer.platform}</Mono>, {footer.abi} {footer.duckdb}</p>}
        <Field label="Name"><input className="field font-mono" value={a.name} onChange={(e) => setA({ ...a, name: e.target.value })} /></Field>
        <Field label="Version"><input className="field font-mono" value={a.version} onChange={(e) => setA({ ...a, version: e.target.value })} /></Field>
        <Field label="Platform"><input className="field font-mono" value={a.platform} onChange={(e) => setA({ ...a, platform: e.target.value })} /></Field>
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
