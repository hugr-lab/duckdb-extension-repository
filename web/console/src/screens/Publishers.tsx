import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { Button, Card, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, Pager, Table, when } from '../components/ui'
import { useLoad, usePaged, useTenant } from '../lib/context'
import { ApiError, seg } from '../lib/http'
import { GrantForm, GrantsList, useGrants } from './Access'

// Publishers (spec 0008; console phase 2a): identities that only publish and promote, from CI by
// trusted publishing (GitHub Actions OIDC) or with an API key. They never sign in here.

interface GitHub {
  id: string
  provider: string
  owner_id: string
  repository_id: string
  workflow: string
  ref?: string
  environment?: string
  created_at: string
  created_by: string
}

interface Publisher {
  name: string
  created_at: string
  created_by: string
  github: GitHub[]
}

interface APIKey {
  id: string
  prefix: string
  key?: string
  expires_at: string
  created_at: string
  created_by: string
  last_used_at?: string
}

export function Publishers() {
  const { client, api, base } = useTenant()
  const nav = useNavigate()
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const list = usePaged<Publisher>(async (cursor, limit) => {
    const q = new URLSearchParams({ limit: String(limit) })
    if (cursor) q.set('cursor', cursor)
    const r = await client.get<{ publishers: Publisher[]; next?: string }>(`${api}/publishers?${q}`)
    return { items: r.data.publishers, next: r.data.next }
  }, [api])
  const add = async (e: React.FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      await client.post(`${api}/publishers`, { body: { name } })
      nav(`${base}/publishers/${seg(name)}`)
    } catch (err) {
      setFailure(err)
    }
    setBusy(false)
  }
  return (
    <>
      <PageTitle title="Publishers">
        <span className="pb-1 text-ink-muted">Identities that publish and promote from CI. They never sign in here.</span>
      </PageTitle>
      <form className="flex items-end gap-2" onSubmit={add}>
        <Field label="New publisher"><input className="field font-mono" required pattern="[a-z][a-z0-9-]{0,63}" placeholder="acl-ci" value={name} onChange={(e) => setName(e.target.value)} /></Field>
        <Button type="submit" tone="brand" disabled={busy}>Add</Button>
      </form>
      {failure != null && <ErrorBox error={failure} />}
      {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : list.items.length === 0 ? <Empty>No publishers.</Empty> : (
        <Table
          rows={list.items}
          rowKey={(p) => p.name}
          columns={[
            ['Publisher', (p) => <Link className="font-mono font-medium no-underline" to={`${base}/publishers/${seg(p.name)}`}>{p.name}</Link>],
            ['Trusted publishing', (p) => (p.github.length ? p.github.map((g) => g.workflow).join(', ') : <span className="text-ink-muted">none</span>)],
            ['Added', (p) => when(p.created_at)],
          ]}
        />
      )}
      <Pager page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} />
    </>
  )
}

/** One publisher, its state kept per publisher: a key shown once never shows on another's page. */
export function PublisherPage() {
  const { name = '' } = useParams()
  return <PublisherView key={name} name={name} />
}

/** One publisher: its GitHub workflows, API keys and grants. */
function PublisherView({ name }: { name: string }) {
  const { client, api, base } = useTenant()
  const nav = useNavigate()
  const path = `${api}/publishers/${seg(name)}`
  const pub = useLoad(() => client.get<Publisher>(path), [path])
  const [removing, setRemoving] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  if (pub.loading && !pub.data) return <Loading />
  if (pub.error) return pub.error instanceof ApiError && pub.error.status === 404 ? <Empty>No such publisher.</Empty> : <ErrorBox error={pub.error} />
  const p = pub.data!.data
  const remove = async () => {
    setRemoving(false)
    try {
      await client.del(path)
      nav(`${base}/publishers`)
    } catch (e) {
      setFailure(e)
    }
  }
  return (
    <>
      <PageTitle eyebrow={<Link to={`${base}/publishers`} className="no-underline">Publishers</Link>} title={<span className="font-mono">{p.name}</span>}>
        <span className="pb-1 text-ink-muted">publishes from CI, never signs in here</span>
        <span className="ml-auto"><Button tone="danger-outline" onClick={() => setRemoving(true)}>Remove…</Button></span>
      </PageTitle>
      {failure != null && <ErrorBox error={failure} />}
      <Workflows publisher={p} onChanged={pub.reload} />
      <Keys publisher={p.name} />
      <PublisherGrants publisher={p.name} />
      <Confirm open={removing} title={`Remove the publisher ${p.name}?`} confirm="Remove" danger onConfirm={remove} onClose={() => setRemoving(false)}>
        <p>Its workflows, API keys and grants go with it: CI using them can no longer publish.</p>
      </Confirm>
    </>
  )
}

function Workflows({ publisher, onChanged }: { publisher: Publisher; onChanged: () => void }) {
  const { client, api } = useTenant()
  const path = `${api}/publishers/${seg(publisher.name)}/github`
  const [adding, setAdding] = useState(false)
  const [f, setF] = useState({ owner_id: '', repository_id: '', workflow: '', ref: '', environment: '' })
  const [removing, setRemoving] = useState<GitHub | null>(null)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: e.target.value })
  const act = async (fn: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      await fn()
      setAdding(false)
    } catch (e) {
      setFailure(e)
    }
    setBusy(false)
    onChanged()
  }
  return (
    <Card title="Trusted publishing" actions={<Button tone="outline" onClick={() => setAdding(!adding)}>{adding ? 'Close' : '+ GitHub workflow'}</Button>}>
      <div className="flex flex-col gap-4">
        {adding && (
          <form className="flex flex-col gap-4" onSubmit={(e) => { e.preventDefault(); act(() => client.post(path, { body: { ...f, provider: 'github' } })) }}>
            <Note>A GitHub Actions OIDC token of this workflow publishes as this publisher. Owner and repository are numeric ids: they survive renames, names do not.</Note>
            <div className="grid grid-cols-2 gap-4">
              <Field label="Owner id"><input className="field font-mono" required inputMode="numeric" value={f.owner_id} onChange={set('owner_id')} /></Field>
              <Field label="Repository id"><input className="field font-mono" required inputMode="numeric" value={f.repository_id} onChange={set('repository_id')} /></Field>
              <Field label="Workflow" hint="owner/repo/.github/workflows/release.yml"><input className="field font-mono" required value={f.workflow} onChange={set('workflow')} /></Field>
              <Field label="Ref" hint="a pattern: refs/tags/v*; empty: any"><input className="field font-mono" value={f.ref} onChange={set('ref')} /></Field>
              <Field label="Environment" hint="empty: any"><input className="field font-mono" value={f.environment} onChange={set('environment')} /></Field>
            </div>
            <div><Button type="submit" tone="brand" disabled={busy}>Add</Button></div>
          </form>
        )}
        {failure != null && <ErrorBox error={failure} />}
        {publisher.github.length === 0 ? <Empty>No workflow publishes as {publisher.name}.</Empty> : (
          <Table
            rows={publisher.github}
            rowKey={(g) => g.id}
            columns={[
              ['Workflow', (g) => <Mono>{g.workflow}</Mono>],
              ['Repository', (g) => <span className="text-[13px] text-ink-muted">owner {g.owner_id} · repository {g.repository_id}</span>],
              ['Ref', (g) => <Mono>{g.ref || 'any'}</Mono>],
              ['Environment', (g) => <Mono>{g.environment || 'any'}</Mono>],
              ['', (g) => <Button tone="danger-outline" onClick={() => setRemoving(g)}>Remove…</Button>],
            ]}
          />
        )}
      </div>
      <Confirm open={removing !== null} title="Remove this workflow?" confirm="Remove" danger
        onConfirm={() => { const g = removing!; setRemoving(null); act(() => client.del(`${path}/${seg(g.id)}`)) }} onClose={() => setRemoving(null)}>
        <p>{removing && <Mono>{removing.workflow}</Mono>} can no longer publish as {publisher.name}.</p>
      </Confirm>
    </Card>
  )
}

function Keys({ publisher }: { publisher: string }) {
  const { client, api } = useTenant()
  const path = `${api}/publishers/${seg(publisher)}/keys`
  const keys = useLoad(() => client.get<{ keys: APIKey[] }>(path), [path])
  const [days, setDays] = useState(90)
  const [issued, setIssued] = useState<APIKey | null>(null)
  const [revoking, setRevoking] = useState<APIKey | null>(null)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const issue = async () => {
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      const expires = new Date(Date.now() + days * 86_400_000).toISOString().replace(/\.\d+Z$/, 'Z')
      const r = await client.post<APIKey>(path, { body: { expires_at: expires } })
      setIssued(r.data)
    } catch (e) {
      setFailure(e)
    }
    setBusy(false)
    keys.reload()
  }
  const revoke = async () => {
    const k = revoking!
    setRevoking(null)
    try {
      await client.del(`${path}/${seg(k.id)}`)
    } catch (e) {
      setFailure(e)
    }
    keys.reload()
  }
  return (
    <Card title="API keys" actions={
      <span className="flex items-center gap-2">
        <label className="flex items-center gap-2 text-[13px] text-ink-muted">valid for
          <select className="field py-1 text-[13px]" value={days} onChange={(e) => setDays(Number(e.target.value))}>
            {[30, 90, 180, 365].map((d) => <option key={d} value={d}>{d} days</option>)}
          </select>
        </label>
        <Button tone="outline" disabled={busy || !!issued} onClick={issue}>+ API key</Button>
      </span>}>
      <div className="flex flex-col gap-4">
        {issued?.key && (
          <div role="status" className="flex flex-col gap-2 rounded-md border border-success bg-success-soft p-4">
            <strong>The key, shown this once: copy it now.</strong>
            <Mono>{issued.key}</Mono>
            <div><Button onClick={() => setIssued(null)}>I have copied it</Button></div>
          </div>
        )}
        {failure != null && <ErrorBox error={failure} />}
        {keys.error ? <ErrorBox error={keys.error} /> : keys.loading && !keys.data ? <Loading /> : keys.data!.data.keys.length === 0 ? <Empty>No API keys.</Empty> : (
          <Table
            rows={keys.data!.data.keys}
            rowKey={(k) => k.id}
            columns={[
              ['Key', (k) => <Mono>{k.prefix}…</Mono>],
              ['Expires', (k) => when(k.expires_at)],
              ['Last used', (k) => (k.last_used_at ? when(k.last_used_at) : <span className="text-ink-muted">never</span>)],
              ['', (k) => <Button tone="danger-outline" onClick={() => setRevoking(k)}>Revoke…</Button>],
            ]}
          />
        )}
      </div>
      <Confirm open={revoking !== null} title="Revoke this key?" confirm="Revoke" danger onConfirm={revoke} onClose={() => setRevoking(null)}>
        <p>{revoking && <Mono>{revoking.prefix}…</Mono>} stops working at once.</p>
      </Confirm>
    </Card>
  )
}

function PublisherGrants({ publisher }: { publisher: string }) {
  const principal = `publisher:${publisher}`
  const [granting, setGranting] = useState(false)
  const list = useGrants({ principal })
  return (
    <Card title="What it may publish" actions={<Button tone="outline" onClick={() => setGranting(!granting)}>{granting ? 'Close' : 'Grant'}</Button>}>
      <div className="flex flex-col gap-4">
        {granting && (
          <GrantForm principal={principal} only={['publish', 'promote']} targets={['extension_group', 'extension', 'channel']}
            onDone={() => { setGranting(false); list.reload() }} />
        )}
        <Note>A publisher is granted publish and promote only: on extensions, extension groups or channels.</Note>
        <GrantsList list={list} empty="Nothing granted: it can publish nothing yet." />
      </div>
    </Card>
  )
}
