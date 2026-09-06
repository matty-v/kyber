import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { LocalNavigation, type LocalNavigationGroup } from './LocalNavigation'

type Section = 'overview' | 'settings'

const groups: LocalNavigationGroup<Section>[] = [
  {
    label: 'Pages',
    items: [
      { id: 'overview', label: 'Overview', description: 'Current state' },
      { id: 'settings', label: 'Settings', description: 'Configuration' },
    ],
  },
]

describe('LocalNavigation', () => {
  it('closes on Escape, restores focus, and unlocks document scrolling', async () => {
    const user = userEvent.setup()
    render(
      <LocalNavigation title="Agent" value="overview" groups={groups} onChange={vi.fn()}>
        <p>Page content</p>
      </LocalNavigation>,
    )

    const trigger = screen.getByRole('button', { name: /overview: current state/i })
    await user.click(trigger)

    expect(screen.getByRole('dialog', { name: /agent navigation/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /close navigation/i })).toHaveFocus()
    expect(document.body.style.overflow).toBe('hidden')

    fireEvent.keyDown(window, { key: 'Escape' })

    expect(screen.queryByRole('dialog', { name: /agent navigation/i })).not.toBeInTheDocument()
    expect(trigger).toHaveFocus()
    expect(document.body.style.overflow).toBe('')
  })
})
