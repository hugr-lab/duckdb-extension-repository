import { BrowserRouter, Route, Routes, useParams } from 'react-router-dom'
import { FrameProvider } from './lib/frame'
import { scopeOf } from './lib/scope'
import { Landing } from './screens/Landing'
import { ScopeRoot } from './screens/ScopeRoot'
import { Server } from './screens/Server'
import { Tenant } from './screens/Tenant'

function TenantScope() {
  const { tenant = '' } = useParams()
  const scope = scopeOf(`/ui/t/${tenant}`)
  if (scope.kind !== 'tenant') return <Landing />
  return (
    <ScopeRoot key={scope.tenant} scope={scope}>
      {(client) => <Tenant client={client} tenant={scope.tenant} root={`/t/${scope.tenant}`} />}
    </ScopeRoot>
  )
}

/** The standalone console, under /ui (spec 0015). */
export function App() {
  return (
    <FrameProvider embedded={false}>
      <BrowserRouter basename="/ui">
        <Routes>
          <Route path="/server/*" element={<ScopeRoot scope={{ kind: 'server' }}>{(client) => <Server client={client} root="/server" />}</ScopeRoot>} />
          <Route path="/t/:tenant/*" element={<TenantScope />} />
          <Route path="*" element={<Landing />} />
        </Routes>
      </BrowserRouter>
    </FrameProvider>
  )
}
