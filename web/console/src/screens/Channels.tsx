import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { Badge, Button, Card, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, Table } from '../components/ui'
import { useLoad, useTenant } from '../lib/context'
import { seg } from '../lib/http'

interface Channel {
  name: string
  kind: string
}

export function Channels() {
  const { client, api, perms, base } = useTenant()
  const [creating, setCreating] = useState(false)
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
        {perms.tenantAdmin && <span className="ml-auto"><Button tone="brand" onClick={() => setCreating(!creating)}>{creating ? 'Close' : 'New channel'}</Button></span>}
      </PageTitle>
      {creating && <NewChannel />}
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

/** Creates a channel (spec 0007 phase 3): a signed one needs a key a server administrator adds. */
function NewChannel() {
  const { client, api, base } = useTenant()
  const nav = useNavigate()
  const [name, setName] = useState('')
  const [kind, setKind] = useState('signed')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      await client.post(`${api}/channels`, { body: { name, kind } })
      nav(`${base}/channels/${seg(name)}`)
    } catch (err) {
      setFailure(err)
    }
    setBusy(false)
  }
  return (
    <Card title="New channel">
      <form className="flex flex-col gap-4" onSubmit={submit}>
        <div className="flex flex-wrap gap-4">
          <Field label="Name" hint="a path segment: lowercase letters, digits and -"><input className="field font-mono" required pattern="[a-z0-9][a-z0-9-]{0,62}" value={name} onChange={(e) => setName(e.target.value)} /></Field>
          <Field label="Kind">
            <select className="field" value={kind} onChange={(e) => setKind(e.target.value)}><option value="signed">signed</option><option value="passthrough">passthrough</option></select>
          </Field>
        </div>
        <Note>A signed channel serves what its key signs: a server administrator adds the key. A passthrough channel serves DuckDB's core byte-identical. Give it DuckDB versions on its page.</Note>
        {failure != null && <ErrorBox error={failure} />}
        <div><Button type="submit" tone="brand" disabled={busy}>Create</Button></div>
      </form>
    </Card>
  )
}
