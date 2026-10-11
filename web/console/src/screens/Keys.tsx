import { useEffect, useState } from 'react'
import { Badge, Button, Card, Confirm, ErrorBox, Field, Loading, Mono, Note, Table, when } from '../components/ui'
import { useLoad, useTenant } from '../lib/context'
import { ApiError, seg } from '../lib/http'
import type { Key, KeyView } from './types'

interface KeyEvent {
  id: string
  key_id: string
  from?: string
  to: string
  actor: string
  forced?: boolean
  at: string
}

/** A channel's keys: which sign it and serve it, and a running re-sign, refreshed every 10 seconds
 * while it runs and this view is open. Its administrators activate and retire trusted keys (phase
 * 2b); a server administrator adds one by signer reference, and may force. */
export function Keys({ channel }: { channel: string }) {
  const { client, api, perms } = useTenant()
  const [pending, setPending] = useState<{ key: Key; change: 'activate' | 'retire' } | null>(null)
  const [force, setForce] = useState(false)
  const [signer, setSigner] = useState('')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const { data, error, loading, reload } = useLoad(() => client.get<KeyView>(`${api}/channels/${seg(channel)}/keys`), [api, channel])
  const events = useLoad(() => client.get<{ events: KeyEvent[] }>(`${api}/channels/${seg(channel)}/keys/events?limit=50`), [api, channel])
  const working = data?.data.resigner?.working ?? false
  useEffect(() => {
    if (!working) return
    const t = setInterval(reload, 10_000)
    return () => clearInterval(t)
  }, [working, reload])
  if (loading && !data) return <Loading />
  if (error) return <ErrorBox error={error} />
  const v = data!.data
  const fp = (id: string) => v.keys.find((k) => k.id === id)?.fingerprint ?? id
  const act = async (fn: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      await fn()
      setSigner('')
    } catch (e) {
      setFailure(e instanceof ApiError && e.status === 412 ? new Error('The key changed meanwhile: it was read again.') : e)
    }
    setBusy(false)
    setForce(false)
    reload()
    events.reload()
  }
  const change = () => {
    const p = pending!
    setPending(null)
    act(() => client.post(`${api}/channels/${seg(channel)}/keys/${seg(p.key.id)}/${p.change}${force ? '?force=true' : ''}`, { ifMatch: p.key.etag }))
  }
  return (
    <div className="space-y-4">
    <Card title="Keys" actions={<Button onClick={reload}>Refresh</Button>}>
      <div className="mb-4 flex flex-wrap gap-6 text-sm">
        <span>Active: <Mono>{v.keys.find((k) => k.id === v.active_key)?.fingerprint ?? 'none'}</Mono></span>
        <span>Serving: <Mono>{v.keys.find((k) => k.id === v.serving_key)?.fingerprint ?? 'none'}</Mono></span>
        {v.unsigned_by_active != null && v.unsigned_by_active > 0 && (
          <span className="text-warning">{v.unsigned_by_active} releases not yet signed by the active key{working ? ' (re-signing)' : ''}</span>
        )}
      </div>
      <Table
        rows={v.keys}
        rowKey={(k) => k.id}
        columns={[
          ['Fingerprint', (k) => <Mono>{k.fingerprint}</Mono>],
          ['State', (k) => <Badge>{k.state}</Badge>],
          ['Trusted since', (k) => when(k.trusted_since)],
          ['Changed', (k) => `${when(k.state_changed_at)} by ${k.state_changed_by}`],
          ['', (k) => (k.state === 'trusted' ? (
            <span className="flex gap-2">
              <Button tone="outline" disabled={busy} onClick={() => setPending({ key: k, change: 'activate' })}>Activate…</Button>
              {k.id !== v.serving_key && <Button tone="danger-outline" disabled={busy} onClick={() => setPending({ key: k, change: 'retire' })}>Retire…</Button>}
            </span>
          ) : null)],
        ]}
      />
      {failure != null && <div className="mt-3"><ErrorBox error={failure} /></div>}
      {perms.server && (
        <form className="mt-4 flex items-end gap-2 border-t border-line pt-4" onSubmit={(e) => { e.preventDefault(); act(() => client.post(`${api}/channels/${seg(channel)}/keys`, { body: { signer_ref: signer, active: false } })) }}>
          <Field label="Add a key" hint="a signer reference (vault:…, kms:…, file:…); it starts trusted, activated later"><input className="field w-[28rem] font-mono" required value={signer} onChange={(e) => setSigner(e.target.value)} /></Field>
          <Button type="submit" disabled={busy}>Add key</Button>
        </form>
      )}
    </Card>
    <Confirm open={pending !== null} title={`${pending?.change ?? ''} ${pending?.key.fingerprint ?? ''}`} confirm={pending?.change ?? ''}
      danger={pending?.change === 'retire'} onConfirm={change} onClose={() => { setPending(null); setForce(false) }}>
      <p>{pending?.change === 'activate'
        ? 'New releases are signed with this key, and a re-sign signs the existing ones; the serving key moves once every release carries its signature.'
        : 'A retired key never comes back: clients stop trusting it. The API refuses while a release still needs it.'}</p>
      {perms.server && (
        <label className="flex items-center gap-2 text-[13px]"><input type="checkbox" checked={force} onChange={(e) => setForce(e.target.checked)} /> force (server administrators): skip the API's safety checks</label>
      )}
      {force && <Note>Forcing can leave clients without a key they trust for some releases.</Note>}
    </Confirm>
    <Card title="Key events">
      {events.loading ? <Loading /> : events.error ? <ErrorBox error={events.error} /> : (
        <Table
          rows={events.data!.data.events}
          rowKey={(e) => e.id}
          columns={[
            ['At', (e) => when(e.at)],
            ['Key', (e) => <Mono>{fp(e.key_id)}</Mono>],
            ['Change', (e) => `${e.from ?? 'new'} → ${e.to}${e.forced ? ' (forced)' : ''}`],
            ['By', (e) => <Mono>{e.actor}</Mono>],
          ]}
        />
      )}
    </Card>
    </div>
  )
}
