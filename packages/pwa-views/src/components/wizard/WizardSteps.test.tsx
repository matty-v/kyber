import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { WizardSteps } from './WizardSteps'
import { initialWizardState } from './types'
import type { WizardState } from './types'

const fullyValid: WizardState = {
  ...initialWizardState([]),
  name: 'alice',
  machine: 'razer',
  identityRepoMode: 'template',
  authType: 'api-key',
  anthropicApiKey: 'sk-ant-xxx',
}

describe('WizardSteps', () => {
  it('renders 5 chip buttons in id order with correct labels', () => {
    render(
      <WizardSteps state={fullyValid} activeStep={1} onStepClick={vi.fn()} />,
    )
    const buttons = screen.getAllByRole('button')
    expect(buttons.length).toBe(5)
    expect(buttons[0]).toHaveTextContent(/Basics/)
    expect(buttons[1]).toHaveTextContent(/Resources/)
    expect(buttons[2]).toHaveTextContent(/Identity/)
    expect(buttons[3]).toHaveTextContent(/Auth/)
    expect(buttons[4]).toHaveTextContent(/Review/)
  })

  it('marks the active chip with aria-current="step"', () => {
    render(
      <WizardSteps state={fullyValid} activeStep={3} onStepClick={vi.fn()} />,
    )
    const buttons = screen.getAllByRole('button')
    expect(buttons[2]).toHaveAttribute('aria-current', 'step')
    expect(buttons[0]).not.toHaveAttribute('aria-current', 'step')
  })

  it('clicking a chip calls onStepClick with that chip\'s id', async () => {
    const user = userEvent.setup()
    const onStepClick = vi.fn()
    render(
      <WizardSteps state={fullyValid} activeStep={1} onStepClick={onStepClick} />,
    )
    await user.click(screen.getByRole('button', { name: /Resources/ }))
    expect(onStepClick).toHaveBeenCalledWith(2)
  })

  it('disables future chips when an earlier step is invalid', async () => {
    const user = userEvent.setup()
    const onStepClick = vi.fn()
    render(
      <WizardSteps state={initialWizardState([])} activeStep={1} onStepClick={onStepClick} />,
    )
    const resourcesChip = screen.getByRole('button', { name: /Resources/ })
    expect(resourcesChip).toHaveAttribute('aria-disabled', 'true')
    expect(resourcesChip).toBeDisabled()
    await user.click(resourcesChip)
    expect(onStepClick).not.toHaveBeenCalled()
  })

  it('enables a future chip once all earlier steps are valid', () => {
    const partiallyValid: WizardState = {
      ...initialWizardState([]),
      name: 'alice',
      machine: 'razer',
      identityRepoMode: 'template',
    }
    render(
      <WizardSteps state={partiallyValid} activeStep={1} onStepClick={vi.fn()} />,
    )
    const resourcesChip = screen.getByRole('button', { name: /Resources/ })
    const identityChip = screen.getByRole('button', { name: /Identity/ })
    const authChip = screen.getByRole('button', { name: /Auth/ })
    const reviewChip = screen.getByRole('button', { name: /Review/ })
    expect(resourcesChip).not.toBeDisabled()
    expect(identityChip).not.toBeDisabled()
    expect(authChip).not.toBeDisabled()
    expect(reviewChip).toBeDisabled()
  })

  it('past chips (id < activeStep) are always clickable for revisits', async () => {
    const user = userEvent.setup()
    const onStepClick = vi.fn()
    render(
      <WizardSteps state={fullyValid} activeStep={3} onStepClick={onStepClick} />,
    )
    await user.click(screen.getByRole('button', { name: /Basics/ }))
    expect(onStepClick).toHaveBeenCalledWith(1)
  })
})
