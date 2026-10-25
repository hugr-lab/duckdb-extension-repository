import { useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Loading, Mono, Note, PageTitle, Table, when } from '../components/ui'
import { useAction } from '../lib/action'
import { useLoad, useTenant } from '../lib/context'
import { seg } from '../lib/http'
import type { Upstream } from './Upstreams'

// Licences (spec 0012 phase 1d): the tenant's licences, imported by its administrators or relayed
// from its credentialed upstreams. kista issues none: it stores what verifies against a key the
// extension's live builds declare (spec 0021: their key blocks, recorded from a marked publisher or
// a repository upstream) and serves it to the extensions' SDK.

export interface License {
  name: string
  /** The licence's source (spec 0033 phase B): the source that owns its key for the name. */
  owner: string
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

/** A licence key of a name's live builds, the trusted sources that brought it, and how many live
 * builds declare it (spec 0021). */
export interface NameKeys {
  name: string
  keys: { kid: string; sources: { kind: 'publisher' | 'upstream'; name: string }[]; builds: number; owner?: string }[]
}

/** A licence's key in the list: a name has a licence per source (spec 0033 phase B). */
export function licenseKey(l: Pick<License, 'name' | 'owner'>) {
  return `${l.name} ${l.owner}`
}

/** The sources of a key in a line: publisher acl-ci · upstream enterest. */
export function sourcesLine(sources: NameKeys['keys'][number]['sources']) {
  return sources.map((s) => `${s.kind} ${s.name}`).join(' · ')
}

/** Whether a licence's key is a live key of its source (spec 0033 phase B): the source brought a
 * live build declaring it. The key's owner's licence, or the very same licence another upstream of
 * the product relayed, is served where its source is; one signed with another key, of a removed
 * source or of none (unknown) is served nowhere. */
export function declared(keys: NameKeys[], l: Pick<License, 'name' | 'key' | 'owner'>) {
  return keys.some((n) => n.name === l.name && n.keys.some((k) => k.kid === l.key && k.sources.some((s) => `${s.kind}:${s.name}` === l.owner)))
}

/** The stored licence an import of a licence file replaces (spec 0033 phase B): the one of the
 * source that owns its key for the name, or where that source holds none, one of unknown source
 * signed with the same key. undefined: the import stores a new one. */
export function replaced(licenses: License[], keys: NameKeys[] | undefined, ext: string, kid?: string): License | undefined {
  const owner = kid ? keys?.find((n) => n.name === ext)?.keys.find((k) => k.kid === kid)?.owner : undefined
  if (!owner) return undefined
  const named = licenses.filter((l) => l.name === ext)
  return named.find((l) => l.owner === owner) ?? named.find((l) => l.owner === 'unknown' && l.key === kid)
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
                  {nks && !declared(nks, l) && <span className="text-[13px] text-warning">not served: its key is no live key of its source</span>}</span>
              )],
              ['Of', (l) => <Mono>{l.owner}</Mono>],
            ]}
          />
        )}
      </Card>
      {selected && <LicenseView key={licenseKey(selected)} l={selected} onChanged={() => { list.reload() }} onRemoved={() => { setOpen(''); list.reload() }} />}
      <Import licenses={ls} keys={nks} onImported={(key) => { setOpen(key); list.reload(); keys.reload() }} onStale={() => { list.reload(); keys.reload() }} />
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
  const path = `${api}/licenses/${seg(l.name)}?source=${encodeURIComponent(l.owner)}`
  const run = async () => {
    const p = pending!
    setPending(null)
    if (p === 'remove') {
      if (await act(() => client.del(path, { ifMatch: l.etag }))) onRemoved()
      return
    }
    if (await act(() => client.post(`${path}/${p}`, { ifMatch: l.etag }))) onChanged()
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
    ['Of', <><Mono>{l.owner}</Mono>: the source that owns the key for {l.name}; a channel serves it where it serves {l.name} from that source</>],
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
      <Confirm open={pending !== null} title={pending === 'remove' ? `Remove the licence for ${l.name}?` : `Make the licence for ${l.name} ${pending}?`}
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

/** Import a licence file; the stored licence it replaces (of the source that owns its key) is
 * replaced after a confirmation. A refusal reads the list and the keys again: a licence a relay
 * stored meanwhile, a changed ETag or a key's new owner is then what the next try replaces. */
function Import({ licenses, keys, onImported, onStale }: {
  licenses: License[]; keys?: NameKeys[]; onImported: (key: string) => void; onStale: () => void
}) {
  const { client, api } = useTenant()
  const file = useRef<HTMLInputElement>(null)
  const [token, setToken] = useState('')
  const [bad, setBad] = useState(false)
  const [replacing, setReplacing] = useState(false)
  const { busy, failure, act } = useAction()
  const p = token ? peek(token) : null
  // the licence it replaces: of the source that owns its key for the name (spec 0033 phase B)
  const stored = p ? replaced(licenses, keys, p.ext, p.kid) : undefined
  const pick = async (f?: File) => {
    setToken('')
    setBad(false)
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
      const r = await client.post<License>(`${api}/licenses`, { raw: { body: new Blob([token]), type: 'application/jwt' }, ifMatch: stored?.etag })
      onImported(licenseKey(r.data))
      return r
    })
    if (ok) {
      setToken('')
      if (file.current) file.current.value = ''
    } else onStale()
  }
  return (
    <Card title="Import a licence">
      <div className="flex flex-col gap-3">
        <Note>For an air-gapped site, or a licence received by other means. It must be in force and verify against a key that a live build of its extension declares (Licence keys, below).</Note>
        <input ref={file} aria-label="licence file" type="file" onChange={(e) => pick(e.target.files?.[0])} />
        {bad && <ErrorBox error="This file is no licence." />}
        {p && !bad && (
          <p className="m-0">For <Mono>{p.ext}</Mono>{p.plan ? `, plan ${p.plan}` : ''}{p.holder ? `, held by ${p.holder}` : ''}{p.exp ? `, until ${when(new Date(p.exp * 1000).toISOString())}` : ''}.
            {stored && <span className="text-warning"> It replaces the licence stored for {p.ext} of {stored.owner} ({stored.source === 'upstream' ? `relayed from ${stored.upstream}` : 'imported'}).</span>}</p>
        )}
        {failure != null && <ErrorBox error={failure} />}
        <div><Button tone="brand" disabled={busy || !p || bad} onClick={() => (stored ? setReplacing(true) : send())}>{stored ? 'Replace…' : 'Import'}</Button></div>
      </div>
      <Confirm open={replacing} title={`Replace the licence for ${p?.ext ?? ''}?`} confirm="Replace" danger onConfirm={send} onClose={() => setReplacing(false)}>
        <p>The stored licence goes; extensions get this one at their next load. Its visibility stays.</p>
      </Confirm>
    </Card>
  )
}

/** The licence keys of the tenant's live builds, by name, with their sources (spec 0021). */
function Keys({ names, loading, error, reload }: { names?: NameKeys[]; loading: boolean; error?: unknown; reload: () => void }) {
  const rows = (names ?? []).flatMap((n) => n.keys.map((k) => ({ name: n.name, ...k })))
  return (
    <Card title="Licence keys" actions={<Button onClick={reload}>Refresh</Button>}>
      <div className="flex flex-col gap-3">
        <Note>The keys licences verify against: those declared in the key blocks of live builds, recorded when a trusted source brought the build (a publisher a server administrator marked, or a repository upstream). A build published another way declares nothing here.</Note>
        {loading && !names ? <Loading /> : error ? <ErrorBox error={error} /> : rows.length === 0 ? <Empty>No live build declares a licence key.</Empty> : (
          <Table rows={rows} rowKey={(r) => `${r.name}|${r.kid}`} columns={[
            ['Extension', (r) => <Mono>{r.name}</Mono>],
            ['Key', (r) => <Mono>{r.kid}</Mono>],
            ['Builds', (r) => r.builds],
            ['Sources', (r) => <span className="text-[13px]">{sourcesLine(r.sources)}</span>],
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
