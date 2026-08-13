import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import type { UpdateStatus } from '../lib/types'

const setPolicyMutate = vi.fn()
const applyMutate = vi.fn()
const checkMutate = vi.fn()

vi.mock('../hooks/useAPI', () => ({
  useUpdates: vi.fn(),
  useSetUpdatePolicy: vi.fn(() => ({ mutate: setPolicyMutate, isPending: false })),
  useCheckUpdates: vi.fn(() => ({ mutate: checkMutate, isPending: false })),
  useApplyUpdate: vi.fn(() => ({ mutate: applyMutate, isPending: false })),
  // The install confirmation names the agents that will lose their sessions.
  useAgents: vi.fn(() => ({ data: [], isLoading: false, error: null })),
}))

import * as useAPIModule from '../hooks/useAPI'
import { UpdatesCard } from './UpdatesCard'

function status(over: Partial<UpdateStatus> = {}): UpdateStatus {
  return {
    currentVersion: '1.0.1',
    latestVersion: '1.0.2',
    updateAvailable: true,
    policy: { channel: 'stable', mode: 'notify' },
    managedBy: 'helm',
    canSelfUpgrade: true,
    applySupported: true,
    ...over,
  }
}

function mockUpdates(ret: unknown) {
  vi.mocked(useAPIModule.useUpdates).mockReturnValue(ret as ReturnType<typeof useAPIModule.useUpdates>)
}

describe('UpdatesCard', () => {
  beforeEach(() => vi.clearAllMocks())

  it('shows what is running and what is available', () => {
    mockUpdates({ data: status(), isLoading: false, error: null })
    render(<UpdatesCard />)

    expect(screen.getByText('1.0.1')).toBeInTheDocument()
    expect(screen.getByText('1.0.2')).toBeInTheDocument()
    expect(screen.getByText('update available')).toBeInTheDocument()
  })

  // Matt's requirement: releases are the default, tracking main is opt-in.
  it('defaults to published releases, not every change', () => {
    mockUpdates({ data: status(), isLoading: false, error: null })
    render(<UpdatesCard />)

    const releases = screen.getByRole('radio', { name: /Published releases/ })
    const everyChange = screen.getByRole('radio', { name: /Every change as it lands/ })
    expect(releases).toBeChecked()
    expect(everyChange).not.toBeChecked()
  })

  // ...and opting in has to say what you are taking on, before it takes effect.
  it('warns before switching to every-change, and only saves on confirm', () => {
    mockUpdates({ data: status(), isLoading: false, error: null })
    render(<UpdatesCard />)

    fireEvent.click(screen.getByRole('radio', { name: /Every change as it lands/ }))
    // Nothing saved yet — the dialog is the gate.
    expect(setPolicyMutate).not.toHaveBeenCalled()
    expect(screen.getByText(/no release, no soak/)).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /I understand/ }))
    expect(setPolicyMutate).toHaveBeenCalledWith({ channel: 'main' })
  })

  it('switching back to releases needs no confirmation', () => {
    mockUpdates({ data: status({ policy: { channel: 'main', mode: 'notify' } }), isLoading: false, error: null })
    render(<UpdatesCard />)

    fireEvent.click(screen.getByRole('radio', { name: /Published releases/ }))
    expect(setPolicyMutate).toHaveBeenCalledWith({ channel: 'stable' })
  })

  it('keeps the stability warning visible while on every-change', () => {
    mockUpdates({ data: status({ policy: { channel: 'main', mode: 'notify' } }), isLoading: false, error: null })
    render(<UpdatesCard />)

    expect(screen.getByText(/follows unreleased code/)).toBeInTheDocument()
  })

  it('installs on demand and never on its own', () => {
    mockUpdates({ data: status(), isLoading: false, error: null })
    render(<UpdatesCard />)

    expect(screen.getByText(/Updates are never installed for you/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /Install 1\.0\.2/ }))

    // Install opens a confirmation rather than upgrading on the first click:
    // the consequence (every agent restarts and loses its session) is invisible
    // from the button itself.
    expect(applyMutate).not.toHaveBeenCalled()
    expect(screen.getByText(/This restarts the whole cluster/)).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /^Install$/ }))
    expect(applyMutate).toHaveBeenCalled()
  })

  it('names the agents that will lose their sessions', () => {
    vi.mocked(useAPIModule.useAgents).mockReturnValue({
      data: [
        { id: 'lando', phase: 'Running' },
        { id: 'yoda', phase: 'Stopped' },
      ],
      isLoading: false,
      error: null,
    } as unknown as ReturnType<typeof useAPIModule.useAgents>)
    mockUpdates({ data: status(), isLoading: false, error: null })
    render(<UpdatesCard />)

    fireEvent.click(screen.getByRole('button', { name: /Install 1\.0\.2/ }))

    // Only the live one is counted. A Stopped agent restarting costs nothing,
    // and including it would overstate the blast radius.
    expect(screen.getByText('lando')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Install — restart 1 agent$/ })).toBeInTheDocument()
  })

  it('installs the version it named, not whatever is latest at request time', () => {
    // Set explicitly: clearAllMocks does not undo a mockReturnValue, so an
    // earlier test's agent list would otherwise change this confirm label.
    vi.mocked(useAPIModule.useAgents).mockReturnValue({
      data: [],
      isLoading: false,
      error: null,
      isError: false,
    } as unknown as ReturnType<typeof useAPIModule.useAgents>)
    mockUpdates({ data: status(), isLoading: false, error: null })
    render(<UpdatesCard />)
    fireEvent.click(screen.getByRole('button', { name: /Install 1\.0\.2/ }))
    fireEvent.click(screen.getByRole('button', { name: /^Install$/ }))
    // Sending undefined lets the server resolve latestVersion again, so a check
    // landing between render and confirm would install something else.
    expect(applyMutate).toHaveBeenCalledWith('1.0.2')
  })

  it('says it could not check the agents rather than implying none are at risk', () => {
    vi.mocked(useAPIModule.useAgents).mockReturnValue({
      data: undefined,
      isLoading: false,
      error: new Error('boom'),
      isError: true,
    } as unknown as ReturnType<typeof useAPIModule.useAgents>)
    mockUpdates({ data: status(), isLoading: false, error: null })
    render(<UpdatesCard />)
    fireEvent.click(screen.getByRole('button', { name: /Install 1\.0\.2/ }))
    expect(screen.getByText(/Couldn't check which agents are running/)).toBeInTheDocument()
  })

  it('counts agents that are still booting — they are rolled too', () => {
    vi.mocked(useAPIModule.useAgents).mockReturnValue({
      data: [
        { id: 'lando', phase: 'Running' },
        { id: 'yoda', phase: 'Starting' },
        { id: 'han', phase: 'Stopped' },
      ],
      isLoading: false,
      error: null,
      isError: false,
    } as unknown as ReturnType<typeof useAPIModule.useAgents>)
    mockUpdates({ data: status(), isLoading: false, error: null })
    render(<UpdatesCard />)
    fireEvent.click(screen.getByRole('button', { name: /Install 1\.0\.2/ }))
    expect(screen.getByRole('button', { name: /Install — restart 2 agents$/ })).toBeInTheDocument()
  })

  it('offers no install button when there is nothing newer', () => {
    mockUpdates({
      data: status({ updateAvailable: false, latestVersion: '1.0.1' }),
      isLoading: false,
      error: null,
    })
    render(<UpdatesCard />)

    expect(screen.getByRole('button', { name: /Install/ })).toBeDisabled()
  })

  // The three different "no" states must not collapse into one message.
  it('explains an ArgoCD-managed cluster rather than hiding the feature', () => {
    mockUpdates({
      data: status({
        canSelfUpgrade: false,
        managedBy: 'argocd',
        reason: 'This cluster is managed by ArgoCD.',
      }),
      isLoading: false,
      error: null,
    })
    render(<UpdatesCard />)

    expect(screen.getByText(/managed by ArgoCD/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Install/ })).not.toBeInTheDocument()
  })

  it('explains an install with no apply path', () => {
    mockUpdates({ data: status({ applySupported: false }), isLoading: false, error: null })
    render(<UpdatesCard />)

    expect(screen.getByText(/cannot install updates itself/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Install/ })).not.toBeInTheDocument()
  })

  it('shows a hold and offers to clear it', () => {
    mockUpdates({
      data: status({ policy: { channel: 'stable', mode: 'notify', pinnedVersion: '1.0.1' } }),
      isLoading: false,
      error: null,
    })
    render(<UpdatesCard />)

    expect(screen.getByText(/Held at/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Clear' }))
    expect(setPolicyMutate).toHaveBeenCalledWith({ pinnedVersion: null })
  })

  it('surfaces a failed check instead of looking up to date', () => {
    mockUpdates({
      data: status({ updateAvailable: false, lastError: 'registry unreachable' }),
      isLoading: false,
      error: null,
    })
    render(<UpdatesCard />)

    expect(screen.getByText(/registry unreachable/)).toBeInTheDocument()
  })

  it('reports a running install', () => {
    mockUpdates({
      data: status({
        lastRun: { jobName: 'kyber-upgrade-1-0-2', targetVersion: '1.0.2', phase: 'running' },
      }),
      isLoading: false,
      error: null,
    })
    render(<UpdatesCard />)

    expect(screen.getByRole('button', { name: /Installing/ })).toBeDisabled()
  })

  it('says so when update checking is switched off entirely', () => {
    mockUpdates({ data: undefined, isLoading: false, error: new Error('503') })
    render(<UpdatesCard />)

    expect(screen.getByText(/not enabled on this control plane/)).toBeInTheDocument()
  })
})
