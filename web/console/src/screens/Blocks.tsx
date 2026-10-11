import { useState } from 'react'
import { Button, Card, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, Pager, Table, when } from '../components/ui'
import { usePaged, useTenant } from '../lib/context'
import { seg } from '../lib/http'

// Blocks (spec 0008; console phase 2b): bodies the tenant refuses, by hash. Blocking yanks their
// releases in every channel; publication and upstream intake refuse them from then on.

interface Block {
  body_hash: string
  reason: string
  created_at: string
  created_by: string
}

export function Blocks() {
  const { client, api } = useTenant()
  const [hash, setHash] = useState('')
  const [reason, setReason] = useState('')
  const [confirming, setConfirming] = useState(false)
  const [unblocking, setUnblocking] = useState('')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const list = usePaged<Block>(async (cursor, limit) => {
    const q = new URLSearchParams({ limit: String(limit) })
    if (cursor) q.set('cursor', cursor)
    const r = await client.get<{ blocks: Block[]; next?: string }>(`${api}/blocks?${q}`)
    return { items: r.data.blocks, next: r.data.next }
  }, [api])
  const act = async (fn: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      await fn()
      setHash('')
      setReason('')
    } catch (e) {
      setFailure(e)
    }
    setBusy(false)
    list.reload()
  }
  return (
    <>
      <PageTitle title="Blocks">
        <span className="pb-1 text-ink-muted">Bodies this tenant refuses, by their hash.</span>
      </PageTitle>
      <Card title="Block a body">
        <form className="flex flex-col gap-4" onSubmit={(e) => { e.preventDefault(); setConfirming(true) }}>
          <div className="grid grid-cols-[1fr_1fr] gap-4">
            <Field label="Body hash" hint="a release's body hash, from its page"><input className="field font-mono" required value={hash} onChange={(e) => setHash(e.target.value.trim())} /></Field>
            <Field label="Reason"><input className="field" required maxLength={400} value={reason} onChange={(e) => setReason(e.target.value)} /></Field>
          </div>
          <Note>Blocking yanks every release with this body, in every channel, and refuses it from publishers and upstreams from then on.</Note>
          <div><Button type="submit" tone="danger" disabled={busy}>Block…</Button></div>
        </form>
      </Card>
      {failure != null && <ErrorBox error={failure} />}
      {list.error ? <ErrorBox error={list.error} /> : list.loading && !list.data ? <Loading /> : list.items.length === 0 ? <Empty>Nothing blocked.</Empty> : (
        <Table rows={list.items} rowKey={(b) => b.body_hash} columns={[
          ['Body hash', (b) => <Mono>{b.body_hash}</Mono>],
          ['Reason', (b) => b.reason],
          ['Blocked', (b) => <span className="text-[13px] text-ink-muted">{when(b.created_at)} by {b.created_by}</span>],
          ['', (b) => <Button tone="outline" disabled={busy} onClick={() => setUnblocking(b.body_hash)}>Unblock…</Button>],
        ]} />
      )}
      <Pager page={list.page} size={list.size} count={list.items.length} hasNext={list.hasNext} onPrev={list.prev} onNext={list.next} onSize={list.setSize} />
      <Confirm open={confirming} title="Block this body?" confirm="Block" danger
        onConfirm={() => { setConfirming(false); act(() => client.post(`${api}/blocks`, { body: { body_hash: hash, reason } })) }} onClose={() => setConfirming(false)}>
        <p><Mono>{hash}</Mono></p>
        <p>Its releases are yanked in every channel; yanking cannot be undone, even by unblocking.</p>
      </Confirm>
      <Confirm open={unblocking !== ''} title="Unblock this body?" confirm="Unblock"
        onConfirm={() => { const h = unblocking; setUnblocking(''); act(() => client.del(`${api}/blocks/${seg(h)}`)) }} onClose={() => setUnblocking('')}>
        <p>It may be published or mirrored again. Releases already yanked stay yanked.</p>
      </Confirm>
    </>
  )
}
