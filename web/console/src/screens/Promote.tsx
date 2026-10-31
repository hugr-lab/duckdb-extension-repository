import { useState } from 'react'
import { Button, Confirm, ErrorBox, Field, Mono, Note } from '../components/ui'
import { useLoad, useTenant } from '../lib/context'
import { useAction } from '../lib/action'
import { seg } from '../lib/http'

// Promotion (spec 0008): releasing, in another signed channel, a version that is already in this
// one, signed with that channel's key: promote on the name in the target, publish in the source.

export interface Promoting {
  target: string
  version: string
  /** private: only callers granted install (promotion never makes a private build public) */
  narrow: boolean
  /** leave the target's current version as it is */
  notCurrent: boolean
}

/** The request body for POST …/channels/{target}/extensions/{ext}/releases/promote. */
export function promoteBody(from: string, p: Promoting) {
  const out: Record<string, unknown> = { from_channel: from, version: p.version }
  if (p.narrow) out.visibility = 'private'
  if (p.notCurrent) out.current = false
  return out
}

interface ChannelRow {
  name: string
  kind: string
  package_type?: string
}

/** The channels a version of ext may be promoted to from channel: the tenant's other signed
 * channels of the same package type where the caller may promote the name. */
export function promoteTargets(channels: ChannelRow[], from: string, packageType: string | undefined, may: (c: string) => boolean) {
  return channels.filter((c) => c.name !== from && c.kind === 'signed' && (c.package_type ?? '') === (packageType ?? '') && may(c.name)).map((c) => c.name)
}

/** A "Promote…" button and its dialog, on an extension's page in a channel. */
export function PromoteButton({ channel, ext, versions }: { channel: string; ext: string; versions: string[] }) {
  const { client, api, perms } = useTenant()
  const chans = useLoad(() => client.get<{ channels: ChannelRow[] }>(`${api}/channels`), [api])
  const all = chans.data?.data.channels ?? []
  const here = all.find((c) => c.name === channel)
  const targets = promoteTargets(all, channel, here?.package_type, (c) => perms.mayPromote(c, ext))
  const [open, setOpen] = useState(false)
  const [p, setP] = useState<Promoting>({ target: '', version: '', narrow: false, notCurrent: false })
  const [done, setDone] = useState('')
  const { busy, failure, act } = useAction('The promotion')
  if (here?.kind !== 'signed' || targets.length === 0) return chans.error ? <ErrorBox error={chans.error} /> : null
  const ready = p.target !== '' && p.version.trim() !== ''
  const submit = () => {
    if (!ready) return
    const q = { ...p, version: p.version.trim() }
    setOpen(false)
    act(() => client.post(`${api}/channels/${seg(q.target)}/extensions/${seg(ext)}/releases/promote`, { body: promoteBody(channel, q) }))
      .then((ok) => ok && setDone(`${q.version} promoted to ${q.target}`))
  }
  return (
    <>
      <span className="flex items-center gap-2">
        {done && <span className="text-[13px] text-ink-muted" role="status">{done}</span>}
        <Button disabled={busy} onClick={() => {
          setDone('')
          setP({ target: targets[0], version: versions[0] ?? '', narrow: false, notCurrent: false })
          setOpen(true)
        }}>Promote…</Button>
      </span>
      {failure != null && <ErrorBox error={failure} />}
      <Confirm open={open} title={`Promote ${ext} from ${channel}`} confirm="Promote" disabled={!ready} onConfirm={submit} onClose={() => setOpen(false)}>
        <p>Every active release of the version that <Mono>{channel}</Mono> serves is released in the target, every platform and slot,
          signed with its key: all or none.</p>
        <Field label="To channel">
          <select className="field" value={p.target} onChange={(e) => setP({ ...p, target: e.target.value })}>
            {targets.map((c) => <option key={c}>{c}</option>)}
          </select>
        </Field>
        <Field label="Version">
          <input className="field font-mono" list="promote-versions" value={p.version} data-autofocus
            onChange={(e) => setP({ ...p, version: e.target.value })}
            onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); submit() } }} />
        </Field>
        <datalist id="promote-versions">{versions.map((v) => <option key={v} value={v} />)}</datalist>
        <label className="flex items-center gap-2 text-[13px]">
          <input type="checkbox" checked={p.narrow} onChange={(e) => setP({ ...p, narrow: e.target.checked })} /> private: only callers granted install
        </label>
        <label className="flex items-center gap-2 text-[13px]">
          <input type="checkbox" checked={p.notCurrent} onChange={(e) => setP({ ...p, notCurrent: e.target.checked })} /> not current: leave the target's
          current version as it is
        </label>
        <Note>The visibility stays the source's unless private is asked: promotion never makes a private build public. The target must serve
          each build's DuckDB version, and you need publish on the name in this channel.</Note>
      </Confirm>
    </>
  )
}
