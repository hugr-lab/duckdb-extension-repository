import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { Badge, Button, Card, Confirm, Empty, ErrorBox, Loading, Mono, Note, Table, when } from '../components/ui'
import { useAction } from '../lib/action'
import { useLoad, useTenant } from '../lib/context'
import { type Client, seg } from '../lib/http'
import {
  type ChannelRetention, type ChannelVersion, type InForce, type Pin, type Plan, type PlanRelease, type PlanRetired, type TenantRetention,
  allPages, fieldWords, graceMeaning, graceWords, inForce, lineRows, restoreWords, retentionWords, ruleBody,
} from '../lib/retention'
import { sizeText } from '../lib/size'
import type { Release } from './types'

// A channel's Retention tab (spec 0042, the owner's mockup's Retention screen): the rule in force and
// the channel's own, what is always kept, each channel's rule, the pins, the versions by line, the
// next pass's plan (Retire, Purge, Retired in the grace with Restore), and "Retire the line…". A
// channel administrator's; the tenant's administrators set the default. Every list follows its pages
// to the end.

/** The plan, every page of it. */
async function readPlan(client: Client, path: string): Promise<Plan> {
  let first: Plan | null = null
  const rest: Plan[] = []
  await allPages(async (cursor) => {
    const p = (await client.get<Plan>(`${path}/plan?limit=500${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`)).data
    if (first) rest.push(p)
    else first = p
    return { items: [], next: p.next }
  })
  const plan = first!
  for (const p of rest) {
    plan.retire.push(...p.retire)
    plan.purge.push(...p.purge)
    plan.retired.push(...p.retired)
  }
  return plan
}

const pageOf = <T,>(client: Client, path: string, key: string) => (cursor: string) =>
  client.get<Record<string, unknown>>(`${path}${path.includes('?') ? '&' : '?'}limit=500${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`)
    .then((r) => ({ items: (r.data[key] ?? []) as T[], next: r.data.next as string | undefined }))

export function Retention({ channel, versions }: { channel: string; versions: ChannelVersion[] }) {
  const { client, api, perms } = useTenant()
  const path = `${api}/channels/${seg(channel)}/retention`
  const [epoch, setEpoch] = useState(0)
  const rule = useLoad(() => client.get<ChannelRetention>(path), [path])
  const plan = useLoad(() => readPlan(client, path), [path])
  const pins = useLoad(() => allPages(pageOf<Pin>(client, `${path}/pins`, 'pins')), [path])
  const releases = useLoad(() => allPages(pageOf<Release>(client, `${api}/channels/${seg(channel)}/releases`, 'releases')), [api, channel])
  const reload = () => {
    rule.reload()
    plan.reload()
    pins.reload()
    releases.reload()
    setEpoch((n) => n + 1)
  }
  if (rule.error) return <ErrorBox error={rule.error} />
  if (!rule.data) return <Loading />
  const r = rule.data.data
  return (
    <div className="flex flex-col gap-4">
      <RuleCard rule={r} etag={r.etag} path={path} purgeOff={plan.data?.purge_off} onChanged={reload} />
      {(perms.tenantAdmin || perms.canAudit) && <ChannelRules epoch={epoch} onChanged={reload} />}
      <Pins channel={channel} path={path} versions={versions} pins={pins.data} error={pins.error} onChanged={reload} />
      <Card title="Versions by line">
        {releases.error ? <ErrorBox error={releases.error} /> : !releases.data || !plan.data ? <Loading /> : (
          <LinesTable rows={lineRows(releases.data, versions, plan.data.retire)} />
        )}
      </Card>
      {plan.error ? <ErrorBox error={plan.error} /> : !plan.data ? <Loading /> : (
        <PlanCards channel={channel} plan={plan.data} onChanged={reload} />
      )}
      <RetireLine channel={channel} path={path} versions={versions} inForce={r.in_force} purgeOff={plan.data?.purge_off} onChanged={reload} />
    </div>
  )
}

/** A rule's form fields, filled again from the stored rule whenever its tag changes (a save, or a 412
 * read again): the form is not remounted, so a failure stays on the page. */
function useRuleForm(rule: { keep: number | 'none' | null; grace: string | null }, etag?: string) {
  const [keep, setKeep] = useState(rule.keep === null ? '' : String(rule.keep))
  const [grace, setGrace] = useState(rule.grace ?? '')
  useEffect(() => {
    setKeep(rule.keep === null ? '' : String(rule.keep))
    setGrace(rule.grace ?? '')
    // the stored rule's tag says when it changed
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [etag])
  return { keep, setKeep, grace, setGrace }
}

/** The rule in force (each field with where it comes from), the channel's own, and its form. */
function RuleCard({ rule, etag, path, purgeOff, onChanged }: { rule: ChannelRetention; etag?: string; path: string; purgeOff?: boolean; onChanged: () => void }) {
  const { client } = useTenant()
  const { keep, setKeep, grace, setGrace } = useRuleForm(rule, etag)
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
        <div aria-label="always kept">
          <b>Always kept</b>
          <ul className="m-0 mt-1 list-disc pl-5">
            <li>each DuckDB version's current versions, in the full and in the public view;</li>
            <li>the newest N versions of each name per DuckDB version, by when a version first arrived there;</li>
            <li>the pinned versions.</li>
          </ul>
        </div>
        <Note>
          A retired release is served nowhere; it can be restored until it is purged (the grace ends, unless a pin keeps it or
          purging is off). {restoreWords}
          {purgeOff ? ' Purging is off while the server\'s gc.interval is 0: nothing is purged.' : ''}
        </Note>
        <form role="group" aria-label="the channel's rule" className="flex flex-wrap items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault()
            act(async () => { await client.put(path, { body: ruleBody(keep, grace), ifMatch: etag }) }).then((ok) => ok && onChanged())
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

/** The tenant's default (its administrators set it) and each signed channel's rule in force, read
 * again whenever the tab changed something (epoch). */
function ChannelRules({ epoch, onChanged }: { epoch: number; onChanged: () => void }) {
  const { client, api, perms } = useTenant()
  const tenant = useLoad(() => client.get<TenantRetention>(`${api}/retention`), [api, epoch])
  const chs = useLoad(async () => {
    const list = await client.get<{ channels: { name: string; kind: string }[] }>(`${api}/channels`)
    return Promise.all(list.data.channels.filter((c) => c.kind === 'signed').map(async (c) => {
      try {
        return { name: c.name, rule: (await client.get<ChannelRetention>(`${api}/channels/${seg(c.name)}/retention`)).data }
      } catch {
        return { name: c.name, rule: null }
      }
    }))
  }, [api, epoch])
  const t = tenant.data?.data
  return (
    <Card title="Each channel's rule">
      <div className="flex flex-col gap-3 text-[13px]">
        <span>Tenant default: {t ? (t.keep === null && t.grace === null ? 'none (no retention by default)' :
          `keep ${t.keep ?? 'not set'} · grace ${t.grace === null ? 'not set' : graceWords(t.grace)}`) : '…'}</span>
        {perms.tenantAdmin && tenant.data && (
          <TenantDefault rule={tenant.data.data} etag={tenant.data.data.etag} onChanged={() => {
            tenant.reload()
            chs.reload()
            onChanged()
          }} />
        )}
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

/** The tenant's default rule's form, filled with the stored rule: a PUT is the whole rule, so a field
 * left empty clears it. */
function TenantDefault({ rule, etag, onChanged }: { rule: TenantRetention; etag?: string; onChanged: () => void }) {
  const { client, api } = useTenant()
  const { keep, setKeep, grace, setGrace } = useRuleForm(rule, etag)
  const { busy, failure, act } = useAction('The tenant\'s rule', onChanged)
  return (
    <>
      <form role="group" aria-label="the tenant's default rule" className="flex flex-wrap items-center gap-2"
        onSubmit={(e) => {
          e.preventDefault()
          act(async () => { await client.put(`${api}/retention`, { body: ruleBody(keep === 'none' ? '' : keep, grace), ifMatch: etag }) })
            .then((ok) => ok && onChanged())
        }}>
        <label className="flex items-center gap-1.5">Keep
          <input aria-label="default keep" className="field w-28 py-1 font-mono" placeholder="none" value={keep} onChange={(e) => setKeep(e.target.value)} />
        </label>
        <label className="flex items-center gap-1.5">Grace
          <input aria-label="default grace" className="field w-28 py-1 font-mono" placeholder="none" value={grace} onChange={(e) => setGrace(e.target.value)} />
        </label>
        <Button type="submit" disabled={busy}>Set the default</Button>
        <span className="text-ink-muted">the whole default: an empty field clears it</span>
      </form>
      {failure != null && <ErrorBox error={failure} />}
    </>
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
        <Note>A pin keeps a version, in one DuckDB version or in every one, whatever the rule: from the pass's retirement, and a retired one from the pass's purge.</Note>
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
      ['DuckDB', (x) => <span className="flex flex-wrap items-center gap-1"><Mono>{x.line}</Mono>{x.notBuilt && <Badge>no longer built</Badge>}</span>],
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

/** The plan's three lists: what the next pass retires, purges, and what is retired in the grace, with
 * Restore wherever a release is still restorable (in the grace, pinned, or while purging is off). */
function PlanCards({ channel, plan, onChanged }: { channel: string; plan: Plan; onChanged: () => void }) {
  const { client, api, base } = useTenant()
  const { busy, failure, act } = useAction('The release', onChanged)
  const [restoring, setRestoring] = useState<PlanRetired | null>(null)
  const restore = (x: PlanRetired) =>
    act(() => client.post(`${api}/channels/${seg(channel)}/extensions/${seg(x.name)}/releases/${seg(x.release.id)}/restore`, { ifMatch: '*' }))
      .then((ok) => ok && onChanged())
  const link = (x: { name: string }, id: string, text: string) => (
    <Link className="font-mono no-underline" to={`${base}/channels/${seg(channel)}/releases/${seg(x.name)}/${seg(id)}`}>{text}</Link>
  )
  const restoreButton = (x: PlanRetired) => <Button disabled={busy} onClick={() => setRestoring(x)}>Restore</Button>
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
            ['State', (x) => (x.pinned ? <Badge>pinned</Badge> : plan.purge_off ? 'purging is off' : 'purged by the next pass')],
            ['Restore', (x) => (x.pinned || plan.purge_off ? restoreButton(x) : '')],
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
            ['', restoreButton],
          ]} />
        )}
      </Card>
      <Confirm open={restoring !== null} title={`Restore ${restoring?.name ?? ''} ${restoring?.version ?? ''} (${restoring?.release.platform ?? ''})?`}
        confirm="Restore" onConfirm={() => {
          const x = restoring!
          setRestoring(null)
          restore(x)
        }} onClose={() => setRestoring(null)}>
        <p>The release returns to the state it had and is served again. {restoreWords}</p>
      </Confirm>
    </>
  )
}

/** "Retire the line…": what only a DuckDB version keeps (a version the channel no longer serves too). */
function RetireLine({ channel, path, versions, inForce: f, purgeOff, onChanged }: {
  channel: string; path: string; versions: ChannelVersion[]; inForce: InForce; purgeOff?: boolean; onChanged: () => void
}) {
  const { client } = useTenant()
  const [open, setOpen] = useState(false)
  const [line, setLine] = useState('')
  const [name, setName] = useState('')
  const [done, setDone] = useState<{ retired: unknown[]; kept: unknown[]; withdrawn: number; pins_removed: number } | null>(null)
  const { busy, failure, act } = useAction('The line', onChanged)
  const l = line.trim() || 'the DuckDB version'
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
        <label className="block">DuckDB version
          <input data-autofocus aria-label="line to retire" list="retention-lines" className="field mt-1 w-full font-mono" value={line}
            onChange={(e) => setLine(e.target.value)} />
          <datalist id="retention-lines">{versions.map((v) => <option key={v.duckdb_version} value={v.duckdb_version} />)}</datalist>
        </label>
        <label className="block">Only this extension (empty: every one)
          <input aria-label="line's extension" className="field mt-1 w-full font-mono" value={name} onChange={(e) => setName(e.target.value)} />
        </label>
        <p aria-label="the line act's scope">
          Retires every active or deprecated release {name.trim() ? <>of <Mono>{name.trim()}</Mono> </> : 'of every extension '}serving{' '}
          <Mono>{l}</Mono> that no other DuckDB version of {channel} keeps, current and pinned versions included; a build another
          DuckDB version keeps stays. {name.trim() ? <>The pins of <Mono>{name.trim()}</Mono> for <Mono>{l}</Mono> are removed</> :
            <>The pins for <Mono>{l}</Mono> are removed</>} (a pin for every version stays). The version need not be served any
          more. A retired release is {graceMeaning(f, purgeOff)}.
        </p>
      </Confirm>
    </Card>
  )
}
