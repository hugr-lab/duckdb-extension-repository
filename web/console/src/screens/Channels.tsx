import { Link } from 'react-router-dom'
import { Badge, Empty, ErrorBox, Loading, Mono, PageTitle, Table } from '../components/ui'
import { useLoad, useTenant } from '../lib/context'
import { seg } from '../lib/http'

interface Channel {
  name: string
  kind: string
}

export function Channels() {
  const { client, api, perms, base } = useTenant()
  const { data, error, loading } = useLoad(async () => {
    const chs = (await client.get<{ channels: Channel[] }>(`${api}/channels`)).data.channels.filter((c) => perms.anyIn(c.name))
    // each channel's DuckDB versions, from its (public) detail
    const details = await Promise.all(
      chs.map((c) => client.get<{ duckdb_versions?: { duckdb_version: string }[] }>(`${api}/channels/${seg(c.name)}`).then((r) => r.data, () => ({}))),
    )
    return chs.map((c, i) => ({ ...c, versions: (details[i] as { duckdb_versions?: { duckdb_version: string }[] }).duckdb_versions?.map((v) => v.duckdb_version) ?? [] }))
  }, [api])
  if (loading) return <Loading />
  if (error) return <ErrorBox error={error} />
  const rows = data ?? []
  return (
    <>
      <PageTitle title="Channels">
        <span className="pb-1 text-ink-muted">{rows.length} {rows.length === 1 ? 'channel' : 'channels'} · each signs what it serves with its own key.</span>
      </PageTitle>
      {rows.length === 0 ? (
        <Empty>No channel you administer.</Empty>
      ) : (
        <Table
          rows={rows}
          rowKey={(c) => c.name}
          columns={[
            ['Channel', (c) => <Link className="font-mono font-medium no-underline" to={`${base}/channels/${seg(c.name)}`}>{c.name}</Link>],
            ['Kind', (c) => <Badge>{c.kind}</Badge>],
            ['DuckDB versions', (c) => <Mono>{c.versions.join(' ')}</Mono>],
            ['Your access', (c) => (perms.channelAdmin(c.name) ? 'channel administrator' : `extensions: ${perms.extensionsIn(c.name).join(', ')}`)],
          ]}
        />
      )}
    </>
  )
}
