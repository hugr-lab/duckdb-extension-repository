import { useState } from 'react'
import { Link } from 'react-router-dom'
import { Button, Card, Checks, Chips, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, Pager, SearchBox, Table, when } from '../components/ui'
import { useLoad, usePaged, useTenant } from '../lib/context'
import { ApiError, seg } from '../lib/http'

// Access (spec 0015 phase 2a), as tresor-server's Access screen: the tenant's principals with their
// grant counts, a principal's grants, granting and revoking; and who may use one extension.

export interface GrantRow {
  id: string
  principal: string
  verbs: string[]
  channel?: string
  extension?: string
  extension_group?: string
  created_at: string
  created_by: string
  via?: 'tenant' | 'channel' | 'extension' | 'extension_group'
  verbs_on_extension?: string[]
}

/** What kind of principal this is, in words; a person or a service is the exception. */
export function principalKind(p: string): string {
  const kind = p.slice(0, p.indexOf(':'))
  return { subject: 'a person', client: 'a service', publisher: 'a publisher', issuer: 'every account of an issuer', role: 'a role', group: 'a group of people' }[kind] ?? kind
}

/** What a grant is on, in words. */
export function grantTarget(g: GrantRow) {
  const where = g.channel ? ` in ${g.channel}` : ''
  if (g.extension_group) return <><span className="text-ink-muted">group</span> <Mono>{g.extension_group}</Mono>{where}</>
  if (g.extension) return <><Mono>{g.extension}</Mono>{where}</>
  if (g.channel) return <><span className="text-ink-muted">channel</span> <Mono>{g.channel}</Mono></>
  return <span className="text-ink-muted">the whole tenant</span>
}

export function Access() {
  const { client, api } = useTenant()
  const [prefix, setPrefix] = useState('')
  const [picked, setPicked] = useState('')
  const [typed, setTyped] = useState('')
  const list = usePaged<{ principal: string; grants: number }>(async (cursor, limit) => {
    const q = new URLSearchParams({ limit: String(limit) })
    if (prefix) q.set('prefix', prefix)
    if (cursor) q.set('cursor', cursor)
    const r = await client.get<{ principals: { principal: string; grants: number }[]; next?: string }>(`${api}/grants/principals?${q}`)
    return { items: r.data.principals, next: r.data.next }
  }, [api, prefix])
  return (
    <>
      <PageTitle title="Access">
        <span className="pb-1 text-ink-muted">Who may do what in this tenant.</span>
      </PageTitle>
      <Note>People get access through their identity provider's roles and groups. Grant to a person or a client only as an exception.</Note>
      <div className="grid grid-cols-[minmax(260px,320px)_1fr] items-start gap-6">
        <section aria-label="principals" className="flex flex-col gap-3 rounded-lg border border-line bg-surface p-4">
          <SearchBox label="Find a principal" placeholder="Starts with… (role:kc|)" value={prefix} onChange={setPrefix} />
          {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : list.items.length === 0 ? <Empty>No grants yet.</Empty> : (
            <ul className="m-0 flex list-none flex-col gap-1 p-0">
              {list.items.map((p) => (
                <li key={p.principal}>
                  <button type="button" aria-pressed={picked === p.principal} onClick={() => setPicked(p.principal)}
                    className={`flex w-full items-center gap-2 rounded-md px-3 py-2 text-left ${picked === p.principal ? 'bg-surface-soft font-semibold' : 'hover:bg-row'}`}>
                    <Mono>{p.principal}</Mono>
                    {p.principal.startsWith('subject:') && <span className="chip border border-line text-ink-muted">a person</span>}
                    <span className="ml-auto text-[13px] text-ink-muted">{p.grants}</span>
                  </button>
                </li>
              ))}
            </ul>
          )}
          <Pager compact page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} />
          <form className="flex flex-col gap-2 border-t border-line pt-3" onSubmit={(e) => { e.preventDefault(); if (typed.trim()) setPicked(typed.trim()) }}>
            <Field label="Another principal" hint="kind:issuer|value, e.g. role:kc|platform-team">
              <input className="field font-mono" value={typed} onChange={(e) => setTyped(e.target.value)} />
            </Field>
            <Button type="submit">Open</Button>
          </form>
        </section>
        {picked ? <Principal key={picked} principal={picked} onChanged={list.reload} /> : <Empty>Choose a principal to see and change its grants.</Empty>}
      </div>
    </>
  )
}

/** One principal: its grants, page by page, revoked one by one; and a form to grant it more. */
function Principal({ principal, onChanged }: { principal: string; onChanged: () => void }) {
  const { client, api } = useTenant()
  const list = usePaged<GrantRow>(async (cursor, limit) => {
    const q = new URLSearchParams({ limit: String(limit), principal })
    if (cursor) q.set('cursor', cursor)
    const r = await client.get<{ grants: GrantRow[]; next?: string }>(`${api}/grants?${q}`)
    return { items: r.data.grants, next: r.data.next }
  }, [api, principal])
  const changed = () => {
    list.reload()
    onChanged()
  }
  return (
    <div className="flex flex-col gap-6">
      <div>
        <h2 className="m-0 font-mono text-[18px] font-bold">{principal}</h2>
        <span className="text-ink-muted">{principalKind(principal)}</span>
      </div>
      <Card title="Grant">
        <GrantForm principal={principal} onDone={changed} />
      </Card>
      <Card title="Granted">
        {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : list.items.length === 0 ? <Empty>Nothing granted.</Empty> : (
          <GrantTable rows={list.items} onRevoked={changed} />
        )}
        <div className="mt-3"><Pager page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} /></div>
      </Card>
    </div>
  )
}

/** Grants, each revocable unless revocable says otherwise (then the reason shows instead). */
export function GrantTable({ rows, onRevoked, principalColumn, revocable, verbs }: {
  rows: GrantRow[]
  onRevoked: () => void
  principalColumn?: boolean
  revocable?: (g: GrantRow) => string | true
  verbs?: (g: GrantRow) => string[]
}) {
  const { client, api } = useTenant()
  const [pending, setPending] = useState<GrantRow | null>(null)
  const [failure, setFailure] = useState<unknown>(null)
  const revoke = async () => {
    const g = pending!
    setPending(null)
    setFailure(null)
    try {
      await client.del(`${api}/grants/${seg(g.id)}`)
    } catch (e) {
      setFailure(e instanceof ApiError && e.status === 409 ? new Error(e.message) : e)
    }
    onRevoked()
  }
  const columns: [string, (g: GrantRow) => React.ReactNode][] = []
  if (principalColumn) columns.push(['Who', (g) => <><Mono>{g.principal}</Mono><div className="text-[12px] text-ink-muted">{principalKind(g.principal)}</div></>])
  columns.push(
    ['On', grantTarget],
    ['May', (g) => <span className="flex flex-wrap gap-1">{(verbs ? verbs(g) : g.verbs).map((v) => <span key={v} className="chip bg-surface-soft text-ink">{v}</span>)}</span>],
    ['Granted', (g) => <span className="text-[13px] text-ink-muted">{when(g.created_at)}</span>],
    ['', (g) => {
      const ok = revocable ? revocable(g) : true
      return ok === true ? <Button tone="danger-outline" onClick={() => setPending(g)}>Revoke…</Button> : <span className="text-[13px] text-ink-muted">{ok}</span>
    }],
  )
  return (
    <>
      {failure != null && <ErrorBox error={failure} />}
      <Table rows={rows} rowKey={(g) => g.id} columns={columns} />
      <Confirm open={pending !== null} title="Revoke this grant?" confirm="Revoke" danger onConfirm={revoke} onClose={() => setPending(null)}>
        {pending && <p><Mono>{pending.principal}</Mono> loses {pending.verbs.join(', ')} on {grantTarget(pending)}.</p>}
      </Confirm>
    </>
  )
}

type Target = 'extension_group' | 'extension' | 'channel' | 'tenant'
const verbsFor: Record<Target, string[]> = {
  extension_group: ['install', 'publish', 'promote', 'admin'],
  extension: ['install', 'publish', 'promote', 'admin'],
  channel: ['install', 'publish', 'promote', 'admin'],
  tenant: ['install', 'publish', 'promote', 'audit', 'admin'],
}

/** Grants verbs to a principal (given, or typed) on the tenant, a channel, an extension or an
 * extension group; fixed fixes the target (a group's or an extension's page). */
export function GrantForm({ principal, fixed, only, onDone }: {
  principal?: string
  fixed?: { target: 'extension_group' | 'extension'; name: string; channel?: string }
  /** the verbs offered at most (a publisher: publish and promote) */
  only?: string[]
  onDone: () => void
}) {
  const { client, api } = useTenant()
  const [who, setWho] = useState('')
  const [target, setTarget] = useState<Target>(fixed?.target ?? 'extension_group')
  const [name, setName] = useState('')
  const [channel, setChannel] = useState(fixed?.channel ?? '')
  const [verbs, setVerbs] = useState<string[]>([only?.[0] ?? 'install'])
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const [done, setDone] = useState('')
  const channels = useLoad(() => client.get<{ channels: { name: string }[] }>(`${api}/channels`), [api])
  const groups = useLoad(() => (fixed || target !== 'extension_group' ? Promise.resolve(null) :
    client.get<{ extension_groups: { name: string }[] }>(`${api}/extension-groups?limit=500`)), [api, target, !!fixed])
  const allowed = verbsFor[target].filter((v) => !only || only.includes(v))
  const chosen = verbs.filter((v) => allowed.includes(v))
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setFailure(null)
    setDone('')
    const body: Record<string, unknown> = { principal: principal ?? who.trim(), verbs: chosen }
    if (target !== 'tenant' && (target === 'channel' || channel)) body.channel = channel
    const n = fixed?.name ?? name.trim()
    if (target === 'extension') body.extension = n
    if (target === 'extension_group') body.extension_group = n
    try {
      await client.post(`${api}/grants`, { body })
      setDone(`Granted ${chosen.join(', ')}.`)
      onDone()
    } catch (err) {
      setFailure(err)
    }
    setBusy(false)
  }
  const needsName = !fixed && (target === 'extension' || target === 'extension_group')
  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      {!principal && (
        <Field label="Principal" hint="kind:issuer|value: role:kc|platform-team, group:kc|analysts, client:kc|node, subject:kc|<sub>, publisher:<name>">
          <input className="field font-mono" required value={who} onChange={(e) => setWho(e.target.value)} />
        </Field>
      )}
      {!fixed && (
        <Field label="On">
          <Chips label="what the grant is on" value={target} onChange={setTarget}
            options={[['extension_group', 'Extension group'], ['extension', 'Extension'], ['channel', 'Channel'], ['tenant', 'Whole tenant']]} />
        </Field>
      )}
      <div className="flex flex-wrap gap-4">
        {needsName && (
          <Field label={target === 'extension_group' ? 'Group' : 'Extension'}>
            <input className="field font-mono" required list={target === 'extension_group' ? 'kista-groups' : undefined} value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
        )}
        {target !== 'tenant' && (
          <Field label={target === 'channel' ? 'Channel' : 'In channel'}>
            <select className="field" value={channel} required={target === 'channel'} onChange={(e) => setChannel(e.target.value)}>
              {target !== 'channel' && <option value="">every channel</option>}
              {target === 'channel' && <option value="" disabled>choose…</option>}
              {(channels.data?.data.channels ?? []).map((c) => <option key={c.name}>{c.name}</option>)}
            </select>
          </Field>
        )}
      </div>
      <datalist id="kista-groups">{(groups.data?.data.extension_groups ?? []).map((g) => <option key={g.name} value={g.name} />)}</datalist>
      <Field label="May">
        <Checks label="verbs" options={allowed} value={chosen} onChange={setVerbs} />
      </Field>
      {target === 'extension_group' && chosen.includes('admin') && (
        <Note>admin on a group manages its extensions' releases. Only a tenant administrator changes who is in the group.</Note>
      )}
      {chosen.includes('admin') && target === 'tenant' && <Note>admin on the whole tenant is everything here, grants included.</Note>}
      {failure != null && <ErrorBox error={failure} />}
      {done && <p role="status" className="m-0 text-success">{done}</p>}
      <div><Button type="submit" tone="brand" disabled={busy || chosen.length === 0}>Grant</Button></div>
    </form>
  )
}

/** Who may use an extension in a channel (its page, tenant administrators): the grants that reach
 * it there, how, and the verbs that apply; only grants on the extension itself are revoked here. */
export function WhoMayUse({ channel, ext }: { channel: string; ext: string }) {
  const { client, api, base } = useTenant()
  const [granting, setGranting] = useState(false)
  const list = useLoad(async () => {
    const rows: GrantRow[] = []
    let cursor = ''
    // every grant reaching one name: a tenant holds at most 1000, so a few pages at most
    for (;;) {
      const q = new URLSearchParams({ extension: ext, limit: '500' })
      if (cursor) q.set('cursor', cursor)
      const r = await client.get<{ grants: GrantRow[]; next?: string }>(`${api}/grants?${q}`)
      rows.push(...r.data.grants)
      if (!r.data.next) return rows
      cursor = r.data.next
    }
  }, [api, ext])
  const here = (list.data ?? []).filter((g) => !g.channel || g.channel === channel)
  const where: Record<string, string> = { tenant: 'on the tenant', channel: 'on the channel', extension_group: 'through the group' }
  return (
    <Card title={<>Who may use <span className="font-mono">{ext}</span> in {channel}</>}
      actions={<Button tone="outline" onClick={() => setGranting(!granting)}>{granting ? 'Close' : 'Grant access'}</Button>}>
      <div className="flex flex-col gap-4">
        {granting && <GrantForm fixed={{ target: 'extension', name: ext, channel }} onDone={list.reload} />}
        {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : here.length === 0 ? <Empty>Only what is public: no grant reaches it.</Empty> : (
          <GrantTable rows={here} onRevoked={list.reload} principalColumn verbs={(g) => g.verbs_on_extension ?? g.verbs}
            revocable={(g) => (g.via === 'extension' ? true : where[g.via ?? 'tenant'])} />
        )}
        <Note>
          Grants on the tenant, the channel or a group count here too, and are changed where they are made:{' '}
          <Link to={`${base}/access`}>Access</Link> or the group's page.
        </Note>
      </div>
    </Card>
  )
}

