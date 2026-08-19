import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ResourcesSection } from './ResourcesSection'
import { initialWizardState } from './types'
import type { AvailableModel, Machine } from '../../lib/types'

const models: AvailableModel[] = [
  { id: 'claude-opus-4-7', displayName: 'Claude Opus 4.7', contextWindow: 1000000, contextWindowKnown: true },
  { id: 'claude-sonnet-4-6', displayName: 'Claude Sonnet 4.6', contextWindow: 1000000, contextWindowKnown: true },
]

// initialWizardState only reads .id for the seed; pass the minimal
// ModelInfo-shaped fixture to keep the existing test setup unchanged.
const initialState = () => initialWizardState(models.map((m) => ({ id: m.id, contextWindow: m.contextWindow })))

describe('ResourcesSection', () => {
  it('does not offer per-agent model overrides', () => {
    render(
      <ResourcesSection
        state={initialState()}
        set={vi.fn()}
        models={models}
        selectedMachine={null}
        machineAvailable={null}
      />,
    )
    expect(screen.queryByLabelText(/^Model$/i)).not.toBeInTheDocument()
    expect(screen.queryByLabelText(/Manual model override/i)).not.toBeInTheDocument()
  })

  it('CPU input wrapper carries data-band="green" when request well under available', () => {
    const { container } = render(
      <ResourcesSection
        state={{ ...initialWizardState(models), cpu: '0.25' }}
        set={vi.fn()}
        models={models}
        selectedMachine={null}
        machineAvailable={{ cpu: 4, memoryGi: 8, diskGi: 100 }}
      />,
    )
    const cpuWrapper = container.querySelector('[data-band="green"][data-resource="cpu"]')
    expect(cpuWrapper).not.toBeNull()
  })

  it('CPU input wrapper carries data-band="red" when request hits 90% of available', () => {
    const { container } = render(
      <ResourcesSection
        state={{ ...initialWizardState(models), cpu: '4' }}
        set={vi.fn()}
        models={models}
        selectedMachine={null}
        machineAvailable={{ cpu: 4, memoryGi: 8, diskGi: 100 }}
      />,
    )
    const cpuWrapper = container.querySelector('[data-band="red"][data-resource="cpu"]')
    expect(cpuWrapper).not.toBeNull()
  })

  it('cpu select fires the setter on change', async () => {
    const user = userEvent.setup()
    const set = vi.fn()
    render(
      <ResourcesSection
        state={initialWizardState(models)}
        set={set}
        models={models}
        selectedMachine={null}
        machineAvailable={null}
      />,
    )
    const cpuSelect = screen.getByLabelText(/cpu/i)
    await user.selectOptions(cpuSelect, '2')
    expect(set).toHaveBeenCalledWith('cpu', '2')
  })
})

const machine: Machine = {
  id: 'razer',
  spec: { provider: 'mock', machineType: 'razer-host' } as Machine['spec'],
  status: {
    phase: 'Running',
    agentCount: 1,
  } as Machine['status'],
} as unknown as Machine

describe('ResourcesSection — capacity card', () => {
  it('renders no card when state.machine is empty', () => {
    render(
      <ResourcesSection
        state={initialWizardState(models)}
        set={vi.fn()}
        models={models}
        selectedMachine={null}
        machineAvailable={null}
      />,
    )
    expect(screen.queryByTestId('wizard-capacity-card')).not.toBeInTheDocument()
    expect(screen.queryByText(/capacity unknown/)).not.toBeInTheDocument()
  })

  it('renders "capacity unknown" when machine picked but availability unknown', () => {
    render(
      <ResourcesSection
        state={{ ...initialWizardState(models), machine: 'razer' }}
        set={vi.fn()}
        models={models}
        selectedMachine={null}
        machineAvailable={null}
      />,
    )
    expect(screen.getByText(/capacity unknown/)).toBeInTheDocument()
  })

  it('renders the capacity card with verdict line when machine + availability are known', () => {
    render(
      <ResourcesSection
        state={{ ...initialWizardState(models), machine: 'razer', cpu: '1', memory: '2Gi' }}
        set={vi.fn()}
        models={models}
        selectedMachine={machine}
        machineAvailable={{ cpu: 4, memoryGi: 8, diskGi: 100 }}
      />,
    )
    expect(screen.getByTestId('wizard-capacity-card')).toBeInTheDocument()
    expect(screen.getByTestId('capacity-verdict')).toHaveTextContent(/Fits on razer/)
    expect(screen.queryByText(/won't fit/)).not.toBeInTheDocument()
  })

  it('shows the "won\'t fit" warning when request exceeds availability', () => {
    render(
      <ResourcesSection
        state={{ ...initialWizardState(models), machine: 'razer', cpu: '8', memory: '16Gi' }}
        set={vi.fn()}
        models={models}
        selectedMachine={machine}
        machineAvailable={{ cpu: 4, memoryGi: 8, diskGi: 100 }}
      />,
    )
    expect(screen.getByText(/won't fit/)).toBeInTheDocument()
  })
})
