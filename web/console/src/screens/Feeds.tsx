import { useState } from 'react'
import { Badge, Button, Card, Checks, Chips, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, Table, when } from '../components/ui'
import { useAction } from '../lib/action'
import { useLoad, useTenant } from '../lib/context'
import { seg } from '../lib/http'
import { entryNames } from './Upstreams'

// Feeds (spec 0011 phase 1b): the other repositories whose signed statements about the bodies the
// tenant holds it subscribes to: an organisation's kista taking Enterest's reviews, or a public
// kista's.

export interface Feed {
  name: string
  url: string
  credential: string
  keys: string[]
  kinds: string[]
  readers: boolean
  offer: 'upstreams' | 'channels'
  channels?: string[]
  state: string
  status: 'new' | 'ok' | 'unauthorized' | 'failing'
  last_error?: string
  last_success_at?: string
  last_run_at?: string
  last_run?: { counts?: Record<string, number>; refused?: { issuer_id?: string; reason: string }[]; error?: string }
  run_requested: boolean
  etag: string
}

export const feedKinds = ['sbom', 'vulnerabilities', 'review', 'provenance', 'notes']

export interface FeedForm {
  name: string
  url: string
  credential: string
  keys: string
  kinds: string[]
  readers: boolean
  offer: 'upstreams' | 'channels'
  channels: string[]
}

/** The body of POST …/feeds: every kind is none named; channels only for the channels offer. */
export function feedBody(f: FeedForm) {
  const body: Record<string, unknown> = { name: f.name, url: f.url, credential: f.credential, keys: entryNames(f.keys), readers: f.readers,
    offer: f.offer }
  if (f.kinds.length > 0 && f.kinds.length < feedKinds.length) body.kinds = f.kinds
  if (f.offer === 'channels') body.channels = f.channels
  return body
}

const statusWords: Record<Feed['status'], string> = { new: 'not run yet', ok: 'ok', unauthorized: 'credential refused', failing: 'failing' }

/** A run's counts, those above zero, in a line. */
export function feedCounts(f: Feed) {
  return Object.entries(f.last_run?.counts ?? {}).filter(([k, n]) => n > 0 && k !== 'offered').map(([k, n]) => `${k} ${n}`).join(' · ')
}

export function Feeds() {
  const { client, api } = useTenant()
  const { data, error, loading, reload } = useLoad(() => client.get<{ feeds: Feed[] }>(`${api}/feeds`), [api])
  const [adding, setAdding] = useState(false)
  const [removing, setRemoving] = useState<Feed | null>(null)
  const [documents, setDocuments] = useState(false)
  const { busy, failure, act } = useAction('The feed', reload)
  const feeds = data?.data.feeds ?? []
  const change = (f: Feed, body: Record<string, unknown>) => act(() => client.post(`${api}/feeds/${seg(f.name)}`, { body, ifMatch: f.etag })).then(reload)
  return (
    <Card title="Feeds" actions={<Button tone="outline" onClick={() => setAdding(!adding)}>{adding ? 'Close' : 'New feed'}</Button>}>
      <div className="flex flex-col gap-3">
        <Note>Another repository's signed statements about the bodies this tenant holds: reviews, scans, SBOMs. A statement is accepted only
          under a pinned key, for a body this tenant offered and still holds.</Note>
        {adding && <NewFeed onDone={() => { setAdding(false); reload() }} />}
        {failure != null && <ErrorBox error={failure} />}
        {error ? <ErrorBox error={error} /> : loading && !data ? <Loading /> : feeds.length === 0 ? <Empty>No feeds.</Empty> : (
          <Table rows={feeds} rowKey={(f) => f.name} columns={[
            ['Feed', (f) => <Mono>{f.name}</Mono>],
            ['Source', (f) => <Mono>{f.url}</Mono>],
            ['Takes', (f) => (
              <span className="text-[13px]">
                {f.kinds.length === 0 ? 'every kind' : f.kinds.join(', ')}
                <div className="text-ink-muted">{f.offer === 'channels' ? `bodies in ${(f.channels ?? []).join(', ')}` : 'mirrored bodies'}
                  {f.readers ? ' · readers may see' : ''}</div>
              </span>
            )],
            ['Last run', (f) => (
              <span className="text-[13px]">
                {f.last_run_at ? when(f.last_run_at) : <span className="text-ink-muted">never</span>}
                <div className="text-ink-muted">{feedCounts(f)}</div>
                {f.last_error && <div className="text-danger break-words">{f.last_error}</div>}
                {f.run_requested && <div className="text-warning">a sync is requested</div>}
              </span>
            )],
            ['State', (f) => <span className="flex flex-wrap gap-1"><Badge>{f.state}</Badge><Badge>{statusWords[f.status] ?? f.status}</Badge></span>],
            ['', (f) => (
              <span className="flex flex-wrap gap-1.5">
                <Button disabled={busy || f.state !== 'active'} onClick={() => act(() => client.post(`${api}/feeds/${seg(f.name)}/sync`)).then(reload)}>Sync</Button>
                <Button tone="outline" disabled={busy} onClick={() => change(f, { state: f.state === 'active' ? 'paused' : 'active' })}>
                  {f.state === 'active' ? 'Pause' : 'Resume'}
                </Button>
                <Button tone="danger-outline" disabled={busy} onClick={() => { setDocuments(false); setRemoving(f) }}>Remove…</Button>
              </span>
            )],
          ]} />
        )}
      </div>
      <Confirm open={removing !== null} title={`Remove the feed ${removing?.name ?? ''}?`} confirm="Remove" danger
        onConfirm={() => {
          const f = removing!
          setRemoving(null)
          act(() => client.del(`${api}/feeds/${seg(f.name)}${documents ? '?documents=true' : ''}`, { ifMatch: f.etag })).then(reload)
        }}
        onClose={() => setRemoving(null)}>
        <p>No more statements are taken from it.</p>
        <label className="mt-2 flex items-center gap-2">
          <input type="checkbox" checked={documents} onChange={(e) => setDocuments(e.target.checked)} />
          Remove the documents it brought too
        </label>
      </Confirm>
    </Card>
  )
}

function NewFeed({ onDone }: { onDone: () => void }) {
  const { client, api } = useTenant()
  const channels = useLoad(() => client.get<{ channels: { name: string }[] }>(`${api}/channels`), [api])
  const [f, setF] = useState<FeedForm>({ name: '', url: '', credential: '', keys: '', kinds: [...feedKinds], readers: false, offer: 'upstreams', channels: [] })
  const { busy, failure, act } = useAction()
  const set = (k: 'name' | 'url' | 'credential' | 'keys') => (e: React.ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: e.target.value })
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (await act(() => client.post(`${api}/feeds`, { body: feedBody(f) }))) onDone()
  }
  return (
    <form onSubmit={submit} className="flex flex-col gap-4 rounded-md border border-line p-4">
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        <Field label="Name"><input className="field font-mono" required pattern="[a-z0-9][a-z0-9-]{0,62}" value={f.name} onChange={set('name')} /></Field>
        <Field label="Source URL" hint="https://<host>/api/v1/tenants/<tenant>: the source tenant, the identity its statements name">
          <input className="field font-mono" required type="url" value={f.url} onChange={set('url')} />
        </Field>
        <Field label="Credential" hint="a configured credential's name; its principal holds feed at the source">
          <input className="field font-mono" required value={f.credential} onChange={set('credential')} />
        </Field>
        <Field label="Pinned keys" hint="the source's attestation key fingerprints (sha256:…), separated by commas">
          <input className="field font-mono" required value={f.keys} onChange={set('keys')} />
        </Field>
      </div>
      <div className="flex flex-col gap-1">
        <span className="text-[13px] font-semibold text-ink">Kinds taken</span>
        <Checks label="kinds" options={feedKinds} value={f.kinds} onChange={(kinds) => setF({ ...f, kinds })} />
      </div>
      <div className="flex flex-col gap-1">
        <span className="text-[13px] font-semibold text-ink">Bodies offered to the source</span>
        <Chips label="offer" value={f.offer} options={[['upstreams', 'Mirrored from upstreams'], ['channels', 'Released in channels']]}
          onChange={(offer) => setF({ ...f, offer })} />
        {f.offer === 'channels' && (
          <Checks label="channels" options={(channels.data?.data.channels ?? []).map((c) => c.name)} value={f.channels}
            onChange={(chs) => setF({ ...f, channels: chs })} />
        )}
        <Note>The source learns which of these bodies this tenant holds. Mirrored bodies keep the tenant's own builds out of it.</Note>
      </div>
      <label className="flex items-center gap-2 text-[13px]">
        <input type="checkbox" checked={f.readers} onChange={(e) => setF({ ...f, readers: e.target.checked })} />
        Show to readers the documents the source lets subscribers show (a paid review shown on a public release is published to anyone)
      </label>
      {failure != null && <ErrorBox error={failure} />}
      <div><Button type="submit" tone="brand" disabled={busy || f.kinds.length === 0 || f.offer === 'channels' && f.channels.length === 0}>Add</Button></div>
    </form>
  )
}
