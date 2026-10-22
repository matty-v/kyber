import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { mockAgent } from '../mocks/fixtures'
import { AgentDiskControl } from './AgentDiskControl'

const { options, increase } = vi.hoisted(() => ({
  options: vi.fn(),
  increase: vi.fn(async () => ({ targetGiB: 50, stage: 'Requested' })),
}))

vi.mock('../hooks/useAPI', () => ({
  useAgentDiskOptions: () => options(),
  useIncreaseAgentDisk: () => ({ mutateAsync: increase, isPending: false, isError: false }),
}))

describe('AgentDiskControl', () => {
  it('offers only a larger whole-GiB target and confirms the restart', async () => {
    options.mockReturnValue({ data: { supported: true, currentTargetGiB: 20, pvcRequestGiB: 20, pvCapacityGiB: 20,
      stepGiB: 1, minimumTargetGiB: 21, suggestedTargetsGiB: [30, 50] }, isError: false })
    const user = userEvent.setup()
    render(<AgentDiskControl agent={{ ...mockAgent, resources: { ...mockAgent.resources, disk: '20Gi' } }} />)
    await user.click(screen.getByRole('button', { name: 'Increase disk' }))
    const input = screen.getByRole('spinbutton', { name: /New size/i })
    expect(screen.getByText(/restart this Agent once/)).toBeInTheDocument()
    await user.clear(input)
    await user.type(input, '20')
    expect(screen.getByRole('button', { name: 'Increase to 20 GiB' })).toBeDisabled()
    await user.clear(input)
    await user.type(input, '30.5')
    expect(screen.getByRole('button', { name: 'Increase to 30.5 GiB' })).toBeDisabled()
    await user.clear(input)
    await user.type(input, '50')
    await user.click(screen.getByRole('button', { name: 'Increase to 50 GiB' }))
    expect(increase).toHaveBeenCalledWith({ name: mockAgent.id, sizeGiB: 50 })
  })

  it('does not offer the action for unsupported storage', () => {
    options.mockReturnValue({ data: { supported: false, reason: 'Local storage', currentTargetGiB: 20,
      stepGiB: 1, minimumTargetGiB: 21, suggestedTargetsGiB: [] }, isError: false })
    render(<AgentDiskControl agent={mockAgent} />)
    expect(screen.getByRole('button', { name: 'Increase disk' })).toBeDisabled()
    expect(screen.getByText(/Increase unavailable: Local storage/)).toBeInTheDocument()
  })
})
