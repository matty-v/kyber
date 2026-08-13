import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'

vi.mock('../hooks/useUpgradeProgress', () => ({
  useUpgradeProgress: vi.fn(),
}))

import * as progressModule from '../hooks/useUpgradeProgress'
import { UpgradeBanner } from './UpgradeBanner'

function mockProgress(over: Partial<progressModule.UpgradeProgress> = {}) {
  vi.mocked(progressModule.useUpgradeProgress).mockReturnValue({
    run: null,
    inFlight: false,
    reconnecting: false,
    targetVersion: null,
    ...over,
  })
}

describe('UpgradeBanner', () => {
  beforeEach(() => vi.clearAllMocks())

  it('renders nothing when no upgrade is running', () => {
    mockProgress()
    const { container } = render(<UpgradeBanner />)
    expect(container).toBeEmptyDOMElement()
  })

  it('warns that agents are losing their sessions while installing', () => {
    mockProgress({ inFlight: true, targetVersion: '1.0.4' })
    render(<UpgradeBanner />)
    expect(screen.getByTestId('upgrade-banner')).toHaveTextContent('Installing 1.0.4')
    expect(screen.getByTestId('upgrade-banner')).toHaveTextContent(
      /lose their current sessions/i,
    )
  })

  it('describes the mid-upgrade outage as expected rather than as an error', () => {
    mockProgress({ inFlight: true, reconnecting: true, targetVersion: '1.0.4' })
    render(<UpgradeBanner />)
    const banner = screen.getByTestId('upgrade-banner')
    // An operator who reads "connection lost" here starts debugging a cluster
    // that is doing exactly what they asked it to.
    expect(banner).toHaveTextContent(/this is expected/i)
    expect(banner).toHaveTextContent(/reconnect on its own/i)
  })

  it('is announced politely to assistive tech rather than as an alert', () => {
    mockProgress({ inFlight: true, targetVersion: '1.0.4' })
    render(<UpgradeBanner />)
    const banner = screen.getByTestId('upgrade-banner')
    expect(banner).toHaveAttribute('role', 'status')
    expect(banner).toHaveAttribute('aria-live', 'polite')
  })
})
