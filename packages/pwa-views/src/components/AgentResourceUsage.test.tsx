import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { AgentDiskPressureBadge, AgentResourceUsage } from './AgentResourceUsage'

const usage = {
  sampledAt: '2026-08-27T12:00:00Z',
  cpuUsageCores: 0.75,
  cpuLimitCores: 2,
  memoryUsedBytes: 1024 ** 3,
  memoryLimitBytes: 2 * 1024 ** 3,
  diskUsedBytes: 18 * 1024 ** 3,
  diskTotalBytes: 20 * 1024 ** 3,
  diskReserveReached: true,
  diskUsageMethod: 'directory',
  diskUsageState: 'ready',
}

describe('AgentResourceUsage', () => {
  it('shows used-of-allocated values and danger at the disk reserve', () => {
    render(<AgentResourceUsage usage={usage} />)
    expect(screen.getByText('0.75 of 2.00 cores')).toBeInTheDocument()
    expect(screen.getByText('1.0 GiB of 2.0 GiB')).toBeInTheDocument()
    expect(screen.getByText('18.0 GiB of 20.0 GiB')).toBeInTheDocument()
    expect(screen.getByTestId('resource-disk').querySelector('[data-usage-band]')).toHaveAttribute('data-usage-band', 'danger')
  })

  it('flags disk pressure at 80% and stays hidden below it', () => {
    const { rerender } = render(<AgentDiskPressureBadge usage={{ ...usage, diskUsedBytes: 16 * 1024 ** 3, diskReserveReached: false }} />)
    expect(screen.getByText('Disk 80%')).toBeInTheDocument()
    rerender(<AgentDiskPressureBadge usage={{ ...usage, diskUsedBytes: 15 * 1024 ** 3, diskReserveReached: false }} />)
    expect(screen.queryByText(/Disk/)).not.toBeInTheDocument()
  })
})
