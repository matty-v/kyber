import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { DropdownMenu, DropdownMenuContent } from '../components/ui/dropdown-menu'
import { LifecycleMenuItems, MismatchBadges, StatusCardBody } from './AgentDetail'
import type { Agent, AgentPhase, AgentStatus } from '../lib/types'

// AgentDetail.test.tsx — kyber#355 regression coverage for the Status
// card body. Before this fix, AgentDetail's Status card had no
// empty-state branch and four optional rows — when the API returned only
// `{phase: "Running"}` (which the v1.3.1 controller did for every running
// agent), the body was an empty <dl> and the card rendered as a bare
// "Status" heading. These tests lock the contract:
//
//   - full status payload → all five rows render
//   - only `phase` set → empty-state row with pending-data copy
//   - partial fields (e.g., podName set, podIP missing) → only the rows
//     for the populated fields render, no spurious "undefined" output

describe('AgentDetail StatusCardBody', () => {
  it('renders all rows when full status payload is present', () => {
    const status: AgentStatus = {
      phase: 'Running',
      podName: 'boba-fett-xyz',
      podIP: '10.244.3.21',
      nodeName: 'gke-node-7',
      startTime: '2026-05-27T10:00:00Z',
      restartCount: 0,
      message: 'Heartbeat received 2s ago',
    }
    render(<StatusCardBody status={status} />)

    expect(screen.getByText('boba-fett-xyz')).toBeInTheDocument()
    expect(screen.getByText('10.244.3.21')).toBeInTheDocument()
    expect(screen.getByText('gke-node-7')).toBeInTheDocument()
    // Started row label present; the formatted timestamp text varies by
    // locale, so assert the label rather than the date string.
    expect(screen.getByText('Started')).toBeInTheDocument()
    expect(screen.getByText('Restarts')).toBeInTheDocument()
    expect(screen.getByText('0')).toBeInTheDocument()
    expect(screen.getByText('Heartbeat received 2s ago')).toBeInTheDocument()

    // Empty-state must NOT show when any field is populated.
    expect(screen.queryByTestId('status-empty-state')).not.toBeInTheDocument()
  })

  it('renders empty-state row when only status.phase is set', () => {
    // This is the v1.3.1 reality the kyber#355 fix exists to resolve:
    // controller wrote nothing pod-derived; UI rendered a bare heading.
    const status: AgentStatus = { phase: 'Running' }
    render(<StatusCardBody status={status} />)

    const empty = screen.getByTestId('status-empty-state')
    expect(empty).toBeInTheDocument()
    // Pending-data wording style mirrors MachineDetail.tsx's EmptyState
    // copy ("X will appear …") so the empty card teaches "not yet
    // available" instead of "feature broken."
    expect(empty.textContent).toMatch(/Pod is starting/i)
    expect(empty.textContent).toMatch(/will appear/i)

    // No data rows rendered.
    expect(screen.queryByText('Pod')).not.toBeInTheDocument()
    expect(screen.queryByText('Pod IP')).not.toBeInTheDocument()
    expect(screen.queryByText('Node')).not.toBeInTheDocument()
  })

  it('renders only the rows that have data when payload is partial', () => {
    // Mid-startup: pod scheduled (NodeName set) but no IP assigned yet,
    // controller may have populated PodName but kubelet hasn't returned
    // StartTime. UI must tolerate any subset.
    const status: AgentStatus = {
      phase: 'Running',
      podName: 'boba-fett-xyz',
      nodeName: 'gke-node-7',
    }
    render(<StatusCardBody status={status} />)

    expect(screen.getByText('boba-fett-xyz')).toBeInTheDocument()
    expect(screen.getByText('gke-node-7')).toBeInTheDocument()

    // PodIP, Started, Restarts, Message rows must be absent.
    expect(screen.queryByText('Pod IP')).not.toBeInTheDocument()
    expect(screen.queryByText('Started')).not.toBeInTheDocument()
    expect(screen.queryByText('Restarts')).not.toBeInTheDocument()

    // Empty-state must NOT show — we have *some* data.
    expect(screen.queryByTestId('status-empty-state')).not.toBeInTheDocument()
  })

  it('renders Restarts row even when restartCount is 0', () => {
    // restartCount=0 is meaningful information (agent is stable). Only
    // hide the row when the field is undefined — `restartCount !== undefined`.
    const status: AgentStatus = { phase: 'Running', restartCount: 0 }
    render(<StatusCardBody status={status} />)
    expect(screen.getByText('Restarts')).toBeInTheDocument()
    expect(screen.getByText('0')).toBeInTheDocument()
  })

  it('falls back gracefully when only message is set', () => {
    // Edge case: a Failed agent may carry only `phase` + `message`. The
    // message row should render; no empty-state.
    const status: AgentStatus = { phase: 'Failed', message: 'OAuth refresh failed' }
    render(<StatusCardBody status={status} />)
    expect(screen.getByText('OAuth refresh failed')).toBeInTheDocument()
    expect(screen.queryByTestId('status-empty-state')).not.toBeInTheDocument()
  })
})

// kyber#379 / PR-E — mismatch safety net badges. These tests pin the
// contract that a healthy agent surfaces NOTHING (so the page isn't
// cluttered with green checkmarks), and an agent in either failure mode
// renders an operator-readable warning with the remedy hint.
describe('AgentDetail MismatchBadges', () => {
  function baseAgent(partial: Partial<Agent> = {}): Agent {
    return {
      name: 'test-agent',
      machine: 'node-01',
      model: 'claude-sonnet-4-5',
      runtime: 'claude-code',
      resources: { cpu: '1', memory: '2Gi', disk: '50Gi' },
      secrets: { authType: 'oauth' },
      status: { phase: 'Running' },
      createdAt: '2026-05-29T00:00:00Z',
      ...partial,
    } as Agent
  }

  it('renders nothing when both flags are false', () => {
    const agent = baseAgent({
      runtimeVersionMismatch: false,
      modelUnsupported: false,
    })
    const { container } = render(<MismatchBadges agent={agent} />)
    expect(container.firstChild).toBeNull()
  })

  it('renders nothing when flags are absent (healthy agent)', () => {
    const agent = baseAgent()
    const { container } = render(<MismatchBadges agent={agent} />)
    expect(container.firstChild).toBeNull()
  })

  it('renders RuntimeVersionMismatch badge with installed+requested versions', () => {
    const agent = baseAgent({
      runtimeVersionMismatch: true,
      runtimeVersion: {
        installedVersion: '2.0.99',
        requestedVersion: '2.1.119',
      },
    })
    render(<MismatchBadges agent={agent} />)
    expect(screen.getByText(/Runtime version mismatch/i)).toBeInTheDocument()
    // Both versions must appear so the operator can read the gap at a glance.
    expect(screen.getByText('2.0.99')).toBeInTheDocument()
    expect(screen.getByText('2.1.119')).toBeInTheDocument()
    expect(screen.getByText(/restart the agent/i)).toBeInTheDocument()
  })

  it('renders RuntimeVersionMismatch badge with generic copy when versions are absent', () => {
    // Edge case: the flag is True but the API didn't surface the version
    // details (older sidecar). The badge still shows + the remedy hint
    // points at "restart" — the operator-actionable bit.
    const agent = baseAgent({ runtimeVersionMismatch: true })
    render(<MismatchBadges agent={agent} />)
    expect(screen.getByText(/Runtime version mismatch/i)).toBeInTheDocument()
    expect(screen.getByText(/restart the agent/i)).toBeInTheDocument()
  })

  it('renders ModelUnsupported badge with model name + remedy hint', () => {
    const agent = baseAgent({
      modelUnsupported: true,
      model: 'claude-fictional-9',
      runtimeVersion: { installedVersion: '2.1.119' },
    })
    render(<MismatchBadges agent={agent} />)
    expect(screen.getByText(/Model rejected/i)).toBeInTheDocument()
    expect(screen.getByText('claude-fictional-9')).toBeInTheDocument()
    // Remedy hint must lead with the common cause (a wrong model id —
    // including one inherited from the fleet default) before the
    // runtime-version path.
    const body = screen.getByText(/Check the model id first/i)
    expect(body).toBeInTheDocument()
  })

  it('renders an inconclusive-probe warning when the probe failed without a verdict', () => {
    // modelUnsupported stays false (condition is Unknown, not True), but
    // the diagnostic is present — "couldn't verify" must be visible in
    // the console, not only in the CRD.
    const agent = baseAgent({
      modelUnsupported: false,
      runtimeVersion: {
        installedVersion: '2.1.240',
        modelProbeMessage: 'Invalid bearer token. Please run /login.',
      },
    })
    render(<MismatchBadges agent={agent} />)
    expect(screen.getByText(/Model check inconclusive/i)).toBeInTheDocument()
    expect(screen.getByText(/Invalid bearer token/)).toBeInTheDocument()
  })

  it('does not render the inconclusive warning on a definite rejection', () => {
    const agent = baseAgent({
      modelUnsupported: true,
      runtimeVersion: {
        installedVersion: '2.1.240',
        modelSupported: false,
        modelProbeMessage: 'no such model: claude-x',
      },
    })
    render(<MismatchBadges agent={agent} />)
    expect(screen.getByText(/Model rejected/i)).toBeInTheDocument()
    expect(screen.queryByText(/Model check inconclusive/i)).not.toBeInTheDocument()
  })

  it('renders the probe diagnostic when the report carries one', () => {
    // The probe message names the rejected model even when the agent
    // inherits the fleet default (agent.model empty) — the case that
    // used to be fully silent (canary regression 2026-08-22).
    const agent = baseAgent({
      modelUnsupported: true,
      model: '',
      runtimeVersion: {
        installedVersion: '2.1.240',
        modelProbeMessage:
          "There's an issue with the selected model (claude-opus-4-canary-marker). It may not exist or you may not have access to it.",
      },
    })
    render(<MismatchBadges agent={agent} />)
    expect(screen.getByText(/claude-opus-4-canary-marker/)).toBeInTheDocument()
  })

  // --- kyber#674: blocked-before-pod conditions -------------------------
  //
  // These differ from the two badges above: they mean NO pod exists at all, so
  // the agent shows a blank status and a restart cannot help. HK-47 sat in
  // exactly this state in production with nothing on screen explaining it.

  it('renders RuntimeImageMissing badge naming the runtime and pointing at the install', () => {
    const agent = baseAgent({ runtimeImageMissing: true, runtime: 'codex' })
    render(<MismatchBadges agent={agent} />)
    expect(screen.getByText(/can't run the/i)).toBeInTheDocument()
    expect(screen.getByText('codex')).toBeInTheDocument()
    // Must steer away from the useless action (restart) toward the real one.
    expect(screen.getByText(/Restarting the agent will not help/i)).toBeInTheDocument()
    expect(screen.getByText(/Helm values/i)).toBeInTheDocument()
  })

  it('prefers the controller-computed remediation verbatim when present', () => {
    const agent = baseAgent({
      runtimeImageMissing: true,
      runtime: 'codex',
      blockedReason: 'set image.codex.tag in the install Helm values',
    })
    render(<MismatchBadges agent={agent} />)
    expect(screen.getByText(/set image\.codex\.tag/i)).toBeInTheDocument()
  })

  it('renders ModelUnresolved badge', () => {
    const agent = baseAgent({ modelUnresolved: true })
    render(<MismatchBadges agent={agent} />)
    expect(screen.getByText(/No model resolved/i)).toBeInTheDocument()
  })

  it('stays silent for a healthy agent on the new flags too', () => {
    const agent = baseAgent({ runtimeImageMissing: false, modelUnresolved: false })
    const { container } = render(<MismatchBadges agent={agent} />)
    expect(container.firstChild).toBeNull()
  })

  it('renders both badges when both signals are True', () => {
    const agent = baseAgent({
      runtimeVersionMismatch: true,
      modelUnsupported: true,
      model: 'claude-fictional-9',
      runtimeVersion: { installedVersion: '2.0.99', requestedVersion: '2.1.119' },
    })
    render(<MismatchBadges agent={agent} />)
    expect(screen.getByText(/Runtime version mismatch/i)).toBeInTheDocument()
    expect(screen.getByText(/Model rejected/i)).toBeInTheDocument()
  })
})

// kyber#599 — the action-states UI test: the lifecycle "More" menu must offer
// the WORKING recovery (Start → desiredPhase=Running) for a crashed agent and
// no longer the no-op "Restart pod" (Restarting only transitions from Running).
// This was the live Boba Fett OOM pain: a MemoryExhausted agent sat dead 46h
// because the menu's only shown recovery (Restart) silently did nothing and the
// working Start was hidden. Rendered in isolation via the extracted
// LifecycleMenuItems (mirrors the StatusCardBody/MismatchBadges convention) —
// no full-page mount, the menu is opened declaratively (`open`) so its items
// are in the DOM without driving a pointer.

// Radix DropdownMenuContent (Popper-positioned, focus-scoped) leans on a few
// browser APIs jsdom doesn't implement; the other portal/menu tests stub the
// same ones.
if (typeof Element !== 'undefined') {
  if (typeof Element.prototype.scrollIntoView !== 'function')
    Element.prototype.scrollIntoView = function () {}
  if (typeof Element.prototype.hasPointerCapture !== 'function')
    Element.prototype.hasPointerCapture = function () {
      return false
    }
  if (typeof Element.prototype.releasePointerCapture !== 'function')
    Element.prototype.releasePointerCapture = function () {}
}
if (typeof globalThis.ResizeObserver === 'undefined') {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
}

function renderLifecycleMenu(phase: AgentPhase) {
  const onSelect = vi.fn()
  render(
    <DropdownMenu open>
      <DropdownMenuContent>
        <LifecycleMenuItems phase={phase} onSelect={onSelect} />
      </DropdownMenuContent>
    </DropdownMenu>,
  )
  return onSelect
}

describe('AgentDetail LifecycleMenuItems (kyber#599)', () => {
  it.each(['MemoryExhausted', 'Failed'] as const)(
    '%s: offers a working Start (not the no-op Restart pod), keeps Require re-auth',
    (phase) => {
      renderLifecycleMenu(phase)
      // The working recovery is surfaced…
      expect(screen.getByRole('menuitem', { name: /^Start$/ })).toBeInTheDocument()
      // …and the auth-wedge recovery (#395) stays…
      expect(screen.getByRole('menuitem', { name: /Require re-auth/ })).toBeInTheDocument()
      // …while the no-op Restart pod is gone, and Stop never applied.
      expect(screen.queryByRole('menuitem', { name: /Restart pod/ })).not.toBeInTheDocument()
      expect(screen.queryByRole('menuitem', { name: /^Stop$/ })).not.toBeInTheDocument()
    },
  )

  it('MemoryExhausted: clicking Start fires the start action (desiredPhase=Running, no CLI)', async () => {
    const user = userEvent.setup()
    const onSelect = renderLifecycleMenu('MemoryExhausted')
    await user.click(screen.getByRole('menuitem', { name: /^Start$/ }))
    expect(onSelect).toHaveBeenCalledWith('start')
    expect(onSelect).not.toHaveBeenCalledWith('restart')
  })

  it('non-regression — Running still offers Stop/Restart pod and no Start', () => {
    renderLifecycleMenu('Running')
    expect(screen.getByRole('menuitem', { name: /^Stop$/ })).toBeInTheDocument()
    expect(screen.getByRole('menuitem', { name: /Restart pod/ })).toBeInTheDocument()
    expect(screen.queryByRole('menuitem', { name: /^Start$/ })).not.toBeInTheDocument()
  })

  it('non-regression — Stopped still offers Start + Restart pod', () => {
    renderLifecycleMenu('Stopped')
    expect(screen.getByRole('menuitem', { name: /^Start$/ })).toBeInTheDocument()
    expect(screen.getByRole('menuitem', { name: /Restart pod/ })).toBeInTheDocument()
  })
})

// kyber#26 — NeedsAuth used to render an empty More menu, so an agent parked
// on a credential that was already valid had no PWA control at all and needed
// a cluster-admin `kubectl annotate` to unwedge it.
describe('AgentDetail LifecycleMenuItems — NeedsAuth (kyber#26)', () => {
  it('offers Restart pod, so the More menu is no longer empty', () => {
    renderLifecycleMenu('NeedsAuth')
    expect(screen.getByRole('menuitem', { name: /Restart pod/ })).toBeInTheDocument()
  })

  it('the Restart pod item fires retry-startup — the /start path, not the dead /restart one', async () => {
    const user = userEvent.setup()
    const onSelect = renderLifecycleMenu('NeedsAuth')
    await user.click(screen.getByRole('menuitem', { name: /Restart pod/ }))
    // 'retry-startup' is dispatched to the start mutation. Firing 'restart'
    // here would set desiredPhase=Restarting, which matches no transition out
    // of NeedsAuth — a visibly-clickable no-op, the #599 defect.
    expect(onSelect).toHaveBeenCalledWith('retry-startup')
    expect(onSelect).not.toHaveBeenCalledWith('restart')
  })

  it('does not offer Require re-auth or Stop — the agent is already parked', () => {
    renderLifecycleMenu('NeedsAuth')
    expect(screen.queryByRole('menuitem', { name: /Require re-auth/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('menuitem', { name: /^Stop$/ })).not.toBeInTheDocument()
  })
})
