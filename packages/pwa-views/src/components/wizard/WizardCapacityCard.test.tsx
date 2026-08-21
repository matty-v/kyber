import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { WizardCapacityCard } from './WizardCapacityCard'
import type { Machine } from '../../lib/types'

function machineWith(asn?: { cpu: string; memory: string; ephemeralStorage?: string }): Machine {
  return {
    id: 'razer',
    spec: { provider: 'mock', machineType: 'razer-host' } as Machine['spec'],
    status: {
      phase: 'Running',
      agentCount: 1,
      assignableCapacity: asn,
    } as Machine['status'],
  } as unknown as Machine
}

describe('WizardCapacityCard', () => {
  it('renders "capacity unknown" when selectedMachine is null (stale id picked)', () => {
    render(
      <WizardCapacityCard
        selectedMachine={null}
        machineAvailable={null}
        newCpu={1}
        newMemoryGi={2}
        newDiskGi={50}
      />,
    )
    expect(screen.getByText(/capacity unknown/)).toBeInTheDocument()
    expect(screen.queryByTestId('wizard-capacity-card')).not.toBeInTheDocument()
  })

  it('renders "capacity unknown" when machine picked but availability unknown', () => {
    render(
      <WizardCapacityCard
        selectedMachine={machineWith()}
        machineAvailable={null}
        newCpu={1}
        newMemoryGi={2}
        newDiskGi={50}
      />,
    )
    expect(screen.getByText(/capacity unknown/)).toBeInTheDocument()
    expect(screen.queryByTestId('wizard-capacity-card')).not.toBeInTheDocument()
  })

  it('renders CPU + Memory + Disk bars + verdict when fits', () => {
    render(
      <WizardCapacityCard
        selectedMachine={machineWith({ cpu: '4', memory: '8Gi', ephemeralStorage: '190Gi' })}
        machineAvailable={{ cpu: 3, memoryGi: 6, diskGi: 140 }}
        newCpu={1}
        newMemoryGi={2}
        newDiskGi={50}
      />,
    )
    expect(screen.getByTestId('proposed-bar-cpu')).toBeInTheDocument()
    expect(screen.getByTestId('proposed-bar-memory')).toBeInTheDocument()
    expect(screen.getByTestId('proposed-bar-disk')).toBeInTheDocument()
    // CPU: usedByOthers = 4 - 3 = 1; new = 1 → "1.00 + 1.00 = 2.00 / 4.00"
    expect(screen.getByText(/1\.00 \+ 1\.00 = 2\.00 \/ 4\.00/)).toBeInTheDocument()
    // Memory: usedByOthers = 8 - 6 = 2; new = 2 → "2.0 + 2.0 = 4.0 / 8.0 GiB"
    expect(screen.getByText(/2\.0 \+ 2\.0 = 4\.0 \/ 8\.0 GiB/)).toBeInTheDocument()
    // Disk: usedByOthers = 190 - 140 = 50; new = 50 → "50.0 + 50.0 = 100.0 / 190.0 GiB"
    expect(screen.getByText(/50\.0 \+ 50\.0 = 100\.0 \/ 190\.0 GiB/)).toBeInTheDocument()
    // Verdict
    expect(screen.getByTestId('capacity-verdict')).toHaveTextContent(/Fits on razer/)
    expect(screen.getByTestId('capacity-verdict')).toHaveTextContent(/2\.00 vCPU, 4\.0 GiB memory and 90\.0 GiB disk will remain/)
  })

  it('marks card as exceeds + verdict references CPU when CPU overflows', () => {
    render(
      <WizardCapacityCard
        selectedMachine={machineWith({ cpu: '4', memory: '8Gi', ephemeralStorage: '190Gi' })}
        machineAvailable={{ cpu: 1, memoryGi: 6, diskGi: 140 }}
        newCpu={4}
        newMemoryGi={1}
        newDiskGi={50}
      />,
    )
    const card = screen.getByTestId('wizard-capacity-card')
    expect(card).toHaveAttribute('data-exceeds', 'true')
    expect(screen.getByTestId('capacity-verdict')).toHaveTextContent(/won't fit — reduce CPU /)
  })

  it('marks card as exceeds + verdict references memory when memory overflows', () => {
    render(
      <WizardCapacityCard
        selectedMachine={machineWith({ cpu: '4', memory: '8Gi', ephemeralStorage: '190Gi' })}
        machineAvailable={{ cpu: 3, memoryGi: 1, diskGi: 140 }}
        newCpu={1}
        newMemoryGi={4}
        newDiskGi={50}
      />,
    )
    expect(screen.getByTestId('capacity-verdict')).toHaveTextContent(/won't fit — reduce memory /)
  })

  it('marks card as exceeds + verdict references disk when disk overflows', () => {
    render(
      <WizardCapacityCard
        selectedMachine={machineWith({ cpu: '4', memory: '8Gi', ephemeralStorage: '190Gi' })}
        machineAvailable={{ cpu: 3, memoryGi: 6, diskGi: 40 }}
        newCpu={1}
        newMemoryGi={2}
        newDiskGi={50}
      />,
    )
    expect(screen.getByTestId('capacity-verdict')).toHaveTextContent(/won't fit — reduce disk /)
  })

  it('verdict references all three when CPU + memory + disk overflow', () => {
    render(
      <WizardCapacityCard
        selectedMachine={machineWith({ cpu: '4', memory: '8Gi', ephemeralStorage: '190Gi' })}
        machineAvailable={{ cpu: 1, memoryGi: 1, diskGi: 10 }}
        newCpu={4}
        newMemoryGi={4}
        newDiskGi={50}
      />,
    )
    expect(screen.getByTestId('capacity-verdict')).toHaveTextContent(
      /won't fit — reduce CPU, memory and disk /,
    )
  })

  it('verdict references both when CPU + memory overflow (no disk)', () => {
    render(
      <WizardCapacityCard
        selectedMachine={machineWith({ cpu: '4', memory: '8Gi', ephemeralStorage: '190Gi' })}
        machineAvailable={{ cpu: 1, memoryGi: 1, diskGi: 140 }}
        newCpu={4}
        newMemoryGi={4}
        newDiskGi={50}
      />,
    )
    expect(screen.getByTestId('capacity-verdict')).toHaveTextContent(
      /won't fit — reduce CPU and memory /,
    )
  })

  it('falls back to availability-as-total when assignableCapacity is missing', () => {
    render(
      <WizardCapacityCard
        selectedMachine={machineWith()} // no assignableCapacity
        machineAvailable={{ cpu: 4, memoryGi: 8, diskGi: 100 }}
        newCpu={1}
        newMemoryGi={2}
        newDiskGi={50}
      />,
    )
    // total = 4 (== available), usedByOthers = 0, new = 1
    expect(screen.getByText(/0\.00 \+ 1\.00 = 1\.00 \/ 4\.00/)).toBeInTheDocument()
    // Disk: total = 100 (from availability), usedByOthers = 0, new = 50
    expect(screen.getByText(/0\.0 \+ 50\.0 = 50\.0 \/ 100\.0 GiB/)).toBeInTheDocument()
  })

	it('treats zero disk on a pending machine as unknown instead of an overflow', () => {
		render(
			<WizardCapacityCard
				selectedMachine={machineWith({ cpu: '8', memory: '32Gi', ephemeralStorage: '0' })}
				machineAvailable={{ cpu: 8, memoryGi: 32, diskGi: 0 }}
				newCpu={1}
				newMemoryGi={2}
				newDiskGi={50}
			/>,
		)
		const card = screen.getByTestId('wizard-capacity-card')
		expect(card).not.toHaveAttribute('data-exceeds')
		expect(screen.queryByTestId('proposed-bar-disk')).not.toBeInTheDocument()
		expect(screen.getByTestId('proposed-disk-unknown')).toHaveTextContent(/available after node provisioning/)
		expect(screen.getByTestId('capacity-verdict')).toHaveTextContent(
			/CPU and memory fit on razer. Disk capacity will be checked after node provisioning./,
		)
	})
})
