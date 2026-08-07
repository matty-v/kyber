import { Routes, Route, useLocation } from 'react-router-dom'
import { Layout } from './components/Layout'
import { Dashboard } from './pages/Dashboard'
import { MachineList } from './pages/MachineList'
import { MachineDetail } from './pages/MachineDetail'
import { AgentList } from './pages/AgentList'
import { AgentDetail } from './pages/AgentDetail'
import { CreateMachine } from './pages/CreateMachine'
import { CreateAgent } from './pages/CreateAgent'
import { Settings } from './pages/Settings'
import { DesignSpecimen } from './pages/DesignSpecimen'
import { MetricsTab } from './pages/MetricsTab'
import { useWebSocket } from './hooks/useWebSocket'

function AppRoutes() {
  // Attach the WebSocket event bus — invalidates React Query caches on CRD changes.
  useWebSocket()

  const location = useLocation()

  // Design specimen is a bare page with no app chrome or API wiring.
  if (location.pathname === '/_design') {
    return (
      <Routes>
        <Route path="_design" element={<DesignSpecimen />} />
      </Routes>
    )
  }

  return (
    <Layout>
      <div
        key={location.pathname}
        style={{ animation: 'kyber-fade-in 220ms ease-out' }}
      >
        <Routes>
          <Route index element={<Dashboard />} />
          <Route path="machines" element={<MachineList />} />
          <Route path="machines/new" element={<CreateMachine />} />
          <Route path="machines/:name" element={<MachineDetail />} />
          <Route path="agents" element={<AgentList />} />
          <Route path="agents/new" element={<CreateAgent />} />
          <Route path="agents/:name" element={<AgentDetail />} />
          <Route path="metrics" element={<MetricsTab />} />
          <Route path="settings" element={<Settings />} />
        </Routes>
      </div>
    </Layout>
  )
}

export function App() {
  return <AppRoutes />
}
