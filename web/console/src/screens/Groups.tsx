import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { Button, Card, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, Pager, SearchBox, Table, when } from '../components/ui'
import { useLoad, usePaged, useTenant } from '../lib/context'
import { ApiError, seg } from '../lib/http'
import { GrantForm, GrantsList, useGrants } from './Access'
import type { Event } from './types'

// Extension groups (spec 0017; console phase 2a): named, explicit sets of extension names that
// grants may name. A tenant administrator changes them; a group's administrator reads it.

interface Group {
  name: string
  description: string
  members: number
  grants?: number
  created_at: string
  created_by: string
  etag: string
}

export function Groups() {
  const { client, api, base, perms } = useTenant()
  const [prefix, setPrefix] = useState('')
  const [creating, setCreating] = useState(false)
  const list = usePaged<Group>(async (cursor, limit) => {
    const q = new URLSearchParams({ limit: String(limit) })
    if (prefix) q.set('prefix', prefix)
    if (cursor) q.set('cursor', cursor)
    const r = await client.get<{ extension_groups: Group[]; next?: string }>(`${api}/extension-groups?${q}`)
    return { items: r.data.extension_groups, next: r.data.next }
  }, [api, prefix])
  const admin = perms.tenantAdmin
  return (
    <>
      <PageTitle title="Extension groups">
        <span className="pb-1 text-ink-muted">Grant once for a family of extensions. Members are named one by one, never by pattern.</span>
        {admin && <span className="ml-auto"><Button tone="brand" onClick={() => setCreating(!creating)}>{creating ? 'Close' : 'New group'}</Button></span>}
      </PageTitle>
      {creating && <NewGroup onDone={list.reload} />}
      <div role="search" className="flex items-center gap-2">
        <SearchBox label="Find a group" value={prefix} onChange={setPrefix} />
        <span className="ml-auto"><Button onClick={list.reload}>Refresh</Button></span>
      </div>
      {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : list.items.length === 0 ? (
        <Empty>{prefix ? `No group starts with “${prefix}”.` : 'No extension groups yet.'}</Empty>
      ) : (
        <Table
          rows={list.items}
          rowKey={(g) => g.name}
          columns={[
            ['Group', (g) => (
              <div>
                <Link className="font-mono font-medium no-underline" to={`${base}/groups/${seg(g.name)}`}>{g.name}</Link>
                {g.description && <div className="text-[13px] text-ink-muted">{g.description}</div>}
              </div>
            )],
            ['Extensions', (g) => g.members],
            ...(admin ? [['Grants', (g: Group) => (g.grants ? plural(g.grants, 'grant') : <span className="text-warning">no one yet</span>)] as [string, (g: Group) => React.ReactNode]] : []),
            ['Created', (g) => when(g.created_at)],
          ]}
        />
      )}
      <Pager page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} />
      <Note>An extension may be in several groups. Adding one to a group gives everyone granted on the group access to it at once: only tenant administrators change members, and every change is an event.</Note>
    </>
  )
}

const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? '' : 's'}`

/** The names typed into the add-members field: separated by commas or spaces, each once. */
export function splitNames(s: string): string[] {
  return [...new Set(s.split(/[\s,]+/).filter(Boolean))]
}

function NewGroup({ onDone }: { onDone: () => void }) {
  const { client, api, base } = useTenant()
  const nav = useNavigate()
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      await client.post(`${api}/extension-groups`, { body: { name, description } })
      onDone()
      nav(`${base}/groups/${seg(name)}`)
    } catch (err) {
      setFailure(err)
    }
    setBusy(false)
  }
  return (
    <Card title="New extension group">
      <form onSubmit={submit} className="flex flex-col gap-4">
        <div className="grid grid-cols-[16rem_1fr] gap-4">
          <Field label="Name" hint="lowercase letters, digits and -">
            <input className="field font-mono" required pattern="[a-z0-9][a-z0-9-]{0,62}" value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
          <Field label="Description">
            <input className="field" maxLength={256} value={description} onChange={(e) => setDescription(e.target.value)} />
          </Field>
        </div>
        {failure != null && <ErrorBox error={failure} />}
        <div><Button type="submit" tone="brand" disabled={busy}>Create</Button></div>
      </form>
    </Card>
  )
}

/** One group, its state kept per group: a move to another group starts afresh. */
export function GroupPage() {
  const { g = '' } = useParams()
  return <Group key={g} g={g} />
}

/** One group: its description, members (page by page), grants and recent changes. A group's
 * administrator reads it; the forms are a tenant administrator's. */
function Group({ g }: { g: string }) {
  const { client, api, base, perms } = useTenant()
  const nav = useNavigate()
  const admin = perms.tenantAdmin
  const path = `${api}/extension-groups/${seg(g)}`
  const group = useLoad(() => client.get<Group>(path), [path])
  const [editing, setEditing] = useState(false)
  const [description, setDescription] = useState('')
  const [removing, setRemoving] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  if (group.loading && !group.data) return <Loading />
  if (group.error) return group.error instanceof ApiError && group.error.status === 404 ? <Empty>No such group, or not yours to read.</Empty> : <ErrorBox error={group.error} />
  const gr = group.data!.data
  const etag = group.data!.etag || gr.etag
  const save = async () => {
    setFailure(null)
    try {
      await client.post(`${path}/description`, { body: { description }, ifMatch: etag })
      setEditing(false)
    } catch (e) {
      setFailure(e instanceof ApiError && e.status === 412 ? new Error('The group changed meanwhile: it was read again.') : e)
    }
    group.reload()
  }
  const remove = async () => {
    setRemoving(false)
    setFailure(null)
    try {
      await client.del(path, { ifMatch: etag })
      nav(`${base}/groups`)
    } catch (e) {
      setFailure(e instanceof ApiError && e.status === 409 ? new Error('Grants name this group: revoke them first (below), then remove it.') : e)
      group.reload()
    }
  }
  return (
    <>
      <PageTitle eyebrow={<Link to={`${base}/groups`} className="no-underline">Extension groups</Link>} title={<span className="font-mono">{gr.name}</span>}>
        <span className="pb-1 text-ink-muted">
          {gr.description && `${gr.description} · `}{plural(gr.members, 'extension')}{gr.grants !== undefined && ` · ${plural(gr.grants, 'grant')}`}
        </span>
        {admin && (
          <div className="ml-auto flex gap-2">
            <Button tone="outline" onClick={() => { setDescription(gr.description); setEditing(!editing) }}>Edit description</Button>
            <Button tone="danger-outline" onClick={() => setRemoving(true)}>Delete…</Button>
          </div>
        )}
      </PageTitle>
      {failure != null && <ErrorBox error={failure} />}
      {editing && (
        <form className="flex items-end gap-2" onSubmit={(e) => { e.preventDefault(); save() }}>
          <Field label="Description"><input className="field w-[32rem]" maxLength={256} value={description} onChange={(e) => setDescription(e.target.value)} /></Field>
          <Button type="submit" tone="brand">Save</Button>
        </form>
      )}
      <Members group={gr.name} grants={gr.grants} admin={admin} onChanged={group.reload} />
      {admin && <GroupGrants group={gr.name} onChanged={group.reload} />}
      {perms.canAudit && <RecentChanges group={gr.name} />}
      <Confirm open={removing} title={`Delete the group ${gr.name}?`} confirm="Delete" danger onConfirm={remove} onClose={() => setRemoving(false)}>
        <p>Its {gr.members} member names go with it; the extensions themselves stay. A group that a grant names cannot be deleted: revoke its grants first.</p>
      </Confirm>
    </>
  )
}

/** A group's members, page by page with a prefix; a tenant administrator adds names (at most 100
 * at a time) and removes them one by one. */
function Members({ group, grants, admin, onChanged }: { group: string; grants?: number; admin: boolean; onChanged: () => void }) {
  const { client, api } = useTenant()
  const path = `${api}/extension-groups/${seg(group)}/members`
  const [prefix, setPrefix] = useState('')
  const [names, setNames] = useState('')
  const [removing, setRemoving] = useState('')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const [done, setDone] = useState('')
  const list = usePaged<{ name: string; added_at: string; added_by: string }>(async (cursor, limit) => {
    const q = new URLSearchParams({ limit: String(limit) })
    if (prefix) q.set('prefix', prefix)
    if (cursor) q.set('cursor', cursor)
    const r = await client.get<{ members: { name: string; added_at: string; added_by: string }[]; next?: string }>(`${path}?${q}`)
    return { items: r.data.members, next: r.data.next }
  }, [path, prefix])
  const change = async (add: string[], remove: string[]) => {
    if (busy) return
    setBusy(true)
    setFailure(null)
    setDone('')
    try {
      const r = await client.post<{ added: string[]; removed: string[] }>(path, { body: { add, remove } })
      const { added, removed } = r.data
      setDone(added.length || removed.length ? [added.length && `Added ${added.join(', ')}.`, removed.length && `Removed ${removed.join(', ')}.`].filter(Boolean).join(' ') : 'Nothing changed: those names were already so.')
      setNames('')
    } catch (e) {
      setFailure(e)
    }
    setBusy(false)
    list.reload()
    onChanged()
  }
  const typed = splitNames(names)
  return (
    <Card title="Extensions in it">
      <div className="flex flex-col gap-4">
        <div role="search" className="flex items-center gap-2">
          <SearchBox label="Find a member" value={prefix} onChange={setPrefix} />
        </div>
        {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : list.items.length === 0 ? (
          <Empty>{prefix ? `No member starts with “${prefix}”.` : 'No extensions in this group yet.'}</Empty>
        ) : (
          <Table
            rows={list.items}
            rowKey={(m) => m.name}
            columns={[
              ['Extension', (m) => <Mono>{m.name}</Mono>],
              ['Added', (m) => <span className="text-[13px] text-ink-muted">{when(m.added_at)} by {m.added_by}</span>],
              ...(admin ? [['', (m: { name: string }) => <Button tone="danger-outline" disabled={busy} onClick={() => setRemoving(m.name)}>Remove…</Button>] as [string, (m: { name: string }) => React.ReactNode]] : []),
            ]}
          />
        )}
        <Pager page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} />
        {admin && (
          <form className="flex flex-col gap-2 border-t border-line pt-4" onSubmit={(e) => { e.preventDefault(); change(typed, []) }}>
            <Field label="Add extensions by name" hint="Exact names, separated by commas or spaces; at most 100 at a time. A name may be added before anything is released under it.">
              <textarea className="field min-h-[4rem] font-mono" value={names} onChange={(e) => setNames(e.target.value)} />
            </Field>
            <Note>Adding a name gives everyone granted on this group its releases at once, in every channel the grant covers.</Note>
            <div><Button type="submit" tone="brand" disabled={busy || typed.length === 0 || typed.length > 100}>Add {typed.length > 0 ? typed.length : ''}</Button></div>
          </form>
        )}
        {failure != null && <ErrorBox error={failure} />}
        {done && <p role="status" className="m-0 text-success">{done}</p>}
      </div>
      <Confirm open={removing !== ''} title={`Remove ${removing} from ${group}?`} confirm="Remove" danger
        onConfirm={() => { const n = removing; setRemoving(''); change([], [n]) }} onClose={() => setRemoving('')}>
        <p>
          {grants ? <>The {plural(grants, 'grant')} on this group</> : 'Grants on this group'} stop covering <Mono>{removing}</Mono> from the next
          request: whoever holds them keeps the name only through other grants.
        </p>
      </Confirm>
    </Card>
  )
}

/** The grants on a group, revoked here; and a form to grant on it. */
function GroupGrants({ group, onChanged }: { group: string; onChanged: () => void }) {
  const [granting, setGranting] = useState(false)
  const list = useGrants({ extension_group: group })
  return (
    <Card title="Who may use it" actions={<Button tone="outline" onClick={() => setGranting(!granting)}>{granting ? 'Close' : 'Grant on this group'}</Button>}>
      <div className="flex flex-col gap-4">
        {granting && <GrantForm fixed={{ target: 'extension_group', name: group }} onDone={() => { setGranting(false); list.reload(); onChanged() }} />}
        <GrantsList list={list} principalColumn empty="No one yet." onChanged={onChanged} />
      </div>
    </Card>
  )
}

/** The group's latest events (spec 0010, subject extension-group:<name>). */
function RecentChanges({ group }: { group: string }) {
  const { client, api, base } = useTenant()
  const subject = `extension-group:${group}`
  // the API's subject filter is a prefix (platform would match platform-2): kept exact here
  const evs = useLoad(async () => {
    const r = await client.get<{ events: Event[] }>(`${api}/events?${new URLSearchParams({ subject, limit: '50' })}`)
    return r.data.events.filter((e) => e.subject === subject).slice(0, 10)
  }, [api, subject])
  const describe = (e: Event) => {
    const d = (e.data ?? {}) as { added?: string[]; removed?: string[]; description?: { new?: string }; members?: number }
    switch (e.kind) {
      case 'extension_group.members':
        return [d.added?.length && `added ${d.added.join(', ')}`, d.removed?.length && `removed ${d.removed.join(', ')}`].filter(Boolean).join('; ')
      case 'extension_group.change':
        return `description: ${d.description?.new ?? ''}`
      case 'extension_group.add':
        return 'created'
      default:
        return e.kind
    }
  }
  return (
    <Card title="Recent changes" actions={<Link to={`${base}/events`} className="text-[13px]">All events</Link>}>
      {evs.error ? <ErrorBox error={evs.error} /> : evs.loading && !evs.data ? <Loading /> : evs.data!.length === 0 ? <Empty>No changes recorded.</Empty> : (
        <ul className="m-0 flex list-none flex-col gap-2 p-0">
          {evs.data!.map((e) => (
            <li key={e.id} className="flex gap-4 text-[13px]">
              <span className="w-40 shrink-0 text-ink-muted">{when(e.at)}</span>
              <span className="min-w-0 break-words">{describe(e)} <span className="text-ink-muted">by {e.actor_name || e.actor}</span></span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  )
}
