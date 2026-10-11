import { useId, useState } from 'react'
import { Link } from 'react-router-dom'
import { Button, Card, Checks, Chips, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, Pager, SearchBox, Table, when } from '../components/ui'
import { type Grant, useLoad, usePaged, useTenant } from '../lib/context'
import { seg } from '../lib/http'

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

/** What kind of principal this is, in words; a person and a service are the exception. */
export function principalKind(p: string): string {
  const i = p.indexOf(':')
  if (i < 0) return 'not a principal: kind:issuer|value'
  const kind = p.slice(0, i)
  return ({ subject: 'a person', client: 'a service', publisher: 'a publisher', issuer: 'every account of an issuer', role: 'a role', group: 'a group of people' } as Record<string, string>)[kind] ?? kind
}

/** What a grant is on, in words. */
export function grantTarget(g: GrantRow) {
  const where = g.channel ? ` in ${g.channel}` : ' in every channel'
  if (g.extension_group) return <><span className="text-ink-muted">group</span> <Mono>{g.extension_group}</Mono>{where}</>
  if (g.extension) return <><Mono>{g.extension}</Mono>{where}</>
  if (g.channel) return <><span className="text-ink-muted">channel</span> <Mono>{g.channel}</Mono></>
  return <span className="text-ink-muted">the whole tenant</span>
}

/** Whether a listed grant is one of the caller's own (whoami's grants carry no id). */
function isOwn(g: GrantRow, mine: Grant[]) {
  const same = (a: string[], b: string[]) => a.length === b.length && a.every((v) => b.includes(v))
  return mine.some((m) => m.principal === g.principal && same(m.verbs, g.verbs) && (m.channel ?? '') === (g.channel ?? '') &&
    (m.extension ?? '') === (g.extension ?? '') && (m.extension_group ?? '') === (g.extension_group ?? ''))
}

/** The tenant's grants with a filter (principal, extension_group), page by page. */
export function useGrants(filter: Record<string, string>) {
  const { client, api } = useTenant()
  const key = new URLSearchParams(filter).toString()
  return usePaged<GrantRow>(async (cursor, limit) => {
    const q = new URLSearchParams({ ...filter, limit: String(limit) })
    if (cursor) q.set('cursor', cursor)
    const r = await client.get<{ grants: GrantRow[]; next?: string }>(`${api}/grants?${q}`)
    return { items: r.data.grants, next: r.data.next }
  }, [api, key])
}

/** Grants of a list: the table with revoke, the pager, an empty state. */
export function GrantsList({ list, principalColumn, empty, onChanged }: {
  list: ReturnType<typeof useGrants>
  principalColumn?: boolean
  empty: string
  onChanged?: () => void
}) {
  return (
    <>
      {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : list.items.length === 0 ? <Empty>{empty}</Empty> : (
        <GrantTable rows={list.items} principalColumn={principalColumn} onRevoked={() => { list.reload(); onChanged?.() }} />
      )}
      <Pager page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} />
    </>
  )
}

/** A principal's characters (kind:issuer|value): the API's prefix takes any but whitespace here. */
const principalRefuse = /\s/g

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
          <SearchBox label="Find a principal" placeholder="role:kc|…" value={prefix} onChange={setPrefix} refuse={principalRefuse} maxLength={300} mono />
          {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : list.items.length === 0 ? (
            <Empty>{prefix ? <>No principal starts with <Mono>{prefix}</Mono>.</> : 'No grants yet.'}</Empty>
          ) : (
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
            <Field label="Another principal" hint={<>kind:issuer|value, e.g. <Mono>role:kc|platform-team</Mono></>}>
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
  const list = useGrants({ principal })
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
        <GrantsList list={list} empty="Nothing granted." onChanged={onChanged} />
      </Card>
    </div>
  )
}

/** Grants, each revocable unless revocable says otherwise (then where it is changed shows). */
export function GrantTable({ rows, onRevoked, principalColumn, revocable, verbs }: {
  rows: GrantRow[]
  onRevoked: () => void
  principalColumn?: boolean
  revocable?: (g: GrantRow) => React.ReactNode | true
  verbs?: (g: GrantRow) => string[]
}) {
  const { client, api, perms } = useTenant()
  const [pending, setPending] = useState<GrantRow | null>(null)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const revoke = async () => {
    const g = pending!
    setPending(null)
    setFailure(null)
    setBusy(true)
    try {
      await client.del(`${api}/grants/${seg(g.id)}`)
    } catch (e) {
      setFailure(e)
    }
    setBusy(false)
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
      return ok === true ? <Button tone="danger-outline" disabled={busy} onClick={() => setPending(g)}>Revoke…</Button> : <span className="text-[13px] text-ink-muted">{ok}</span>
    }],
  )
  return (
    <>
      {failure != null && <ErrorBox error={failure} />}
      <Table rows={rows} rowKey={(g) => g.id} columns={columns} />
      <Confirm open={pending !== null} title="Revoke this grant?" confirm="Revoke" danger onConfirm={revoke} onClose={() => setPending(null)}>
        {pending && <p><Mono>{pending.principal}</Mono> loses {pending.verbs.join(', ')} on {grantTarget(pending)}.</p>}
        {pending && isOwn(pending, perms.grants) && <p className="font-semibold text-danger">This is one of your own grants: you may lose access here.</p>}
      </Confirm>
    </>
  )
}

export type Target = 'extension_group' | 'extension' | 'channel' | 'tenant'
const verbsFor: Record<Target, string[]> = {
  extension_group: ['install', 'publish', 'promote', 'admin'],
  extension: ['install', 'publish', 'promote', 'admin'],
  channel: ['install', 'publish', 'promote', 'admin'],
  tenant: ['install', 'publish', 'promote', 'audit', 'admin'],
}

/** The verbs a form offers: the target's, within only, and install alone to an issuer-wide
 * principal (spec 0006: every account of the issuer would hold the others). */
export function offeredVerbs(target: Target, principal: string, only?: string[]): string[] {
  return verbsFor[target].filter((v) => (!only || only.includes(v)) && (!principal.startsWith('issuer:') || v === 'install'))
}

/** The body of POST …/grants: channel only when one is chosen, the name under its target's key. */
export function grantBody(principal: string, target: Target, name: string, channel: string, verbs: string[]) {
  const body: Record<string, unknown> = { principal, verbs }
  if (target !== 'tenant' && channel) body.channel = channel
  if (target === 'extension') body.extension = name
  if (target === 'extension_group') body.extension_group = name
  return body
}

/** Grants verbs to a principal (given, or typed) on the tenant, a channel, an extension or an
 * extension group; fixed fixes the target (a group's or an extension's page, its channel kept). */
export function GrantForm({ principal, fixed, only, targets, onDone }: {
  principal?: string
  fixed?: { target: 'extension_group' | 'extension'; name: string; channel?: string }
  /** the verbs offered at most (a publisher: publish and promote) */
  only?: string[]
  /** the targets offered (a publisher: never the whole tenant) */
  targets?: Target[]
  onDone: () => void
}) {
  const { client, api } = useTenant()
  const ids = useId()
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
  const p = principal ?? who.trim()
  const allowed = offeredVerbs(target, p, only)
  const chosen = verbs.filter((v) => allowed.includes(v))
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setFailure(null)
    setDone('')
    try {
      await client.post(`${api}/grants`, { body: grantBody(p, target, fixed?.name ?? name.trim(), channel, chosen) })
      setDone(`Granted ${chosen.join(', ')} to ${p}.`)
      setWho('')
      setName('')
      onDone()
    } catch (err) {
      setFailure(err)
    }
    setBusy(false)
  }
  const needsName = !fixed && (target === 'extension' || target === 'extension_group')
  const offered = ([['extension_group', 'Extension group'], ['extension', 'Extension'], ['channel', 'Channel'], ['tenant', 'Whole tenant']] as [Target, string][])
    .filter(([t]) => !targets || targets.includes(t))
  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      {!principal && (
        <Field label="Principal" hint={<>kind:issuer|value: <Mono>role:kc|platform-team</Mono>, <Mono>group:kc|analysts</Mono>, <Mono>client:kc|node</Mono>, <Mono>publisher:ci</Mono></>}>
          <input className="field font-mono" required value={who} onChange={(e) => setWho(e.target.value)} />
        </Field>
      )}
      {!fixed && (
        <div className="flex flex-col gap-1">
          <span className="text-[13px] font-semibold text-ink">On</span>
          <Chips label="what the grant is on" value={target} onChange={setTarget} options={offered} />
        </div>
      )}
      <div className="flex flex-wrap gap-4">
        {needsName && (
          <Field label={target === 'extension_group' ? 'Group' : 'Extension'}>
            <input className="field font-mono" required list={target === 'extension_group' ? `${ids}-groups` : undefined} value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
        )}
        {target !== 'tenant' && (
          <Field label={target === 'channel' ? 'Channel' : 'In channel'} hint={fixed?.channel ? 'this page’s channel; grant in every channel from Access' : undefined}>
            <select className="field" value={channel} required={target === 'channel'} disabled={!!fixed?.channel} onChange={(e) => setChannel(e.target.value)}>
              {target !== 'channel' && <option value="">every channel</option>}
              {target === 'channel' && <option value="" disabled>choose…</option>}
              {(channels.data?.data.channels ?? []).map((c) => <option key={c.name}>{c.name}</option>)}
            </select>
          </Field>
        )}
      </div>
      <datalist id={`${ids}-groups`}>{(groups.data?.data.extension_groups ?? []).map((g) => <option key={g.name} value={g.name} />)}</datalist>
      <div className="flex flex-col gap-1">
        <span className="text-[13px] font-semibold text-ink">May</span>
        <Checks label="verbs" options={allowed} value={chosen} onChange={setVerbs} />
      </div>
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
    // every grant reaching one name: a tenant holds at most 1000, so two pages at most
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
  const changedAt = (g: GrantRow): React.ReactNode | true => {
    switch (g.via) {
      case 'extension':
        return true
      case 'extension_group':
        return <>through the group: <Link to={`${base}/groups/${seg(g.extension_group ?? '')}`}>change it there</Link></>
      case 'channel':
        return <>on the channel: <Link to={`${base}/access`}>change it in Access</Link></>
      case 'tenant':
        return <>on the tenant: <Link to={`${base}/access`}>change it in Access</Link></>
      default:
        return <Link to={`${base}/access`}>change it in Access</Link>
    }
  }
  return (
    <Card title={<>Who may use <span className="font-mono">{ext}</span> in {channel}</>}
      actions={<Button tone="outline" onClick={() => setGranting(!granting)}>{granting ? 'Close' : 'Grant access'}</Button>}>
      <div className="flex flex-col gap-4">
        {granting && <GrantForm fixed={{ target: 'extension', name: ext, channel }} onDone={() => { setGranting(false); list.reload() }} />}
        {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : here.length === 0 ? (
          <Empty>No grant reaches it: only its public releases are served, to anyone.</Empty>
        ) : (
          <GrantTable rows={here} onRevoked={list.reload} principalColumn verbs={(g) => g.verbs_on_extension ?? g.verbs} revocable={changedAt} />
        )}
        <Note>Grants on the tenant, the channel or a group count here too, and are changed where they are made.</Note>
      </div>
    </Card>
  )
}
