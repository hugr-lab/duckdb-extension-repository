import { Empty, ErrorBox, Loading, Mono, Note, PageTitle, Table } from '../components/ui'
import { useLoad, useTenant } from '../lib/context'
import { sizeText } from './BundleUploads'

// A tenant's limits (spec 0038), read only: for each, what is set, what applies and what is used. A
// server administrator sets them with the CLI or the API; growth past one is refused (429
// tenant-limit), nothing that exists is removed.

/** One limit as GET …/limits answers it: set is "default", "unlimited" or a number; limit is null
 * for unlimited bytes. */
export interface Limit {
  set: string | number
  limit: number | null
  used: number
}

const names: [string, string][] = [['bytes', 'Bytes'], ['channels', 'Channels'], ['upstreams', 'Upstreams'], ['publishers', 'Publishers']]

/** A limit's value as it reads: bytes as a size, null as unlimited. */
export function limitText(name: string, v: string | number | null): string {
  if (v === null || v === 'unlimited') return 'unlimited'
  if (typeof v === 'string') return v
  return name === 'bytes' ? sizeText(v) : String(v)
}

export function Limits() {
  const { client, api } = useTenant()
  const l = useLoad(() => client.get<{ limits: Record<string, Limit> }>(`${api}/limits`), [api])
  const rows = l.data ? names.flatMap(([n, label]) => (l.data!.data.limits[n] ? [{ n, label, ...l.data!.data.limits[n] }] : [])) : []
  return (
    <>
      <PageTitle title="Limits">
        <span className="pb-1 text-ink-muted">What this tenant may grow to; a server administrator sets them.</span>
      </PageTitle>
      {l.error ? <ErrorBox error={l.error} /> : l.loading && !l.data ? <Loading /> : rows.length === 0 ? <Empty>No limits.</Empty> : (
        <>
          <Table rows={rows} rowKey={(r) => r.n} columns={[
            ['Limit', (r) => r.label],
            ['Used', (r) => <Mono>{limitText(r.n, r.used)}</Mono>],
            ['Applies', (r) => (
              <span className={r.limit !== null && r.used >= r.limit ? 'font-semibold text-danger' : undefined}>
                <Mono>{limitText(r.n, r.limit)}</Mono>{r.limit !== null && r.used >= r.limit ? ' (reached)' : ''}
              </span>
            )],
            ['Set', (r) => <Mono>{limitText(r.n, r.set)}</Mono>],
          ]} />
          <Note>A limit reached refuses growth of its kind (a new channel, upstream or publisher; a new body); a lowered limit removes
            nothing. Bytes count each body the tenant's own sources keep once, and their icons; a distributor's builds are not
            counted.</Note>
        </>
      )}
    </>
  )
}
