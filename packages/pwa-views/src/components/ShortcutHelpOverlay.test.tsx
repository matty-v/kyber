import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ShortcutHelpOverlay } from './ShortcutHelpOverlay'

describe('ShortcutHelpOverlay', () => {
  it('renders nothing when closed', () => {
    render(<ShortcutHelpOverlay open={false} onClose={vi.fn()} />)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('renders the cheat sheet when open and lists every binding', () => {
    render(<ShortcutHelpOverlay open={true} onClose={vi.fn()} />)
    const dialog = screen.getByRole('dialog', { name: /keyboard shortcuts/i })
    expect(dialog).toBeInTheDocument()
    // Every binding from SHORTCUT_BINDINGS appears as a <kbd>.
    for (const keys of ['g f', 'g m', 'g a', 'g s', '?', 'Esc']) {
      expect(screen.getByText(keys)).toBeInTheDocument()
    }
    // Description copy exists for at least the nav items.
    expect(screen.getByText(/go to fleet/i)).toBeInTheDocument()
    expect(screen.getByText(/go to machines/i)).toBeInTheDocument()
  })

  it('Esc fires onClose', async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    render(<ShortcutHelpOverlay open={true} onClose={onClose} />)
    await user.keyboard('{Escape}')
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('? fires onClose (toggle-off)', async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    render(<ShortcutHelpOverlay open={true} onClose={onClose} />)
    await user.keyboard('?')
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('clicking the backdrop fires onClose', async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    const { container } = render(
      <ShortcutHelpOverlay open={true} onClose={onClose} />,
    )
    // The backdrop is the absolutely-positioned div sibling of the dialog.
    const backdrop = container.querySelector('.absolute.inset-0') as HTMLElement
    expect(backdrop).not.toBeNull()
    await user.click(backdrop)
    expect(onClose).toHaveBeenCalled()
  })
})
