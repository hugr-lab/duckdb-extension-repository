import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, Pager, Table, when } from '../components/ui'
import { useLoad, usePaged, useTenant } from '../lib/context'
import { ApiError, seg } from '../lib/http'
import { GrantForm, GrantsList, useGrants } from './Access'
import { HoldForm } from './Holds'
import { holdWords } from './Sources'

// Publishers (spec 0008, 0023; console phase 2a): identities that only publish and promote, from CI
// of any forge, signing their requests with an SSH key registered here. They never sign in here.

interface PublisherKey {
  id: string
  type: string
  fingerprint: string
  title: string
  public_key: string
  expires_at?: string
  created_at: string
  created_by: string
  last_used_at?: string
}

interface Publisher {
  /** its id (spec 0037): opaque; a distributor recognises its own publisher by it */
  id: string
  name: string
  created_at: string
  created_by: string
  keys: PublisherKey[]
  /** a source of licence keys (spec 0021): marked by a server administrator */
  license_keys?: boolean
  /** its hold (spec 0033): what it publishes waits this long for any channel; omitted for none */
  hold?: string
  /** a distributor's publisher's hold as its entries are held: the larger of its own and the tenant's
   * minimum for distributors' publishers (spec 0037); omitted for none */
  effective_hold?: string
  /** a distributor's publisher's distributor (spec 0033 phase B): marked by a server administrator */
  distributor?: string
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
            ['Publisher', (p) => (
              <span className="flex items-center gap-2">
                <Link className="font-mono font-medium no-underline" to={`${base}/publishers/${seg(p.name)}`}>{p.name}</Link>
                {p.license_keys && <Badge>licence keys</Badge>}
                {p.distributor && <Badge>{`distributor ${p.distributor}`}</Badge>}
              </span>
            )],
            ['Keys', (p) => (p.keys.length ? p.keys.map((k) => k.title || k.fingerprint).join(', ') : <span className="text-ink-muted">none</span>)],
            ['Hold', (p) => holdWords(p.distributor ? p.effective_hold : p.hold)],
            ['Added', (p) => when(p.created_at)],
          ]}
        />
      )}
      <Pager page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} />
      <DistributorHold onChanged={list.reload} />
    </>
  )
}

/** The tenant's minimum hold for distributors' publishers (spec 0037): an entry one publishes is held
 * for the larger of its publisher's hold and this, so a stolen distributor's credential cannot reach
 * a channel at once. Its administrators set it (a day by default); a caller who may not read it sees
 * nothing. onChanged runs after a change: the publishers' effective holds follow it. */
export function DistributorHold({ onChanged }: { onChanged?: () => void }) {
  const { client, api } = useTenant()
  const path = `${api}/distributor-hold`
  const hold = useLoad(() => client.get<{ hold: string }>(path), [path])
  if (hold.error instanceof ApiError && hold.error.status === 404) return null
  return (
    <Card title="Minimum hold for distributors' publishers">
      {hold.error != null && <ErrorBox error={hold.error} />}
      <HoldForm path={path} label="minimum hold for distributors' publishers" caption="Minimum hold" input="new minimum hold" button="Set minimum"
        current={hold.data ? hold.data.data.hold : null}
        onChanged={() => {
          hold.reload()
          onChanged?.()
        }}
        note={<>What a distributor's publisher publishes is held at least this long, whatever hold its distributor sets: within it, removing
          a stolen key with its held entries purged keeps them from every channel. Raising it holds what is held now longer; lowering it
          applies to what arrives from then on.</>} />
    </Card>
  )
}

/** One publisher, its state kept per publisher. */
export function PublisherPage() {
  const { name = '' } = useParams()
  return <PublisherView key={name} name={name} />
}

/** One publisher: its keys and grants. */
function PublisherView({ name }: { name: string }) {
  const { client, api, base, perms } = useTenant()
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
        <span className="pb-1 text-[13px] text-ink-muted">id <Mono>{p.id}</Mono></span>
        <span className="ml-auto"><Button tone="danger-outline" onClick={() => setRemoving(true)}>Remove…</Button></span>
      </PageTitle>
      {failure != null && <ErrorBox error={failure} />}
      {p.license_keys && (
        <Note>A source of licence keys: its publications record the licence keys their builds declare, which this tenant's licences verify
          against. A server administrator marked it, and only a server administrator changes its keys and grants: a change made here is refused.
          Removing it drops the keys it brought.</Note>
      )}
      {p.distributor && (
        <Note>A distributor's publisher, {p.distributor}'s: it publishes into the tenant's <Link to={`${base}/catalogue`}>catalogue</Link> only,
          without grants, and reaches no channel by itself: channels take its builds by their rules. Its keys and its hold
          ({holdWords(p.hold)}) are the distributor's: a change a tenant administrator makes here is refused. Its builds are held
          for {holdWords(p.effective_hold)}: its hold, or the tenant's minimum for distributors' publishers when that is longer.</Note>
      )}
      <Keys publisher={p} onChanged={pub.reload} />
      {(!p.distributor || perms.server) && (
        <HoldForm card="Hold" path={`${path}/hold`} label="publisher hold" current={p.hold} onChanged={pub.reload}
          note={<>What it publishes waits this long in the <Link to={`${base}/catalogue`}>catalogue</Link> before a channel's automatic
            placement takes it; a channel administrator may place it earlier by hand. Within it, removing a stolen key with its held
            entries purged keeps them from every channel. 0 clears it.</>} />
      )}
      {!p.distributor && <PublisherGrants publisher={p.name} />}
      <Confirm open={removing} title={`Remove the publisher ${p.name}?`} confirm="Remove" danger onConfirm={remove} onClose={() => setRemoving(false)}>
        <p>Its keys and grants go with it: CI signing with them can no longer publish.</p>
      </Confirm>
    </>
  )
}

function Keys({ publisher, onChanged }: { publisher: Publisher; onChanged: () => void }) {
  const { client, api } = useTenant()
  const path = `${api}/publishers/${seg(publisher.name)}/keys`
  const [adding, setAdding] = useState(false)
  const [line, setLine] = useState('')
  const [days, setDays] = useState(365)
  const [removing, setRemoving] = useState<PublisherKey | null>(null)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const act = async (fn: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      await fn()
      setAdding(false)
      setLine('')
    } catch (e) {
      setFailure(e)
    }
    setBusy(false)
    onChanged()
  }
  const add = (e: React.FormEvent) => {
    e.preventDefault()
    const body: { public_key: string; expires_at?: string } = { public_key: line.trim() }
    if (days > 0) body.expires_at = new Date(Date.now() + days * 86_400_000).toISOString().replace(/\.\d+Z$/, 'Z')
    act(() => client.post(path, { body }))
  }
  return (
    <Card title="Keys" actions={<Button tone="outline" onClick={() => setAdding(!adding)}>{adding ? 'Close' : '+ Key'}</Button>}>
      <div className="flex flex-col gap-4">
        {adding && (
          <form className="flex flex-col gap-4" onSubmit={add}>
            <Note>The public half of the SSH key CI signs its requests with: the line of its .pub file. Make one with
              <Mono>ssh-keygen -t ed25519 -N '' -C ci -f publisher_key</Mono>; the private half stays a CI secret.</Note>
            <Field label="Public key" hint="ssh-ed25519 AAAA… comment, or ecdsa-sha2-nistp256">
              <textarea className="field font-mono" required rows={2} value={line} onChange={(e) => setLine(e.target.value)} />
            </Field>
            <label className="flex items-center gap-2 text-[13px] text-ink-muted"
              title="Within the server's limit on a key's lifetime (limits.ceilings.publisher_key_lifetime, a year by default)">expires
              <select className="field py-1 text-[13px]" value={days} onChange={(e) => setDays(Number(e.target.value))}>
                {[90, 180, 365].map((d) => <option key={d} value={d}>in {d} days</option>)}
                <option value={0}>never</option>
              </select>
            </label>
            <div><Button type="submit" tone="brand" disabled={busy}>Add</Button></div>
          </form>
        )}
        {failure != null && <ErrorBox error={failure} />}
        {publisher.keys.length === 0 ? <Empty>No key signs as {publisher.name}.</Empty> : (
          <Table
            rows={publisher.keys}
            rowKey={(k) => k.id}
            columns={[
              ['Key', (k) => <span className="flex flex-col"><Mono>{k.fingerprint}</Mono><span className="text-[13px] text-ink-muted">{k.type}{k.title && ` · ${k.title}`}</span></span>],
              ['Expires', (k) => (k.expires_at ? when(k.expires_at) : <span className="text-ink-muted">never</span>)],
              ['Last used', (k) => (k.last_used_at ? when(k.last_used_at) : <span className="text-ink-muted">never</span>)],
              ['', (k) => <Button tone="danger-outline" onClick={() => setRemoving(k)}>Remove…</Button>],
            ]}
          />
        )}
      </div>
      <Confirm open={removing !== null} title="Remove this key?" confirm="Remove" danger
        onConfirm={() => { const k = removing!; setRemoving(null); act(() => client.del(`${path}/${seg(k.id)}`)) }} onClose={() => setRemoving(null)}>
        <p>{removing && <Mono>{removing.fingerprint}</Mono>} stops working at once: CI signing with it can no longer publish as {publisher.name}.</p>
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
