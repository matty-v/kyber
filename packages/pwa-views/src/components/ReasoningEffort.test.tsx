import { render, screen, fireEvent } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import { EffortSelect } from './ReasoningEffort'

// Native option strings stay data; the view never maps one provider's level to
// another. The backend remains the authority for any write.
describe('Reasoning effort selector', () => {
  it('offers Default and only the qualified native choices', () => {
    const change = vi.fn()
    render(<EffortSelect value="" onChange={change} options={{ state: 'supported', levels: ['low', 'high', 'ultra'], runtime: 'codex', runtimeVersion: '0.146.0', model: 'gpt-5.6-sol' }} />)
    const selector = screen.getByRole('combobox', { name: 'Reasoning effort' })
    expect(screen.getAllByRole('option').map(option => option.textContent)).toEqual(['Default (no override)', 'low', 'high', 'ultra'])
    fireEvent.change(selector, { target: { value: 'ultra' } })
    expect(change).toHaveBeenCalledWith('ultra')
  })
  it('keeps an unsupported configured value visible and allows clearing during an outage', () => {
    const change = vi.fn()
    render(<EffortSelect value="high" onChange={change} unavailable />)
    const selector = screen.getByRole('combobox', { name: 'Reasoning effort' })
    expect(selector).toHaveValue('high')
    expect(screen.getByRole('option', { name: 'high (not available)' })).toBeDisabled()
    expect(screen.getByRole('option', { name: 'Default (no override)' })).not.toBeDisabled()
    fireEvent.change(selector, { target: { value: '' } })
    expect(change).toHaveBeenCalledWith('')
  })
  it('does not invent effort choices for unknown support', () => {
    render(<EffortSelect value="" onChange={vi.fn()} options={{ state: 'unknown', levels: [], runtime: 'codex', runtimeVersion: 'future', model: 'future' }} />)
    expect(screen.getAllByRole('option')).toHaveLength(1)
    expect(screen.getByText('Only Default is available for this combination.')).toBeInTheDocument()
  })
})
