import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { SecretsHelpContent } from './SecretsHelpContent'

describe('SecretsHelpContent', () => {
  it('renders all three section labels: Format, Updates, Limits', () => {
    render(<SecretsHelpContent />)
    expect(screen.getByText('Format')).toBeInTheDocument()
    expect(screen.getByText('Updates')).toBeInTheDocument()
    expect(screen.getByText('Limits')).toBeInTheDocument()
  })

  it('describes the key/value env var convention', () => {
    render(<SecretsHelpContent />)
    // The env-var format is described as "$USER_<KEY>" inline code.
    expect(screen.getByText('$USER_<KEY>')).toBeInTheDocument()
  })

  it('describes the file mount path convention', () => {
    render(<SecretsHelpContent />)
    expect(screen.getByText('/user-secrets/<key>.bin')).toBeInTheDocument()
  })

  it('mentions the 256 KiB aggregate limit', () => {
    render(<SecretsHelpContent />)
    expect(screen.getByText(/256 KiB/)).toBeInTheDocument()
  })
})
