import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { Badge, Button, Card, Confirm, ErrorBox, Loading, Mono, when } from '../components/ui'
import { useLoad, useTenant } from '../lib/context'
import { ApiError, seg } from '../lib/http'
import type { Release as R } from './types'

type Change = 'yank' | 'deprecate' | 'activate' | 'current' | 'public' | 'private' | 'purge'

/** Which changes a release in a state allows (the API decides; this only offers them). */
function changes(r: R): Change[] {
  switch (r.state) {
    case 'active':
      return ['current', r.visibility === 'public' ? 'private' : 'public', 'deprecate', 'yank']
    case 'deprecated':
      return ['activate', r.visibility === 'public' ? 'private' : 'public', 'yank']
    case 'yanked':
      return ['purge']
  }
  return []
}

const explain: Record<Change, string> = {
  yank: 'A yanked release is served nowhere. Yanking cannot be undone: a fix is a new version.',
  deprecate: 'A deprecated release is served on its versioned path only, never as current.',
  activate: 'The release is active again.',
  current: 'The release becomes current: served on the flat path for its DuckDB versions and platform.',
  public: 'Anyone may install the release.',
  private: 'Only callers granted install may install the release.',
  purge:
    'The release and its signatures are deleted for good. Its slot stays taken: this version, platform and build can never be released again in this channel, with this body or another.',
}

export function Release() {
  const { c = '', ext = '', id = '' } = useParams()
  const { client, api, base } = useTenant()
  const nav = useNavigate()
  const path = `${api}/channels/${seg(c)}/extensions/${seg(ext)}/releases/${seg(id)}`
  const { data, error, loading, reload } = useLoad(() => client.get<R>(path), [path])
  const [pending, setPending] = useState<Change | null>(null)
  const [typed, setTyped] = useState('')
  const [failure, setFailure] = useState<unknown>(null)
  if (loading && !data) return <Loading />
  if (error) return <ErrorBox error={error} />
  const r = data!.data
  const etag = data!.etag || r.etag
  const run = async () => {
    const ch = pending
    setPending(null)
    setTyped('')
    setFailure(null)
    try {
      if (ch === 'purge') {
        await client.del(path, { ifMatch: etag })
        nav(`${base}/channels/${seg(c)}`)
        return
      }
      await client.post(`${path}/${ch}`, { ifMatch: etag })
    } catch (e) {
      setFailure(e instanceof ApiError && e.status === 412 ? new Error('The release changed meanwhile: it was read again.') : e)
    }
    reload()
  }
  const facts: [string, React.ReactNode][] = [
    ['Version', r.version],
    ['Platform', <Mono>{r.platform}</Mono>],
    ['Slot', <Mono>{r.slot}</Mono>],
    ['Build', <Mono>{r.abi} {r.build_duckdb_version ?? r.build_c_api ?? ''}</Mono>],
    ['Body hash', <Mono>{r.body_hash}</Mono>],
    ['State', <Badge>{r.state}</Badge>],
    ['Visibility', <Badge>{r.visibility}</Badge>],
    // the highest active one among its DuckDB versions and platform is served as current
    ['Current-eligible', r.seq > 0 ? `yes (#${r.seq})` : 'no: versioned path only'],
    ['Origin', r.origin],
    ['Created', `${when(r.created_at)}${r.created_by ? ` by ${r.created_by}` : ''}`],
    ['Changed', r.state_changed_at ? `${when(r.state_changed_at)}${r.state_changed_by ? ` by ${r.state_changed_by}` : ''}` : ''],
  ]
  if (r.shadows) facts.push(['Shadows', r.shadows])
  return (
    <div className="space-y-4">
      <div className="flex items-center gap-3">
        <Link to={`${base}/channels/${seg(c)}`} className="text-sm text-brand-strong hover:underline">← {c}</Link>
        <h1 className="text-2xl font-bold">{r.name} {r.version}</h1>
      </div>
      {failure != null && <ErrorBox error={failure} />}
      <Card
        title="Release"
        actions={changes(r).map((ch) => (
          <Button key={ch} tone={ch === 'purge' || ch === 'yank' ? 'danger' : 'plain'} onClick={() => setPending(ch)}>
            {ch}
          </Button>
        ))}
      >
        <dl className="grid grid-cols-[10rem_1fr] gap-x-4 gap-y-2 text-sm">
          {facts.map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-ink-muted">{k}</dt>
              <dd>{v}</dd>
            </div>
          ))}
        </dl>
      </Card>
      {r.provenance != null && (
        <Card title="Provenance">
          <pre className="overflow-x-auto rounded-xl bg-row p-3 font-mono text-xs">{JSON.stringify(r.provenance, null, 2)}</pre>
        </Card>
      )}
      <Confirm
        open={pending !== null}
        title={`${pending ?? ''} ${r.name} ${r.version} (${r.platform})`}
        confirm={pending ?? ''}
        danger={pending === 'purge' || pending === 'yank'}
        disabled={pending === 'purge' && typed !== r.name}
        onConfirm={run}
        onClose={() => {
          setPending(null)
          setTyped('')
        }}
      >
        <p>{pending ? explain[pending] : ''}</p>
        {pending === 'purge' && (
          <label className="block">
            Type <Mono>{r.name}</Mono> to confirm:
            <input data-autofocus aria-label="confirm name" className="mt-1 w-full rounded-full border border-line bg-surface px-3 py-1" value={typed} onChange={(e) => setTyped(e.target.value)} />
          </label>
        )}
      </Confirm>
    </div>
  )
}
