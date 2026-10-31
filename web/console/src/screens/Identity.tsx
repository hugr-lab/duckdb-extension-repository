import { useState } from 'react'
import { Button, Card, Confirm, Empty, ErrorBox, Field, Loading, Mono, Note, PageTitle, when } from '../components/ui'
import { useLoad, useTenant } from '../lib/context'
import { ApiError, seg } from '../lib/http'

// Identity (spec 0015 phase 2a): the tenant's issuer records (spec 0006), their console clients
// (spec 0015), and its audiences.

interface Issuer {
  name: string
  url: string
  jwks_uri?: string
  algorithms?: string[]
  required_claims?: Record<string, string>
  roles_claim?: string[]
  groups_claim?: string[]
  client_claim?: string[]
  max_token_lifetime?: string
  created_at?: string
  created_by?: string
  etag?: string
}

interface ConsoleIssuer {
  name: string
  client_id: string
  scopes: string[]
  audience_parameter: string
  audience: string
}

/** A claim path as typed: keys joined by dots, or a JSON array of keys when a key has dots. */
export function claimPath(s: string): string[] | undefined {
  const t = s.trim()
  if (!t) return undefined
  if (!t.startsWith('[')) return t.split('.')
  let v: unknown
  try {
    v = JSON.parse(t)
  } catch {
    v = null
  }
  if (!Array.isArray(v) || v.length === 0 || !v.every((k) => typeof k === 'string' && k !== '')) {
    throw new Error(`A claim path is keys joined by dots, or a JSON array of keys: ${t}`)
  }
  return v as string[]
}

/** Required claims as typed: claim=value, separated by commas. */
export function requiredClaims(s: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const pair of s.split(',').map((x) => x.trim()).filter(Boolean)) {
    const i = pair.indexOf('=')
    if (i <= 0) throw new Error(`A required claim is claim=value: ${pair}`)
    out[pair.slice(0, i)] = pair.slice(i + 1)
  }
  return out
}

const showPath = (p?: string[]) => (!p || p.length === 0 ? '' : p.some((k) => k.includes('.')) ? JSON.stringify(p) : p.join('.'))

export function Identity() {
  const { client, api, tenant } = useTenant()
  const issuers = useLoad(() => client.get<{ issuers: Issuer[] }>(`${api}/issuers?limit=500`), [api])
  const consoles = useLoad(() => client.get<{ audience: string; issuers: ConsoleIssuer[] }>(`${api}/console`, { anonymous: true }), [api])
  const audiences = useLoad(() => client.get<{ audiences: string[] }>(`${api}/audiences`), [api])
  const [adding, setAdding] = useState(false)
  const reload = () => {
    issuers.reload()
    consoles.reload()
  }
  return (
    <>
      <PageTitle title="Identity">
        <span className="pb-1 text-ink-muted">The identity providers whose tokens {tenant} accepts, and the audiences they carry.</span>
        <span className="ml-auto"><Button tone="brand" onClick={() => setAdding(!adding)}>{adding ? 'Close' : 'Add an issuer'}</Button></span>
      </PageTitle>
      {adding && <NewIssuer onDone={() => { setAdding(false); reload() }} />}
      {issuers.error ? <ErrorBox error={issuers.error} /> : issuers.loading && !issuers.data ? <Loading /> : issuers.data!.data.issuers.length === 0 ? (
        <Empty>No issuer records: no token is accepted here yet.</Empty>
      ) : (
        issuers.data!.data.issuers.map((is) => (
          <IssuerCard key={is.name} is={is} onChanged={reload} canonical={consoles.data?.data.audience ?? ''}
            console={consoles.error ? 'unknown' : consoles.data?.data.issuers.find((c) => c.name === is.name)} />
        ))
      )}
      <Card title="Audiences">
        {audiences.error ? <ErrorBox error={audiences.error} /> : audiences.loading && !audiences.data ? <Loading /> : (
          <ul className="m-0 flex list-none flex-col gap-1 p-0">
            {audiences.data!.data.audiences.map((a, i) => (
              <li key={a}><Mono>{a}</Mono>{i === 0 && <span className="ml-2 text-[13px] text-ink-muted">canonical</span>}</li>
            ))}
          </ul>
        )}
        <div className="mt-3"><Note>A token must carry one of these in aud. Assigning another audience (api://kista-acme for Entra) is a server administrator's.</Note></div>
      </Card>
    </>
  )
}

function IssuerCard({ is, console: c, canonical, onChanged }: { is: Issuer; console?: ConsoleIssuer | 'unknown'; canonical: string; onChanged: () => void }) {
  const cc = c === 'unknown' ? undefined : c
  const { client, api } = useTenant()
  const [removing, setRemoving] = useState(false)
  const [editing, setEditing] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const path = `${api}/issuers/${seg(is.name)}`
  const remove = async () => {
    setRemoving(false)
    setFailure(null)
    try {
      await client.del(path, { ifMatch: is.etag })
    } catch (e) {
      setFailure(e)
    }
    onChanged()
  }
  const facts: [string, React.ReactNode][] = [
    ['URL', <Mono>{is.url}</Mono>],
    ['JWKS', is.jwks_uri ? <Mono>{is.jwks_uri}</Mono> : <span className="text-ink-muted">from discovery</span>],
    ['Algorithms', is.algorithms?.join(', ') ?? ''],
    ['Required claims', Object.entries(is.required_claims ?? {}).map(([k, v]) => `${k}=${v}`).join(', ') || <span className="text-ink-muted">none: issuer-wide grants refused</span>],
    ['Roles claim', <Mono>{showPath(is.roles_claim)}</Mono>],
    ['Groups claim', <Mono>{showPath(is.groups_claim)}</Mono>],
    ['Client claim', <Mono>{showPath(is.client_claim)}</Mono>],
    ['Token lifetime', is.max_token_lifetime ?? ''],
    ['Added', `${when(is.created_at)}${is.created_by ? ` by ${is.created_by}` : ''}`],
    ['Console sign-in', c === 'unknown' ? <span className="text-ink-muted">could not be read</span> : cc ? <><Mono>{cc.client_id}</Mono> <span className="text-ink-muted">· {cc.scopes.join(' ')} · audience {cc.audience}</span></> : <span className="text-ink-muted">off</span>],
  ]
  return (
    <Card title={<span className="font-mono">{is.name}</span>}
      actions={<>
        <Button tone="outline" onClick={() => setEditing(!editing)}>{editing ? 'Close' : cc ? 'Console client' : 'Allow console sign-in'}</Button>
        <Button tone="danger-outline" onClick={() => setRemoving(true)}>Remove…</Button>
      </>}>
      <dl className="m-0 grid grid-cols-[10rem_1fr] gap-x-4 gap-y-2">
        {facts.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-ink-muted">{k}</dt>
            <dd className="m-0 min-w-0 break-words">{v}</dd>
          </div>
        ))}
      </dl>
      {failure != null && <div className="mt-3"><ErrorBox error={failure} /></div>}
      {editing && <ConsoleClientForm key={cc?.client_id ?? ''} is={is} cc={cc} canonical={canonical} onDone={() => { setEditing(false); onChanged() }} onStale={onChanged} />}
      <Confirm open={removing} title={`Remove the issuer ${is.name}?`} confirm="Remove" danger onConfirm={remove} onClose={() => setRemoving(false)}>
        <p>Its tokens stop working here, and every grant to its principals goes with it: a record added later under the same name starts with none.</p>
        <p>If you signed in through it, you lose access here too.</p>
      </Confirm>
    </Card>
  )
}

/** Sets or removes an issuer's console client (spec 0015), with the record's ETag. */
function ConsoleClientForm({ is, cc, canonical, onDone, onStale }: { is: Issuer; cc?: ConsoleIssuer; canonical: string; onDone: () => void; onStale: () => void }) {
  const { client, api } = useTenant()
  const [clientID, setClientID] = useState(cc?.client_id ?? '')
  const [scopes, setScopes] = useState(cc?.scopes.join(' ') ?? 'openid profile offline_access')
  // the canonical audience is the default: kept empty, so a later public_url still applies
  const [audience, setAudience] = useState(cc && cc.audience !== canonical ? cc.audience : '')
  const [param, setParam] = useState(cc?.audience_parameter ?? '')
  const [busy, setBusy] = useState(false)
  const [turningOff, setTurningOff] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const path = `${api}/issuers/${seg(is.name)}/console`
  const act = async (f: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      await f()
      onDone()
    } catch (e) {
      if (e instanceof ApiError && e.status === 412) {
        setFailure(new Error('The issuer record changed meanwhile: it was read again.'))
        onStale()
      } else setFailure(e)
    }
    setBusy(false)
  }
  return (
    <form className="mt-4 flex flex-col gap-4 border-t border-line pt-4"
      onSubmit={(e) => { e.preventDefault(); act(() => client.post(path, { ifMatch: is.etag, body: { client_id: clientID, scopes: scopes.split(/\s+/).filter(Boolean), audience, audience_parameter: param } })) }}>
      <Note>A public client at this issuer lets the console sign in here. Its tokens must carry one of the tenant's audiences.</Note>
      <div className="grid grid-cols-2 gap-4">
        <Field label="Client id"><input className="field font-mono" required value={clientID} onChange={(e) => setClientID(e.target.value)} /></Field>
        <Field label="Scopes"><input className="field font-mono" value={scopes} onChange={(e) => setScopes(e.target.value)} /></Field>
        <Field label="Audience" hint="empty: the canonical one"><input className="field font-mono" value={audience} onChange={(e) => setAudience(e.target.value)} /></Field>
        <Field label="Audience parameter" hint="the authorization request's parameter naming it, if the IdP takes one (audience, resource)"><input className="field font-mono" value={param} onChange={(e) => setParam(e.target.value)} /></Field>
      </div>
      {failure != null && <ErrorBox error={failure} />}
      <div className="flex gap-2">
        <Button type="submit" tone="brand" disabled={busy}>Save</Button>
        {cc && <Button tone="danger-outline" disabled={busy} onClick={() => setTurningOff(true)}>Turn console sign-in off…</Button>}
      </div>
      <Confirm open={turningOff} title={`Turn console sign-in off at ${is.name}?`} confirm="Turn off" danger
        onConfirm={() => { setTurningOff(false); act(() => client.del(path, { ifMatch: is.etag })) }} onClose={() => setTurningOff(false)}>
        <p>Nobody can sign in to this console through {is.name} any more; sessions already open keep their tokens until they expire. If it is the only issuer with a console client, the CLI or a server administrator turns it on again.</p>
      </Confirm>
    </form>
  )
}

/** Adds an issuer record: the API fetches its discovery document and JWKS before it accepts it. */
function NewIssuer({ onDone }: { onDone: () => void }) {
  const { client, api } = useTenant()
  const [f, setF] = useState({ name: '', url: '', jwks_uri: '', required: '', roles: '', groups: '', client: '', lifetime: '24h' })
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: e.target.value })
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setFailure(null)
    try {
      const body: Record<string, unknown> = { name: f.name, url: f.url, max_token_lifetime: f.lifetime || undefined, required_claims: requiredClaims(f.required) }
      if (f.jwks_uri) body.jwks_uri = f.jwks_uri
      for (const [k, v] of [['roles_claim', f.roles], ['groups_claim', f.groups], ['client_claim', f.client]] as const) {
        const p = claimPath(v)
        if (p) body[k] = p
      }
      await client.post(`${api}/issuers`, { body })
      onDone()
    } catch (err) {
      setFailure(err)
    }
    setBusy(false)
  }
  return (
    <Card title="Add an issuer">
      <form onSubmit={submit} className="flex flex-col gap-4">
        <div className="grid grid-cols-2 gap-4">
          <Field label="Name" hint="how principals name it: role:<name>|admin"><input className="field font-mono" required pattern="[a-z][a-z0-9-]{0,15}" value={f.name} onChange={set('name')} /></Field>
          <Field label="Issuer URL" hint="https, exactly the tokens' iss"><input className="field font-mono" required type="url" value={f.url} onChange={set('url')} /></Field>
          <Field label="Required claims" hint="claim=value, separated by commas: tid=… for Entra. Without any, no issuer-wide grant."><input className="field font-mono" value={f.required} onChange={set('required')} /></Field>
          <Field label="JWKS URI" hint="empty: from the discovery document"><input className="field font-mono" value={f.jwks_uri} onChange={set('jwks_uri')} /></Field>
          <Field label="Roles claim" hint="keys joined by dots (realm_access.roles), or a JSON array of keys"><input className="field font-mono" value={f.roles} onChange={set('roles')} /></Field>
          <Field label="Groups claim"><input className="field font-mono" value={f.groups} onChange={set('groups')} /></Field>
          <Field label="Client claim" hint="azp, appid or client_id"><input className="field font-mono" value={f.client} onChange={set('client')} /></Field>
          <Field label="Longest token lifetime" hint="within the server's limit (168h by default)"><input className="field font-mono" value={f.lifetime} onChange={set('lifetime')} /></Field>
        </div>
        {failure != null && <ErrorBox error={failure} />}
        <div><Button type="submit" tone="brand" disabled={busy}>{busy ? 'Checking the issuer…' : 'Add'}</Button></div>
      </form>
    </Card>
  )
}
