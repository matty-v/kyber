import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { usePrefixedPath } from '../lib/route-prefix'
import { ArrowLeft } from 'lucide-react'
import { useComputeConfig, useCreateMachine } from '../hooks/useAPI'
import { Button } from '../components/Button'
import { Card } from '../components/Card'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  buildManagedMachineRequest,
  buildMockRequest,
  validateManagedMachineForm,
  validateMockForm,
  type ManagedMachineFormState,
  type MockFormState,
} from './CreateMachine.helpers'
import type { VMType } from '@/lib/types'
import { toKebabCase, toKebabCaseTyping } from '../lib/names'

// formatVMTypeLabel renders a catalog entry as a dropdown label, e.g.
// "e2-small (2 vCPU, 2Gi)". Memory is shown as the raw resource.Quantity
// string from the server — matches how the PWA displays agent resources
// elsewhere, and avoids unit-conversion drift between client and server.
function formatVMTypeLabel(vt: VMType): string {
  return `${vt.type} (${vt.cpu} vCPU, ${vt.memory})`
}

export function CreateMachine() {
  const navigate = useNavigate()
  const { data: config, isLoading: configLoading } = useComputeConfig()

  if (configLoading || !config) {
    return <LoadingState />
  }

  switch (config.compute.provider) {
    case 'mock':
    case 'static':
      return <CreateMachineMock navigate={navigate} provider={config.compute.provider} />
    case 'fake':
      return <CreateManagedMachine navigate={navigate} provider="fake" capabilities={config.compute.managed} />
    case 'gce':
      return <CreateManagedMachine navigate={navigate} provider="gce" capabilities={config.compute.managed} />
    case 'gke':
      return <CreateManagedMachine navigate={navigate} provider="gke" capabilities={config.compute.managed} />
    case '':
    default:
      return <CreateManagedMachine navigate={navigate} provider="gce" capabilities={config.compute.managed} />
  }
}

function LoadingState() {
  return (
    <div>
      <h1 className="text-xl font-bold text-text-primary mb-6">New Machine</h1>
      <Card className="max-w-lg">
        <p className="text-sm text-text-muted">Loading config…</p>
      </Card>
    </div>
  )
}

// Managed-compute form — driven by capabilities advertised by the server.
function CreateManagedMachine({
  navigate,
  provider,
  capabilities,
}: {
  navigate: ReturnType<typeof useNavigate>
  provider: 'gce' | 'gke' | 'fake'
  capabilities?: { profiles: VMType[]; locations: string[]; diskSizesGb: number[]; supportsInterruptible: boolean }
}) {
  const createMachine = useCreateMachine()
  const prefixed = usePrefixedPath()
  // Initial machineType: first entry from the server-provided catalog so
  // the form is always submittable. If the catalog is unexpectedly empty
  // (server misconfig), seed an empty string and let validation surface it.
  const profiles = capabilities?.profiles ?? []
  const locations = capabilities?.locations ?? []
  const diskSizes = capabilities?.diskSizesGb ?? []
  const [form, setForm] = useState<ManagedMachineFormState>({
    name: '',
    machineType: profiles[0]?.type ?? '',
    diskSizeGb: provider === 'gke' ? '0' : String(diskSizes.includes(50) ? 50 : (diskSizes[0] ?? '')),
    zone: locations[0] ?? '',
    spot: false,
  })
  const [fieldError, setFieldError] = useState<string | null>(null)

  function set<K extends keyof ManagedMachineFormState>(key: K, value: ManagedMachineFormState[K]) {
    setForm((prev) => ({ ...prev, [key]: value }))
    setFieldError(null)
  }

  function setName(raw: string) {
    set('name', toKebabCaseTyping(raw))
  }
  function blurName(raw: string) {
    set('name', toKebabCase(raw))
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    const err = provider === 'gke'
      ? (!form.name ? 'Name is required' : !form.machineType ? 'Machine profile is required' : !form.zone ? 'Location is required' : null)
      : validateManagedMachineForm(form)
    if (err) {
      setFieldError(err)
      return
    }
    try {
      // Defense-in-depth strip on name — if the user submits without blurring
      // (e.g. via Enter while focused), strip leading/trailing hyphens here.
      // See #189.
      await createMachine.mutateAsync(buildManagedMachineRequest({ ...form, name: toKebabCase(form.name) }, provider))
      navigate(prefixed('/machines'))
    } catch (err) {
      setFieldError(err instanceof Error ? err.message : 'Failed to create machine')
    }
  }

  const labelClass = 'block text-sm font-medium text-text-muted mb-1.5'

  return (
    <div>
      <div className="flex items-center gap-3 mb-6">
        <Button variant="ghost" size="sm" onClick={() => navigate(prefixed('/machines'))}>
          <ArrowLeft className="h-4 w-4" />
        </Button>
        <h1 className="text-xl font-bold text-text-primary">New Machine</h1>
      </div>

      <Card className="max-w-lg">
        <form onSubmit={(e) => void submit(e)} className="space-y-5">
          <div>
            <label className={labelClass}>Name</label>
            <Input
              type="text"
              required
              value={form.name}
              onChange={(e) => setName(e.target.value)}
              onBlur={(e) => blurName(e.target.value)}
              placeholder="worker-1"
            />
            <p className="mt-1.5 text-xs text-text-muted">
              Auto-converted to kebab-case (lowercase + hyphens)
            </p>
          </div>

          <div>
            <label className={labelClass}>Machine Type</label>
            <Select value={form.machineType} onValueChange={(v) => set('machineType', v)}>
              <SelectTrigger>
                <SelectValue placeholder="Select a machine type" />
              </SelectTrigger>
              <SelectContent>
                {profiles.map((vt) => (
                  <SelectItem key={vt.type} value={vt.type}>
                    {formatVMTypeLabel(vt)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {profiles.length === 0 && (
              <p className="mt-1.5 text-xs text-danger">
                No machine profiles are available from this compute provider.
              </p>
            )}
          </div>

          <div>
            <label className={labelClass}>Zone</label>
            <Select value={form.zone} onValueChange={(v) => set('zone', v)}>
              <SelectTrigger>
                <SelectValue placeholder="Select a zone" />
              </SelectTrigger>
              <SelectContent>
                {locations.map((z) => (
                  <SelectItem key={z} value={z}>
                    {z}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {provider !== 'gke' && <div>
            <label className={labelClass}>Disk Size</label>
            <Select value={form.diskSizeGb} onValueChange={(v) => set('diskSizeGb', v)}>
              <SelectTrigger>
                <SelectValue placeholder="Select a disk size" />
              </SelectTrigger>
              <SelectContent>
                {diskSizes.map((size) => (
                  <SelectItem key={size} value={String(size)}>
                    {size} GB
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>}

          {capabilities?.supportsInterruptible && <div className="flex items-center gap-3 py-1">
            <input
              type="checkbox"
              id="spot"
              checked={form.spot}
              onChange={(e) => set('spot', e.target.checked)}
              className="h-4 w-4 rounded border-border-strong bg-surface-overlay text-accent"
            />
            <label htmlFor="spot" className="text-sm text-text-secondary">
              Interruptible instance (provider may stop it)
            </label>
          </div>}

          {fieldError && (
            <p className="text-sm text-danger bg-danger/10 rounded-lg px-3 py-2">{fieldError}</p>
          )}

          <div className="flex gap-3 pt-3">
            <Button
              type="button"
              variant="ghost"
              size="md"
              onClick={() => navigate(prefixed('/machines'))}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              variant="primary"
              size="md"
              loading={createMachine.isPending}
            >
              Create Machine
            </Button>
          </div>
        </form>
      </Card>
    </div>
  )
}

// Mock form — minimal. Just a name; capacity is auto-detected by the
// controller from node.status.allocatable (kyber#240). The operator no
// longer picks CPU/Memory/Disk numbers — the laptop's actual hardware
// dictates what's available, with the control plane reserving a small
// carve-out for itself.
function CreateMachineMock({
  navigate,
  provider,
}: {
  navigate: ReturnType<typeof useNavigate>
  provider: 'static' | 'mock'
}) {
  const createMachine = useCreateMachine()
  const prefixed = usePrefixedPath()
  const [form, setForm] = useState<MockFormState>({ name: '' })
  const [fieldError, setFieldError] = useState<string | null>(null)

  function setName(raw: string) {
    setForm({ name: toKebabCaseTyping(raw) })
    setFieldError(null)
  }
  function blurName(raw: string) {
    setForm({ name: toKebabCase(raw) })
    setFieldError(null)
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    const err = validateMockForm(form)
    if (err) {
      setFieldError(err)
      return
    }
    try {
      // Defense-in-depth strip on name — see #189 + GCE submit comment above.
      await createMachine.mutateAsync(buildMockRequest({ ...form, name: toKebabCase(form.name) }, provider))
      navigate(prefixed('/machines'))
    } catch (err) {
      setFieldError(err instanceof Error ? err.message : 'Failed to create machine')
    }
  }

  const labelClass = 'block text-sm font-medium text-text-muted mb-1.5'

  return (
    <div>
      <div className="flex items-center gap-3 mb-6">
        <Button variant="ghost" size="sm" onClick={() => navigate(prefixed('/machines'))}>
          <ArrowLeft className="h-4 w-4" />
        </Button>
        <h1 className="text-xl font-bold text-text-primary">New Machine</h1>
      </div>

      <Card className="max-w-lg">
        <form onSubmit={(e) => void submit(e)} className="space-y-5">
          <div>
            <label className={labelClass}>Name</label>
            <Input
              type="text"
              required
              value={form.name}
              onChange={(e) => setName(e.target.value)}
              onBlur={(e) => blurName(e.target.value)}
              placeholder="local"
            />
            <p className="mt-1.5 text-xs text-text-muted">
              Auto-converted to kebab-case (lowercase + hyphens)
            </p>
          </div>

          <p className="text-xs text-text-muted leading-relaxed">
            This machine will use the full capacity of this node. The Kyber
            control plane reserves a small CPU/memory/disk carve-out (see
            helm chart <code className="font-mono">controlPlane.platformReservation</code>);
            everything else is available for agents. Additional machines must
            already have a distinct Ready Kubernetes node labelled{' '}
            <code className="font-mono">kyber.io/machine=&lt;name&gt;</code>.
          </p>

          {fieldError && (
            <p className="text-sm text-danger bg-danger/10 rounded-lg px-3 py-2">{fieldError}</p>
          )}

          <div className="flex gap-3 pt-3">
            <Button
              type="button"
              variant="ghost"
              size="md"
              onClick={() => navigate(prefixed('/machines'))}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              variant="primary"
              size="md"
              loading={createMachine.isPending}
            >
              Create Machine
            </Button>
          </div>
        </form>
      </Card>
    </div>
  )
}
