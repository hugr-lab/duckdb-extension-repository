import { useId, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, Table, when } from '../components/ui'
import { useAction } from '../lib/action'
import { useLoad, useTenant } from '../lib/context'
import { seg } from '../lib/http'
import type { Client } from '../lib/http'
import type { Upstream } from './Upstreams'

// Licences (spec 0012 phase 1d): the tenant's licences, imported by its administrators or relayed
// from its credentialed upstreams. kista issues none: it stores what verifies against a key the
// extension's live builds declare (spec 0021: their key blocks, recorded from a marked publisher or
// a repository upstream) and serves it to the extensions' SDK. A licence belongs to a catalogue
// entry (spec 0035): a source's builds of the name, which every write names.

export interface License {
  name: string
  /** The licence's catalogue entry, by its source (spec 0035): upstream:<name>, publisher:<name>, or
   * a removed source's id form. */
  entry: string
  id: string
  issuer: string
  holder_id: string
  holder?: string
  plan: string
  versions?: { min?: string; below?: string }
  limits?: { name: string; value: boolean | number | string }[]
  issued_at: string
  expires_at: string
  grace_until: string
  key: string
  source: 'import' | 'upstream'
  upstream?: string
  visibility: 'public' | 'private'
  status: string
  etag: string
}

/** An entry a key is live for (spec 0035): its source, by kind and name (a distributor's publisher's
 * with its distributor), and how many of its builds declare the key. */
export interface KeySource {
  kind: 'publisher' | 'upstream'
  name: string
  distributor?: string
  builds: number
}

/** The licence keys of a name's live builds, each with the entries it is live for: what an import
 * into each accepts (spec 0021, 0035). */
export interface NameKeys {
  name: string
  keys: { kid: string; sources: KeySource[] }[]
}

/** A licence's key in the list: a name has a licence per entry (spec 0035). */
export function licenseKey(l: Pick<License, 'name' | 'entry'>) {
  return `${l.name} ${l.entry}`
}

/** A licence's route, by its entry (spec 0035): the licence, or one of its actions (public,
 * private), the entry in the query after the path. */
export function licensePath(api: string, l: Pick<License, 'name' | 'entry'>, action?: 'public' | 'private') {
  return `${api}/licenses/${seg(l.name)}${action ? `/${action}` : ''}?source=${encodeURIComponent(l.entry)}`
}

/** An import's route: into the entry it names (spec 0035, required). */
export function importPath(api: string, entry: string) {
  return `${api}/licenses?source=${encodeURIComponent(entry)}`
}

/** An entry as the licence routes name it: upstream:<name>, publisher:<name>. */
export function entryOf(s: Pick<KeySource, 'kind' | 'name'>) {
  return `${s.kind}:${s.name}`
}

/** What an entry's source is, in words: an upstream, a publisher, or a distributor's publisher. */
export function entryKind(s: KeySource) {
  if (s.kind === 'upstream') return 'upstream'
  return s.distributor ? `${s.distributor}'s publisher (a distributor)` : 'publisher'
}

/** The entries of a key in a line: publisher acl-ci (2 builds) · upstream enterest (1 build). */
export function sourcesLine(sources: KeySource[]) {
  return sources.map((s) => `${s.kind} ${s.name} (${s.builds} ${s.builds === 1 ? 'build' : 'builds'}${s.distributor ? `, distributor ${s.distributor}` : ''})`).join(' · ')
}

/** Whether a licence's key is a live key of its entry (spec 0035): a live build of the entry, or
 * its newest available version, declares it. A licence signed with another key, or of a removed
 * source, is served nowhere. */
export function declared(keys: NameKeys[], l: Pick<License, 'name' | 'key' | 'entry'>) {
  return keys.some((n) => n.name === l.name && n.keys.some((k) => k.kid === l.key && k.sources.some((s) => entryOf(s) === l.entry)))
}

/** The entries a licence of a name signed with kid would be accepted into: those its key is live
 * for (spec 0035). */
export function entriesFor(keys: NameKeys[] | undefined, ext: string, kid?: string): KeySource[] {
  if (!kid) return []
  return keys?.find((n) => n.name === ext)?.keys.find((k) => k.kid === kid)?.sources ?? []
}

/** The entry an import preselects: the only one listing the key, never one among several (spec
 * 0035: a copied key is another entry, and the choice is the administrator's). */
export function preselect(entries: KeySource[]) {
  return entries.length === 1 ? entryOf(entries[0]) : ''
}

/** The stored licence an import into an entry replaces (spec 0035): the name's licence in that
 * entry. undefined: the import stores a new one. */
export function replaced(licenses: License[], ext: string, entry: string): License | undefined {
  return entry ? licenses.find((l) => l.name === ext && l.entry === entry) : undefined
}

interface SyncResult {
  counts: Record<string, number>
  refused?: { name?: string; id?: string; rule: string; reason: string }[]
  error?: string
}

const day = 86_400_000

/** Whole days until an instant (negative once past). */
export function daysUntil(at: string, now = Date.now()) {
  return Math.ceil((new Date(at).getTime() - now) / day)
}

/** What the list says about a licence's end: in grace, expiring within 30 days, or nothing. */
export function expiry(l: Pick<License, 'status' | 'expires_at' | 'grace_until'>, now = Date.now()): string {
  if (l.status === 'grace') return `in grace: ${Math.max(0, daysUntil(l.grace_until, now))} days left`
  if (l.status !== 'valid') return ''
  const n = daysUntil(l.expires_at, now)
  return n <= 30 ? `expires in ${Math.max(0, n)} ${n === 1 ? 'day' : 'days'}` : ''
}

/** A licence file's claims and its header's kid, read without verifying (the server verifies):
 * only to name what an import stores and replaces. null when it is no licence. */
export function peek(token: string): { ext: string; kid?: string; plan?: string; holder?: string; exp?: number } | null {
  const parts = token.trim().split('.')
  if (parts.length !== 3) return null
  const json = (part: string) => {
    const b64 = part.replace(/-/g, '+').replace(/_/g, '/')
    return JSON.parse(new TextDecoder().decode(Uint8Array.from(atob(b64 + '='.repeat((4 - (b64.length % 4)) % 4)), (c) => c.charCodeAt(0))))
  }
  try {
    const p = json(parts[1])
    if (typeof p?.ext !== 'string' || p.ext === '') return null
    let kid: string | undefined
    try {
      const h = json(parts[0])
      kid = typeof h?.kid === 'string' ? h.kid : undefined
    } catch {
      kid = undefined
    }
    return { ext: p.ext, kid, plan: typeof p.plan === 'string' ? p.plan : undefined, holder: typeof p.holder === 'string' ? p.holder : undefined,
      exp: typeof p.exp === 'number' ? p.exp : undefined }
  } catch {
    return null
  }
}

/** A sync's counts in a line: received 3 · stored 1 · refused 1. */
export function syncLine(r?: SyncResult | null) {
  if (!r) return ''
  return Object.entries(r.counts ?? {}).filter(([, n]) => n > 0).map(([k, n]) => `${k} ${n}`).join(' · ') || 'nothing received'
}

export function Licenses() {
  const { client, api } = useTenant()
  const [open, setOpen] = useState('')
  const list = useLoad(() => client.get<{ licenses: License[] }>(`${api}/licenses`), [api])
  const keys = useLoad(() => client.get<{ names: NameKeys[] }>(`${api}/license-keys`), [api])
  if (list.loading && !list.data) return <Loading />
  if (list.error) return <ErrorBox error={list.error} />
  const ls = list.data!.data.licenses
  const nks = keys.data?.data.names
  const selected = ls.find((l) => licenseKey(l) === open)
  return (
    <>
      <PageTitle title="Licences">
        <span className="pb-1 text-ink-muted">What extensions read from this tenant: imported here, or relayed from an upstream. kista issues none.</span>
      </PageTitle>
      <Card title="Licences" actions={<Button onClick={list.reload}>Refresh</Button>}>
        {ls.length === 0 ? <Empty>No licences.</Empty> : (
          <Table
            rows={ls}
            rowKey={licenseKey}
            columns={[
              ['Extension', (l) => (
                <button type="button" className="font-mono font-medium text-brand-strong underline-offset-2 hover:underline" aria-expanded={open === licenseKey(l)}
                  onClick={() => setOpen(open === licenseKey(l) ? '' : licenseKey(l))}>{l.name}</button>
              )],
              ['Plan', (l) => l.plan],
              ['Holder', (l) => l.holder || <Mono>{l.holder_id}</Mono>],
              ['Source', (l) => (l.source === 'upstream' ? <>upstream <Mono>{l.upstream ?? ''}</Mono></> : 'import')],
              ['Visibility', (l) => <Badge>{l.visibility}</Badge>],
              ['Expires', (l) => (
                <span className="text-[13px]">{when(l.expires_at)}{expiry(l) && <div className="text-warning">{expiry(l)}</div>}</span>
              )],
              ['Status', (l) => (
                <span className="flex flex-wrap items-center gap-2"><Badge>{l.status}</Badge>
                  {nks && !declared(nks, l) && <span className="text-[13px] text-warning">no live build of its entry declares its key</span>}</span>
              )],
              ['Entry', (l) => <Mono>{l.entry}</Mono>],
            ]}
          />
        )}
      </Card>
      {selected && <LicenseView key={licenseKey(selected)} l={selected} onChanged={() => { list.reload() }} onRemoved={() => { setOpen(''); list.reload() }} />}
      <Import client={client} api={api} licenses={ls} keys={nks} keysLoading={keys.loading} keysError={keys.error}
        onImported={(key) => { setOpen(key); list.reload(); keys.reload() }} onStale={() => { list.reload(); keys.reload() }} />
      <Keys names={nks} loading={keys.loading} error={keys.error} reload={keys.reload} />
      <Relays />
    </>
  )
}

/** A licence's claims and limits, and what an administrator changes. */
function LicenseView({ l, onChanged, onRemoved }: { l: License; onChanged: () => void; onRemoved: () => void }) {
  const { client, api, base } = useTenant()
  const [pending, setPending] = useState<'public' | 'private' | 'remove' | null>(null)
  const { busy, failure, act } = useAction('The licence', onChanged)
  const run = async () => {
    const p = pending!
    setPending(null)
    if (p === 'remove') {
      if (await act(() => client.del(licensePath(api, l), { ifMatch: l.etag }))) onRemoved()
      return
    }
    if (await act(() => client.post(licensePath(api, l, p), { ifMatch: l.etag }))) onChanged()
  }
  const versions = l.versions ? [l.versions.min && `from ${l.versions.min}`, l.versions.below && `below ${l.versions.below}`].filter(Boolean).join(', ') : ''
  const facts: [string, React.ReactNode][] = [
    ['Licence', <Mono>{l.id}</Mono>],
    ['Issuer', <Mono>{l.issuer}</Mono>],
    ['Holder', <>{l.holder ? `${l.holder} · ` : ''}<Mono>{l.holder_id}</Mono></>],
    ['Plan', l.plan],
    ['Versions', versions || 'every version'],
    ['Issued', when(l.issued_at)],
    ['Expires', when(l.expires_at)],
    ['Grace until', when(l.grace_until)],
    ['Signed by', <Mono>{l.key}</Mono>],
    ['Entry', <><Mono>{l.entry}</Mono>: the catalogue entry it belongs to; a channel serves it where it serves {l.name} from this entry</>],
    ['Source', l.source === 'upstream' ? <>relayed from upstream <Link className="font-mono" to={`${base}/upstreams/${seg(l.upstream ?? '')}`}>{l.upstream}</Link></> : 'imported'],
  ]
  return (
    <Card title={<span className="font-mono">{l.name}</span>} actions={
      <>
        <Button tone="outline" disabled={busy} onClick={() => setPending(l.visibility === 'public' ? 'private' : 'public')}>Make {l.visibility === 'public' ? 'private' : 'public'}</Button>
        <Button tone="danger-outline" disabled={busy} onClick={() => setPending('remove')}>Remove…</Button>
      </>
    }>
      <div className="flex flex-col gap-4">
        <dl className="m-0 grid grid-cols-[10rem_1fr] gap-x-4 gap-y-2">
          {facts.map(([k, v]) => (
            <div key={k} className="contents"><dt className="text-ink-muted">{k}</dt><dd className="m-0 min-w-0 break-words">{v}</dd></div>
          ))}
        </dl>
        {(l.limits ?? []).length > 0 ? (
          <Table rows={l.limits!} rowKey={(x) => x.name} columns={[
            ['Limit', (x) => <Mono>{x.name}</Mono>],
            ['Value', (x) => <Mono>{String(x.value)}</Mono>],
          ]} />
        ) : <Empty>No limits: the extension's defaults apply.</Empty>}
        <Note>The extension enforces the plan and limits, not kista. A build accepts a licence only when its own key block holds the key that signed it. kista serves the licence in a channel only while a live build there declares that key.</Note>
        {l.visibility === 'public' && <Note>Public: anyone who is served a public release of {l.name} here gets this licence, and can copy it.</Note>}
        {failure != null && <ErrorBox error={failure} />}
      </div>
      <Confirm open={pending !== null} title={pending === 'remove' ? `Remove the licence for ${l.name} in ${l.entry}?` : `Make the licence for ${l.name} in ${l.entry} ${pending}?`}
        confirm={pending === 'remove' ? 'Remove' : `Make ${pending}`} danger={pending !== 'private'} onConfirm={run} onClose={() => setPending(null)}>
        <p>{pending === 'remove'
          ? <>Extensions stop getting it at their next load. {l.source === 'upstream' ? 'A relayed licence comes back at the upstream’s next relay while it still supplies it.' : ''}</>
          : pending === 'public'
            ? 'Whoever is served a public release of the extension in a channel gets it too, without a token: anyone who reaches the channel can copy it.'
            : 'Only tokens holding install on the extension get it.'}</p>
      </Confirm>
    </Card>
  )
}

/** The entry an import goes into (spec 0035), from what is listed (the entries whose live keys
 * include the licence's key), what the administrator chose ('' none yet, 'other', or a listed
 * entry) and what they typed: the chosen listed entry; by default the only one listed, never one
 * among several; a choice no longer listed is dropped; the typed one when they chose another or none
 * is listed. A typed entry is named by its source's name (upstream:<name>, publisher:<name>), as the
 * licences list shows live entries: otherwise the licence it replaces would not be found and no
 * If-Match sent. bad says why a typed entry is not one ('' none). */
export function importEntry(listed: KeySource[], choice: string, typed: string): { entry: string; bad: string } {
  const names = listed.map(entryOf)
  if (listed.length > 0 && choice !== 'other') {
    return { entry: names.includes(choice) ? choice : preselect(listed), bad: '' }
  }
  const entry = typed.trim()
  if (entry !== '' && !/^(upstream|publisher):[^:\s]+$/.test(entry)) {
    return { entry: '', bad: 'Name the entry by its source: upstream:<name> or publisher:<name>.' }
  }
  return { entry, bad: '' }
}

/** Import a licence file into a catalogue entry (spec 0035): the entries whose live keys include
 * the file's key are listed, the only one preselected (never one among several); another may be
 * named, and kista refuses it unless its keys verify the licence. The licence stored in that entry is
 * replaced after a confirmation. A refusal reads the list and the keys again. */
export function Import({ client, api, licenses, keys, keysLoading, keysError, onImported, onStale }: {
  client: Client; api: string; licenses: License[]; keys?: NameKeys[]; keysLoading: boolean; keysError?: unknown
  onImported: (key: string) => void; onStale: () => void
}) {
  const file = useRef<HTMLInputElement>(null)
  const legend = useId()
  const [token, setToken] = useState('')
  const [bad, setBad] = useState(false)
  const [choice, setChoice] = useState('') // '' none yet, a listed entry, or 'other'
  const [other, setOther] = useState('')
  const [replacing, setReplacing] = useState(false)
  const { busy, failure, act } = useAction()
  const p = token ? peek(token) : null
  const listed = p ? entriesFor(keys, p.ext, p.kid) : []
  const { entry, bad: badEntry } = importEntry(listed, choice, other)
  const stored = p ? replaced(licenses, p.ext, entry) : undefined
  const clear = () => {
    setToken('')
    setBad(false)
    setChoice('')
    setOther('')
  }
  const pick = async (f?: File) => {
    clear()
    if (!f) return
    if (f.size > 16384) {
      setBad(true)
      return
    }
    const t = (await f.text()).trim()
    setBad(peek(t) === null)
    setToken(t)
  }
  const send = async () => {
    setReplacing(false)
    const ok = await act(async () => {
      const r = await client.post<License>(importPath(api, entry), { raw: { body: new Blob([token]), type: 'application/jwt' }, ifMatch: stored?.etag })
      onImported(licenseKey(r.data))
      return r
    })
    if (ok) {
      clear()
      if (file.current) file.current.value = ''
    } else onStale()
  }
  const otherInput = (labelled: boolean) => (
    <input className="field font-mono" aria-label={labelled ? 'other entry' : undefined} placeholder="upstream:<name> or publisher:<name>" value={other}
      onChange={(e) => { setOther(e.target.value); if (listed.length > 0) setChoice('other') }} />
  )
  const entries = () => {
    if (keysLoading && !keys) return <Loading />
    if (keysError != null && !keys) return <ErrorBox error={keysError} />
    if (listed.length === 0) {
      return (
        <div className="flex flex-col gap-1.5">
          <Note>No entry's live builds declare this licence's key: kista refuses it unless the entry you name declares it.</Note>
          <Field label="Into the entry">{otherInput(false)}</Field>
        </div>
      )
    }
    return (
      <div role="radiogroup" aria-labelledby={legend} className="flex flex-col gap-1.5">
        <span id={legend} className="text-[13px] font-semibold text-ink">Into the entry{listed.length > 1 ? ': several declare its key, choose one' : ''}</span>
        {listed.map((s) => (
          <label key={entryOf(s)} className="flex items-center gap-2 text-[13px]">
            <input type="radio" name="entry" value={entryOf(s)} checked={choice !== 'other' && entry === entryOf(s)} onChange={() => setChoice(entryOf(s))} />
            <Mono>{entryOf(s)}</Mono><span className="text-ink-muted">{entryKind(s)}</span>
          </label>
        ))}
        <div className="flex items-center gap-2 text-[13px]">
          <label className="flex items-center gap-2">
            <input type="radio" name="entry" value="other" checked={choice === 'other'} onChange={() => setChoice('other')} />
            <span>another entry</span>
          </label>
          {otherInput(true)}
        </div>
      </div>
    )
  }
  return (
    <Card title="Import a licence">
      <div className="flex flex-col gap-3">
        <Note>For an air-gapped site, or a licence received by other means. It goes into one catalogue entry, a source's builds of its extension: it must be in force and verify against a key that a live build of that entry declares (Licence keys, below).</Note>
        <input ref={file} aria-label="licence file" type="file" onChange={(e) => pick(e.target.files?.[0])} />
        {bad && <ErrorBox error="This file is no licence." />}
        {p && !bad && (
          <>
            <p className="m-0">For <Mono>{p.ext}</Mono>{p.plan ? `, plan ${p.plan}` : ''}{p.holder ? `, held by ${p.holder}` : ''}{p.exp ? `, until ${when(new Date(p.exp * 1000).toISOString())}` : ''}.</p>
            {entries()}
            {badEntry && <p className="m-0 text-[13px] text-warning">{badEntry}</p>}
            {stored && <p className="m-0 text-warning">It replaces the licence stored for {p.ext} in {stored.entry} ({stored.source === 'upstream' ? `relayed from ${stored.upstream}` : 'imported'}).</p>}
          </>
        )}
        {failure != null && <ErrorBox error={failure} />}
        <div><Button tone="brand" disabled={busy || !p || bad || !entry} onClick={() => (stored ? setReplacing(true) : send())}>{stored ? 'Replace…' : 'Import'}</Button></div>
      </div>
      <Confirm open={replacing} title={`Replace the licence for ${p?.ext ?? ''} in ${entry}?`} confirm="Replace" danger onConfirm={send} onClose={() => setReplacing(false)}>
        <p>The stored licence goes; extensions get this one at their next load. Its visibility stays.</p>
      </Confirm>
    </Card>
  )
}

/** The licence keys of the tenant's live builds, by name, with the entries each is live for (spec
 * 0021, 0035). */
function Keys({ names, loading, error, reload }: { names?: NameKeys[]; loading: boolean; error?: unknown; reload: () => void }) {
  const rows = (names ?? []).flatMap((n) => n.keys.map((k) => ({ name: n.name, ...k })))
  return (
    <Card title="Licence keys" actions={<Button onClick={reload}>Refresh</Button>}>
      <div className="flex flex-col gap-3">
        <Note>The keys licences verify against: those declared in the key blocks of live builds, recorded when a trusted source brought the build (a publisher a server administrator marked, or a repository upstream). Each is listed with the entries it is live for: an import into one of them accepts it. A key live for several entries is declared by several sources. A build published another way declares nothing here.</Note>
        {loading && !names ? <Loading /> : error ? <ErrorBox error={error} /> : rows.length === 0 ? <Empty>No live build declares a licence key.</Empty> : (
          <Table rows={rows} rowKey={(r) => `${r.name}|${r.kid}`} columns={[
            ['Extension', (r) => <Mono>{r.name}</Mono>],
            ['Key', (r) => <Mono>{r.kid}</Mono>],
            ['Entries', (r) => <span className="text-[13px]">{sourcesLine(r.sources)}</span>],
          ]} />
        )}
      </div>
    </Card>
  )
}

/** The relay: each upstream that relays licences (a repository with a credential, not bundle mode),
 * its last run with its refusals, and a sync on request. */
function Relays() {
  const { client, api, base } = useTenant()
  const ups = useLoad(async () => {
    const all: Upstream[] = []
    for (let cursor = ''; ;) {
      const q = new URLSearchParams({ limit: '500' })
      if (cursor) q.set('cursor', cursor)
      const r = await client.get<{ upstreams: Upstream[]; next?: string }>(`${api}/upstreams?${q}`)
      all.push(...r.data.upstreams)
      if (!r.data.next) break
      cursor = r.data.next
    }
    const relaying = all.filter((u) => u.kind === 'repository' && u.mode !== 'bundle' && u.credential)
    return Promise.all(relaying.map(async (u) => {
      const s = await client.get<{ last_run_at?: string; result: SyncResult | null }>(`${api}/upstreams/${seg(u.name)}/licenses`)
      return { u, at: s.data.last_run_at, result: s.data.result }
    }))
  }, [api])
  const { busy, failure, act } = useAction()
  return (
    <Card title="Relay from upstreams" actions={<Button onClick={ups.reload}>Refresh</Button>}>
      <div className="flex flex-col gap-3">
        <Note>A repository upstream with a credential hands on the licences its credential holds, for the extensions it supplies, every few hours.</Note>
        {failure != null && <ErrorBox error={failure} />}
        {ups.loading && !ups.data ? <Loading /> : ups.error ? <ErrorBox error={ups.error} /> : ups.data!.length === 0 ? <Empty>No upstream relays licences.</Empty> : ups.data!.map(({ u, at, result }) => (
          <div key={u.name} className="flex flex-col gap-2 border-t border-line pt-3 first:border-t-0 first:pt-0">
            <div className="flex flex-wrap items-center gap-3">
              <Link className="font-mono font-medium" to={`${base}/upstreams/${seg(u.name)}`}>{u.name}</Link>
              {u.state !== 'active' && <Badge>{u.state}</Badge>}
              <span className="text-[13px] text-ink-muted">{at ? <>last {when(at)} · {result?.error ? 'failed' : syncLine(result)}</> : 'never relayed'}</span>
              <span className="ml-auto"><Button tone="outline" disabled={busy || u.state !== 'active'} onClick={() => act(() => client.post(`${api}/upstreams/${seg(u.name)}/licenses/sync`)).then(ups.reload)}>Sync now</Button></span>
            </div>
            {result?.error && <ErrorBox error={result.error} />}
            {(result?.refused ?? []).length > 0 && (
              <Table rows={result!.refused!} rowKey={(x) => `${x.name}|${x.id}|${x.rule}`} columns={[
                ['Extension', (x) => <Mono>{x.name ?? ''}</Mono>],
                ['Licence', (x) => <Mono>{x.id ?? ''}</Mono>],
                ['Rule', (x) => <Badge>{x.rule}</Badge>],
                ['Why', (x) => <span className="text-[13px]">{x.reason}</span>],
              ]} />
            )}
          </div>
        ))}
      </div>
    </Card>
  )
}
