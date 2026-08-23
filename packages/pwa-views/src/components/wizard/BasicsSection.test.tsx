import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { BasicsSection } from './BasicsSection'
import { initialWizardState } from './types'
import type { WizardState } from './types'
import type { Machine } from '../../lib/types'

const machines: Machine[] = [
  {
    id: 'razer',
    phase: 'Running',
    spec: { provider: 'mock' } as Machine['spec'],
    status: {
      phase: 'Running',
      availableCapacity: { cpu: '2', memory: '4Gi' },
    } as Machine['status'],
    createdAt: '',
  } as unknown as Machine,
]

describe('BasicsSection', () => {
  it('renders the Name field and fires set("name") on change', async () => {
    const user = userEvent.setup()
    const set = vi.fn()
    render(
      <BasicsSection
        state={initialWizardState([])}
        set={set}
        machines={machines}
        agents={[]}
      />,
    )
    const nameInput = screen.getByLabelText(/name/i)
    await user.type(nameInput, 'alice')
    expect(set).toHaveBeenCalledWith('name', expect.any(String))
  })

  it('renders machine options with formatAvailLabel content', () => {
    render(
      <BasicsSection
        state={initialWizardState([])}
        set={vi.fn()}
        machines={machines}
        agents={[]}
      />,
    )
    expect(screen.getByText(/razer — 2 CPU \/ 4 GiB avail/)).toBeInTheDocument()
  })

  it('renders the soulDescription textarea bound to state.soulDescription', async () => {
    const user = userEvent.setup()
    const set = vi.fn()
    render(
      <BasicsSection
        state={{ ...initialWizardState([]), soulDescription: 'pre-filled' }}
        set={set}
        machines={machines}
        agents={[]}
      />,
    )
    const textarea = screen.getByLabelText(/description/i) as HTMLTextAreaElement
    expect(textarea.value).toBe('pre-filled')
    await user.clear(textarea)
    await user.type(textarea, 'updated')
    expect(set).toHaveBeenCalledWith('soulDescription', expect.any(String))
  })

  it('binds the optional startup prompt and enforces the UI limit', async () => {
    const user = userEvent.setup()
    const set = vi.fn()
    render(<BasicsSection state={{ ...initialWizardState([]), startupPrompt: 'Begin here' }} set={set} machines={machines} agents={[]} />)
    const textarea = screen.getByLabelText(/startup prompt/i) as HTMLTextAreaElement
    expect(textarea.value).toBe('Begin here')
    expect(textarea.maxLength).toBe(32768)
    await user.type(textarea, '!')
    expect(set).toHaveBeenCalledWith('startupPrompt', expect.any(String))
  })

  // Regression for #189: typing "my-agent" character-by-character must leave
  // the input showing "my-agent". Pre-fix, the on-keystroke toKebabCase
  // stripped trailing hyphens, eating '-' between 'y' and 'a' so the user
  // could only ever produce "myagent".
  it('preserves hyphens as the user types a name like "my-agent" (#189)', async () => {
    function Controlled() {
      const [state, setState] = useState<WizardState>(initialWizardState([]))
      const set = <K extends keyof WizardState>(key: K, value: WizardState[K]) =>
        setState((prev) => ({ ...prev, [key]: value }))
      return (
        <BasicsSection state={state} set={set} machines={machines} agents={[]} />
      )
    }
    const user = userEvent.setup()
    render(<Controlled />)
    const input = screen.getByLabelText(/name/i) as HTMLInputElement
    await user.type(input, 'my-agent')
    expect(input.value).toBe('my-agent')
  })

  // Companion test: trailing hyphen survives typing but is stripped on blur.
  it('strips trailing hyphens on blur (#189)', async () => {
    function Controlled() {
      const [state, setState] = useState<WizardState>(initialWizardState([]))
      const set = <K extends keyof WizardState>(key: K, value: WizardState[K]) =>
        setState((prev) => ({ ...prev, [key]: value }))
      return (
        <BasicsSection state={state} set={set} machines={machines} agents={[]} />
      )
    }
    const user = userEvent.setup()
    render(<Controlled />)
    const input = screen.getByLabelText(/name/i) as HTMLInputElement
    await user.type(input, 'my-')
    expect(input.value).toBe('my-')
    await user.tab() // shifts focus, fires blur
    expect(input.value).toBe('my')
  })
})
