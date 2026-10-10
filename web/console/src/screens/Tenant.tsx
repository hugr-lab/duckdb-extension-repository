import { useMemo } from 'react'
import { Empty, ErrorBox, Loading } from '../components/ui'
import { type Grant, Perms, TenantContext, useLoad } from '../lib/context'
import { ApiError, type Client } from '../lib/http'
import { TenantArea } from './TenantArea'

/** A tenant's scope: gated by its whoami (an admin grant anywhere in it, or audit). */
export function Tenant({ client, tenant }: { client: Client; tenant: string }) {
  const api = `/tenants/${tenant}`
  const me = useLoad(() => client.get<{ grants: Grant[]; holds_admin: boolean; holds_audit: boolean }>(`${api}/whoami`), [api])
  const perms = useMemo(() => me.data && new Perms(false, me.data.data.grants, me.data.data.holds_audit), [me.data])
  if (me.loading) return <Loading />
  if (me.error) return me.error instanceof ApiError && me.error.status === 404 ? <Empty>No such tenant.</Empty> : <ErrorBox error={me.error} />
  if (!me.data!.data.holds_admin && !me.data!.data.holds_audit) return <Empty>Nothing to administer here.</Empty>
  return (
    <TenantContext.Provider value={{ client, tenant, perms: perms!, api, base: `/t/${tenant}` }}>
      <TenantArea />
    </TenantContext.Provider>
  )
}
