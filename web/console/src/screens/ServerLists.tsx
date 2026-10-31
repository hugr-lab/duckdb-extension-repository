import { useState } from 'react'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, SearchBox, Table, when } from '../components/ui'
import { useLoad } from '../lib/context'
import { useAction } from '../lib/action'
import { ApiError, type Client, seg } from '../lib/http'

// The server's lists (server administrators): the reserved extension names (spec 0030), and the
// upstream credentials configured on the server (spec 0009), whose settings are never shown.

export interface ReservedName {
  name: string
  kind: 'core' | 'community'
  added_at: string
  /** builtin, version:<v>@<commit>, upstream:<tenant>/<upstream>, bundle:<id>, admin:<actor>,
   * community-sync@<commit> */
  sources?: string[]
}

/** Whether a reserved name is an administrator's alone, so DELETE removes it (a name a version, an
 * upstream or the seed brought answers 409). */
export function removable(n: Pick<ReservedName, 'sources'>) {
  const s = n.sources ?? []
  return s.length > 0 && s.every((x) => x.startsWith('admin:'))
}

/** The names typed in a form: split on commas and white space, without duplicates. */
export function splitNames(s: string) {
  return [...new Set(s.split(/[\s,]+/).map((x) => x.trim()).filter((x) => x !== ''))]
}

/** POST reserved-names' answer: the names newly reserved, and by name the tenants that keep
 * releases of it. */
export interface Reserved {
  added: string[]
  tenants?: Record<string, string[]>
}

/** The tenants keeping releases, by name, in words ("geo: acme, beta"). */
export function keptWords(t?: Record<string, string[]>) {
  return Object.keys(t ?? {}).sort().map((n) => `${n}: ${t![n].join(', ')}`).join('; ')
}

/** The reserve request: force only while it is offered (after a 409), so it is never sent stale. */
export function reserveBody(names: string, kind: 'core' | 'community', force: boolean, offered: boolean) {
  return { names: splitNames(names), kind, force: offered && force }
}

export function ReservedNames({ client }: { client: Client }) {
  const list = useLoad(() => client.get<{ names: ReservedName[] }>('/reserved-names'), [])
  const [prefix, setPrefix] = useState('')
  const [names, setNames] = useState('')
  const [kind, setKind] = useState<'core' | 'community'>('community')
  const [force, setForce] = useState(false)
  const [removing, setRemoving] = useState('')
  const [added, setAdded] = useState<Reserved | null>(null)
  const reserve = useAction('The reserved names', list.reload)
  const remove = useAction('The reserved name', list.reload)
  const busy = reserve.busy || remove.busy
  const conflict = reserve.failure instanceof ApiError && reserve.failure.status === 409
  const rows = (list.data?.data.names ?? []).filter((n) => n.name.startsWith(prefix))
  return (
    <>
      <PageTitle title="Reserved names">
        <span className="pb-1 text-ink-muted">Extension names DuckDB owns: a tenant reaches them only through grants that name them.</span>
      </PageTitle>
      <Card title="Reserve names">
        <form className="flex flex-wrap items-end gap-2" onSubmit={(e) => {
          e.preventDefault()
          const body = reserveBody(names, kind, force, conflict)
          setAdded(null)
          setForce(false) // offered again only by the next 409
          reserve.act(async () => {
            const r = await client.post<Reserved>('/reserved-names', { body })
            setAdded(r.data)
          }).then((ok) => {
            if (ok) {
              setNames('')
              list.reload()
            }
          })
        }}>
          <Field label="Names" hint="separated by commas or spaces"><input className="field w-[24rem] font-mono" required value={names} onChange={(e) => setNames(e.target.value)} /></Field>
          <Field label="Kind">
            <select className="field" value={kind} onChange={(e) => setKind(e.target.value as 'core' | 'community')}>
              <option value="community">community</option>
              <option value="core">core</option>
            </select>
          </Field>
          <Button type="submit" tone="brand" disabled={busy}>Reserve</Button>
        </form>
        {conflict && (
          <label className="mt-3 flex items-center gap-2 text-[13px]">
            <input type="checkbox" checked={force} onChange={(e) => setForce(e.target.checked)} /> force: reserve them though tenants have releases of
            them (the releases stay; only grants naming the extension reach them from then on)
          </label>
        )}
        {reserve.failure != null && <div className="mt-3"><ErrorBox error={reserve.failure} /></div>}
        {added && (
          <p className="m-0 mt-3 text-[13px] text-ink-muted" role="status">
            Reserved: {added.added.length === 0 ? 'nothing new' : added.added.join(', ')}
            {keptWords(added.tenants) && <> · tenants keeping releases: {keptWords(added.tenants)}</>}
          </p>
        )}
      </Card>
      <div role="search" className="flex flex-wrap items-center gap-2">
        <SearchBox label="Find a reserved name" value={prefix} onChange={setPrefix} />
        <span className="ml-auto"><Button onClick={list.reload}>Refresh</Button></span>
      </div>
      {remove.failure != null && <ErrorBox error={remove.failure} />}
      {list.error ? <ErrorBox error={list.error} /> : !list.data ? <Loading /> : rows.length === 0 ? <Empty>{prefix ? 'Nothing matches.' : 'No reserved names.'}</Empty> : (
        <Table
          rows={rows}
          rowKey={(n) => n.name}
          columns={[
            ['Name', (n) => <Mono>{n.name}</Mono>],
            ['Kind', (n) => <Badge>{n.kind}</Badge>],
            ['Sources', (n) => <span className="flex flex-wrap gap-1">{(n.sources ?? []).map((s) => <span key={s} className="chip border border-line text-ink"><Mono>{s}</Mono></span>)}</span>],
            ['Added', (n) => <span className="text-[13px] text-ink-muted">{when(n.added_at)}</span>],
            ['', (n) => (removable(n) ? <Button tone="danger-outline" disabled={busy} onClick={() => { remove.setFailure(null); setRemoving(n.name) }}>Remove…</Button> : '')],
          ]}
        />
      )}
      <Confirm open={removing !== ''} title={`Remove ${removing}?`} confirm="Remove" danger onClose={() => setRemoving('')}
        onConfirm={() => {
          const n = removing
          setRemoving('')
          remove.act(() => client.del(`/reserved-names/${seg(n)}`)).then((ok) => ok && list.reload())
        }}>
        <p>The name is no longer reserved: tenants' grants on every extension reach it again. Only a name administrators reserved can be
          removed.</p>
      </Confirm>
    </>
  )
}

export interface Credential {
  name: string
  kind: string
  tenants: string[]
  prefixes: string[]
  allow_public: boolean
  forwards_usage: { tenant: string; upstream: string }[]
}

export function Credentials({ client }: { client: Client }) {
  const list = useLoad(() => client.get<{ credentials: Credential[] }>('/credentials'), [])
  return (
    <>
      <PageTitle title="Upstream credentials">
        <span className="pb-1 text-ink-muted">Configured on the server (credentials:), never shown: names, the tenants that may use each, and the prefixes
          it is sent to.</span>
      </PageTitle>
      {list.error ? <ErrorBox error={list.error} /> : !list.data ? <Loading /> : list.data.data.credentials.length === 0 ? <Empty>No credentials configured.</Empty> : (
        <Table
          rows={list.data.data.credentials}
          rowKey={(c) => c.name}
          columns={[
            ['Name', (c) => <Mono>{c.name}</Mono>],
            ['Kind', (c) => <Badge>{c.kind}</Badge>],
            ['Tenants', (c) => (c.tenants.includes('*') ? <span className="text-ink-muted">every tenant</span> : c.tenants.join(', '))],
            ['Prefixes', (c) => <span className="flex flex-col">{c.prefixes.map((p) => <Mono key={p}>{p}</Mono>)}</span>],
            ['Public releases', (c) => (c.allow_public ? 'allowed' : 'no')],
            ['Forwards usage', (c) => (c.forwards_usage.length === 0 ? '' : c.forwards_usage.map((f) => `${f.tenant}/${f.upstream}`).join(', '))],
          ]}
        />
      )}
      <Note>A credential is sent only to its prefixes, for the tenants it lists; a repository upstream names it. Its secret stays in the
        server's configuration.</Note>
    </>
  )
}
