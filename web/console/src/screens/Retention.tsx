import { useState } from 'react'
import { Link } from 'react-router-dom'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Loading, Mono, Note, Table, when } from '../components/ui'
import { useAction } from '../lib/action'
import { useLoad, useTenant } from '../lib/context'
import { seg } from '../lib/http'
import {
  type ChannelRetention, type ChannelVersion, type Pin, type Plan, type PlanRelease, type PlanRetired, type TenantRetention,
  fieldWords, graceWords, inForce, lineRows, retentionWords, ruleBody,
} from '../lib/retention'
import { sizeText } from '../lib/size'
import type { Release } from './types'

// A channel's Retention tab (spec 0042, the owner's mockup's Retention screen): the rule in force and
// the channel's own, what is always kept, each channel's rule, the pins, the versions by line, the
// next pass's plan (Retire, Purge, Retired in the grace with Restore), and "Retire the line…". A
// channel administrator's; the tenant's administrators set the default.

export function Retention({ channel, versions }: { channel: string; versions: ChannelVersion[] }) {
  const { client, api, perms } = useTenant()
  const path = `${api}/channels/${seg(channel)}/retention`
  const rule = useLoad(() => client.get<ChannelRetention>(path), [path])
  const plan = useLoad(() => client.get<Plan>(`${path}/plan?limit=500`), [path])
  const pins = useLoad(() => client.get<{ pins: Pin[] }>(`${path}/pins?limit=500`), [path])
  const releases = useLoad(() => client.get<{ releases: Release[] }>(`${api}/channels/${seg(channel)}/releases?limit=500`), [api, channel])
  const reload = () => {
    rule.reload()
    plan.reload()
    pins.reload()
    releases.reload()
  }
  if (rule.error) return <ErrorBox error={rule.error} />
  if (!rule.data) return <Loading />
  const r = rule.data.data
  return (
    <div className="flex flex-col gap-4">
      <RuleCard rule={r} etag={rule.data.etag} path={path} purgeOff={plan.data?.data.purge_off} onChanged={reload} />
      {(perms.tenantAdmin || perms.canAudit) && <ChannelRules onChanged={reload} />}
      <Pins channel={channel} path={path} versions={versions} pins={pins.data?.data.pins} error={pins.error} onChanged={reload} />
      <Card title="Versions by line">
        {releases.error ? <ErrorBox error={releases.error} /> : !releases.data || !plan.data ? <Loading /> : (
          <LinesTable rows={lineRows(releases.data.data.releases, versions, plan.data.data.retire)} />
        )}
      </Card>
      {plan.error ? <ErrorBox error={plan.error} /> : !plan.data ? <Loading /> : (
        <PlanCards channel={channel} plan={plan.data.data} onChanged={reload} />
      )}
      <RetireLine channel={channel} path={path} versions={versions} onChanged={reload} />
    </div>
  )
}

/** The rule in force (each field with where it comes from), the channel's own, and its form. */
function RuleCard({ rule, etag, path, purgeOff, onChanged }: { rule: ChannelRetention; etag?: string; path: string; purgeOff?: boolean; onChanged: () => void }) {
  const { client } = useTenant()
  const [keep, setKeep] = useState(rule.keep === null ? '' : String(rule.keep))
  const [grace, setGrace] = useState(rule.grace ?? '')
  const { busy, failure, act } = useAction('The rule', onChanged)
  const f = rule.in_force
  return (
    <Card title="Rule">
      <div className="flex flex-col gap-3 text-[13px]">
        <span aria-label="retention in force"><b>{retentionWords(f)}</b></span>
        <dl className="m-0 grid grid-cols-[8rem_1fr] gap-x-4 gap-y-1">
          <dt className="text-ink-muted">Keep</dt><dd>{fieldWords(f.keep)}</dd>
          <dt className="text-ink-muted">Grace</dt><dd>{fieldWords(f.grace, true)}</dd>
          <dt className="text-ink-muted">Last pass</dt><dd>{rule.checked_at ? when(rule.checked_at) : 'never'}{rule.next_run ? ` · next ${when(rule.next_run)}` : ''}</dd>
        </dl>
        <Note>
          Always kept: each DuckDB version's current versions (in the full and the public view) and the pinned versions. The newest
          versions are counted per DuckDB version, by when a version first arrived there; a retired release is served nowhere, and
          can be restored until the grace ends.{purgeOff ? ' Purging is off while the server\'s gc.interval is 0: nothing is purged.' : ''}
        </Note>
        <form role="group" aria-label="the channel's rule" className="flex flex-wrap items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault()
            act(() => client.put(path, { body: ruleBody(keep, grace), ifMatch: etag })).then((ok) => ok && onChanged())
          }}>
          <label className="flex items-center gap-1.5">Keep
            <input aria-label="keep" className="field w-28 py-1 font-mono" placeholder="tenant's" value={keep} onChange={(e) => setKeep(e.target.value)} />
          </label>
          <label className="flex items-center gap-1.5">Grace
            <input aria-label="grace" className="field w-28 py-1 font-mono" placeholder="tenant's" value={grace} onChange={(e) => setGrace(e.target.value)} />
          </label>
          <Button type="submit" disabled={busy}>Save</Button>
          <span className="text-ink-muted">empty: the tenant's · keep none: no retention here · grace 720h, 30d, 0</span>
        </form>
        {failure != null && <ErrorBox error={failure} />}
      </div>
    </Card>
  )
}

/** The tenant's default (its administrators set it) and each signed channel's rule in force. */
function ChannelRules({ onChanged }: { onChanged: () => void }) {
  const { client, api, perms } = useTenant()
  const tenant = useLoad(() => client.get<TenantRetention>(`${api}/retention`), [api])
  const chs = useLoad(async () => {
    const list = await client.get<{ channels: { name: string; kind: string }[] }>(`${api}/channels`)
    return Promise.all(list.data.channels.filter((c) => c.kind === 'signed').map(async (c) => {
      try {
        return { name: c.name, rule: (await client.get<ChannelRetention>(`${api}/channels/${seg(c.name)}/retention`)).data }
      } catch {
        return { name: c.name, rule: null }
      }
    }))
  }, [api])
  const [keep, setKeep] = useState('')
  const [grace, setGrace] = useState('')
  const { busy, failure, act } = useAction('The tenant\'s rule', () => tenant.reload())
  const t = tenant.data?.data
  return (
    <Card title="Each channel's rule">
      <div className="flex flex-col gap-3 text-[13px]">
        <span>Tenant default: {t ? (t.keep === null && t.grace === null ? 'none (no retention by default)' :
          `keep ${t.keep ?? 'not set'} · grace ${t.grace === null ? 'not set' : graceWords(t.grace)}`) : '…'}</span>
        {perms.tenantAdmin && (
          <form role="group" aria-label="the tenant's default rule" className="flex flex-wrap items-center gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              act(() => client.put(`${api}/retention`, { body: ruleBody(keep === 'none' ? '' : keep, grace), ifMatch: tenant.data?.etag }))
                .then((ok) => {
                  if (ok) {
                    tenant.reload()
                    chs.reload()
                    onChanged()
                  }
                })
            }}>
            <label className="flex items-center gap-1.5">Keep
              <input aria-label="default keep" className="field w-28 py-1 font-mono" placeholder="none" value={keep} onChange={(e) => setKeep(e.target.value)} />
            </label>
            <label className="flex items-center gap-1.5">Grace
              <input aria-label="default grace" className="field w-28 py-1 font-mono" placeholder="none" value={grace} onChange={(e) => setGrace(e.target.value)} />
            </label>
            <Button type="submit" disabled={busy}>Set the default</Button>
          </form>
        )}
        {failure != null && <ErrorBox error={failure} />}
        {chs.error ? <ErrorBox error={chs.error} /> : !chs.data ? <Loading /> : (
          <Table rows={chs.data} rowKey={(x) => x.name} columns={[
            ['Channel', (x) => <Mono>{x.name}</Mono>],
            ['In force', (x) => (x.rule ? retentionWords(x.rule.in_force) : '—')],
            ['Keep from', (x) => x.rule?.in_force.keep?.from ?? ''],
            ['Grace from', (x) => x.rule?.in_force.grace?.from ?? ''],
          ]} />
        )}
      </div>
    </Card>
  )
}

/** The pins (Pin and Unpin, with the reason). */
function Pins({ channel, path, versions, pins, error, onChanged }: {
  channel: string; path: string; versions: ChannelVersion[]; pins?: Pin[]; error?: unknown; onChanged: () => void
}) {
  const { client } = useTenant()
  const [form, setForm] = useState({ name: '', version: '', line: '', reason: '' })
  const [unpinning, setUnpinning] = useState<Pin | null>(null)
  const { busy, failure, act } = useAction('The pins', onChanged)
  return (
    <Card title="Pins">
      <div className="flex flex-col gap-3 text-[13px]">
        <Note>A pin keeps a version, in one DuckDB version or in every one, whatever the rule: a retired one from the purge too.</Note>
        {error != null ? <ErrorBox error={error} /> : !pins ? <Loading /> : pins.length === 0 ? <Empty>No pins in {channel}.</Empty> : (
          <Table rows={pins} rowKey={(p) => p.id} columns={[
            ['Extension', (p) => <Mono>{p.name}</Mono>],
            ['Version', (p) => <Mono>{p.version}</Mono>],
            ['DuckDB', (p) => (p.duckdb_version ? <Mono>{p.duckdb_version}</Mono> : 'every version')],
            ['Reason', (p) => p.reason],
            ['Pinned', (p) => `${when(p.pinned_at)} by ${p.pinned_by}`],
            ['', (p) => <Button tone="danger-outline" onClick={() => setUnpinning(p)}>Unpin</Button>],
          ]} />
        )}
        <form role="group" aria-label="pin a version" className="flex flex-wrap items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault()
            act(() => client.post(`${path}/pins`, { body: { name: form.name, version: form.version, duckdb_version: form.line || undefined,
              reason: form.reason } })).then((ok) => {
              if (ok) {
                setForm({ name: '', version: '', line: '', reason: '' })
                onChanged()
              }
            })
          }}>
          <input aria-label="pin name" className="field w-36 py-1 font-mono" placeholder="extension" required value={form.name}
            onChange={(e) => setForm({ ...form, name: e.target.value })} />
          <input aria-label="pin version" className="field w-24 py-1 font-mono" placeholder="version" required value={form.version}
            onChange={(e) => setForm({ ...form, version: e.target.value })} />
          <select aria-label="pin DuckDB version" className="field py-1" value={form.line} onChange={(e) => setForm({ ...form, line: e.target.value })}>
            <option value="">every DuckDB version</option>
            {versions.map((v) => <option key={v.duckdb_version}>{v.duckdb_version}</option>)}
          </select>
          <input aria-label="pin reason" className="field w-56 py-1" placeholder="why (LTS nodes run it)" required maxLength={400} value={form.reason}
            onChange={(e) => setForm({ ...form, reason: e.target.value })} />
          <Button type="submit" disabled={busy}>Pin</Button>
        </form>
        {failure != null && <ErrorBox error={failure} />}
      </div>
      <Confirm open={unpinning !== null} title={`Unpin ${unpinning?.name ?? ''} ${unpinning?.version ?? ''}?`} confirm="Unpin" danger
        onConfirm={() => {
          const p = unpinning!
          setUnpinning(null)
          act(() => client.del(`${path}/pins/${seg(p.id)}`)).then((ok) => ok && onChanged())
        }} onClose={() => setUnpinning(null)}>
        <p>The rule applies to the version again: the next pass may retire it, or purge it if it is retired and past its grace.</p>
      </Confirm>
    </Card>
  )
}

function LinesTable({ rows }: { rows: ReturnType<typeof lineRows> }) {
  if (rows.length === 0) return <Empty>No releases.</Empty>
  const list = (vs: string[]) => (vs.length === 0 ? '' : <Mono>{vs.join(' ')}</Mono>)
  return (
    <Table rows={rows} rowKey={(x) => `${x.name}|${x.line}`} columns={[
      ['Extension', (x) => <Mono>{x.name}</Mono>],
      ['DuckDB', (x) => <Mono>{x.line}</Mono>],
      ['Current', (x) => list(x.current)],
      ['Kept', (x) => list(x.kept)],
      ['Retired next', (x) => list(x.retire)],
      ['Retired', (x) => list(x.retired)],
    ]} />
  )
}

function bodyWords(r: PlanRelease) {
  if (!r.shared) return 'freed'
  const named = r.shared_by.join(', ')
  const others = r.others > 0 ? `${r.others} more` : ''
  return `kept by ${[named, others].filter(Boolean).join(', ')}`
}

/** The plan's three lists: what the next pass retires, purges, and what is retired in the grace (with
 * Restore). */
function PlanCards({ channel, plan, onChanged }: { channel: string; plan: Plan; onChanged: () => void }) {
  const { client, api, base } = useTenant()
  const { busy, failure, act } = useAction('The release', onChanged)
  const restore = (x: PlanRetired) =>
    act(() => client.post(`${api}/channels/${seg(channel)}/extensions/${seg(x.name)}/releases/${seg(x.release.id)}/restore`, { ifMatch: '*' }))
      .then((ok) => ok && onChanged())
  const link = (x: { name: string }, id: string, text: string) => (
    <Link className="font-mono no-underline" to={`${base}/channels/${seg(channel)}/releases/${seg(x.name)}/${seg(id)}`}>{text}</Link>
  )
  return (
    <>
      {failure != null && <ErrorBox error={failure} />}
      <Card title="Retire">
        {!inForce(plan.in_force) ? <Empty>No rule in force: the pass retires nothing.</Empty> : plan.retire.length === 0 ? <Empty>Nothing to retire.</Empty> : (
          <Table rows={plan.retire} rowKey={(x) => `${x.source}|${x.name}|${x.version}`} columns={[
            ['Extension', (x) => <Mono>{x.name}</Mono>],
            ['Version', (x) => <Mono>{x.version}</Mono>],
            ['DuckDB', (x) => <Mono>{x.lines.join(' ')}</Mono>],
            ['Platforms', (x) => x.platforms.join(', ')],
            ['Bytes', (x) => sizeText(x.bytes)],
            ['Why', (x) => Object.entries(x.kept).map(([l, vs]) => `${l} keeps ${vs.join(' ')}`).join('; ') +
              (x.kept_by.length > 0 ? `; ${x.kept_by.length} of its builds kept by another DuckDB version` : '')],
            ['Bodies', (x) => x.releases.map(bodyWords).join('; ')],
          ]} />
        )}
      </Card>
      <Card title="Purge">
        {plan.purge.length === 0 ? <Empty>Nothing past its grace.</Empty> : (
          <Table rows={plan.purge} rowKey={(x) => x.release.id} columns={[
            ['Release', (x) => link(x, x.release.id, `${x.name} ${x.version}`)],
            ['Platform', (x) => <Mono>{x.release.platform}</Mono>],
            ['Bytes', (x) => sizeText(x.release.bytes)],
            ['Body', (x) => bodyWords(x.release)],
            ['', (x) => (x.pinned ? <Badge>pinned</Badge> : plan.purge_off ? 'purging is off' : 'purged by the next pass')],
          ]} />
        )}
      </Card>
      <Card title="Retired, in the grace">
        {plan.retired.length === 0 ? <Empty>No retired release in its grace.</Empty> : (
          <Table rows={plan.retired} rowKey={(x) => x.release.id} columns={[
            ['Release', (x) => link(x, x.release.id, `${x.name} ${x.version}`)],
            ['Platform', (x) => <Mono>{x.release.platform}</Mono>],
            ['Retired', (x) => when(x.retired_at ?? '')],
            ['Purged', (x) => (x.pinned ? 'never while pinned' : x.purge_at ? when(x.purge_at) : 'not while no rule is in force')],
            ['', (x) => <Button disabled={busy} onClick={() => restore(x)}>Restore</Button>],
          ]} />
        )}
      </Card>
    </>
  )
}

/** "Retire the line…": what only a DuckDB version keeps (a version the channel no longer serves too). */
function RetireLine({ channel, path, versions, onChanged }: { channel: string; path: string; versions: ChannelVersion[]; onChanged: () => void }) {
  const { client } = useTenant()
  const [open, setOpen] = useState(false)
  const [line, setLine] = useState('')
  const [name, setName] = useState('')
  const [done, setDone] = useState<{ retired: unknown[]; kept: unknown[]; withdrawn: number; pins_removed: number } | null>(null)
  const { busy, failure, act } = useAction('The line', onChanged)
  return (
    <Card title="Retire a line">
      <div className="flex flex-col gap-2 text-[13px]">
        <span><Button tone="danger-outline" onClick={() => setOpen(true)}>Retire the line…</Button></span>
        {done && <span aria-label="the line act's outcome">{done.retired.length} retired, {done.kept.length} kept by another DuckDB version
          {done.withdrawn > 0 ? `, ${done.withdrawn} withdrawn skipped` : ''}{done.pins_removed > 0 ? `, ${done.pins_removed} pins removed` : ''}.</span>}
        {failure != null && <ErrorBox error={failure} />}
      </div>
      <Confirm open={open} title={`Retire a DuckDB version's line in ${channel}`} confirm="Retire the line" danger disabled={busy || line.trim() === ''}
        onConfirm={() => {
          setOpen(false)
          act(async () => {
            const r = await client.post<{ retired: unknown[]; kept: unknown[]; withdrawn: number; pins_removed: number }>(`${path}/lines`,
              { body: { duckdb_version: line.trim(), name: name.trim() || undefined } })
            setDone(r.data)
          }).then((ok) => ok && onChanged())
        }} onClose={() => setOpen(false)}>
        <p>Every active or deprecated release serving only this DuckDB version is retired, current and pinned versions included; the
          line's pins go. A build another DuckDB version keeps stays. The version need not be served any more.</p>
        <label className="block">DuckDB version
          <input data-autofocus aria-label="line to retire" list="retention-lines" className="field mt-1 w-full font-mono" value={line}
            onChange={(e) => setLine(e.target.value)} />
          <datalist id="retention-lines">{versions.map((v) => <option key={v.duckdb_version} value={v.duckdb_version} />)}</datalist>
        </label>
        <label className="block">Only this extension (empty: every one)
          <input aria-label="line's extension" className="field mt-1 w-full font-mono" value={name} onChange={(e) => setName(e.target.value)} />
        </label>
      </Confirm>
    </Card>
  )
}
