import { useEffect } from 'react'
import { Badge, Button, Card, ErrorBox, Loading, Mono, Table, when } from '../components/ui'
import { useLoad, useTenant } from '../lib/context'
import { seg } from '../lib/http'
import type { KeyView } from './types'

interface KeyEvent {
  id: string
  key_id: string
  from?: string
  to: string
  actor: string
  forced?: boolean
  at: string
}

/** A channel's keys (read): which sign it and serve it, and a running re-sign, refreshed every 10
 * seconds while it runs and this view is open. */
export function Keys({ channel }: { channel: string }) {
  const { client, api } = useTenant()
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
        ]}
      />
    </Card>
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
