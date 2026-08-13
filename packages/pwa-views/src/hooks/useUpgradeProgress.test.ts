import { describe, it, expect, vi, beforeEach } from 'vitest'
import { renderHook } from '@testing-library/react'

const toastInfo = vi.fn()
const toastSuccess = vi.fn()
const toastError = vi.fn()

vi.mock('sonner', () => ({
  toast: {
    info: (...args: unknown[]) => toastInfo(...args),
    success: (...args: unknown[]) => toastSuccess(...args),
    error: (...args: unknown[]) => toastError(...args),
  },
}))

vi.mock('./useAPI', () => ({
  useUpdates: vi.fn(),
}))

import * as useAPIModule from './useAPI'
import { useUpgradeProgress } from './useUpgradeProgress'
import type { UpdateRun, UpdateStatus } from '../lib/types'

function mockRun(run: UpdateRun | undefined, isError = false) {
  vi.mocked(useAPIModule.useUpdates).mockReturnValue({
    data: run ? ({ lastRun: run } as UpdateStatus) : undefined,
    isError,
  } as unknown as ReturnType<typeof useAPIModule.useUpdates>)
}

const running: UpdateRun = { jobName: 'job-1', targetVersion: '1.0.4', phase: 'running' }

describe('useUpgradeProgress', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    sessionStorage.clear()
  })

  it('reports in-flight while pending or running', () => {
    mockRun(running)
    const { result } = renderHook(() => useUpgradeProgress())
    expect(result.current.inFlight).toBe(true)
    expect(result.current.targetVersion).toBe('1.0.4')
  })

  it('is not in-flight once the run has finished', () => {
    mockRun({ ...running, phase: 'succeeded' })
    const { result } = renderHook(() => useUpgradeProgress())
    expect(result.current.inFlight).toBe(false)
    expect(result.current.targetVersion).toBeNull()
  })

  it('treats an unreachable control plane mid-run as reconnecting, not failure', () => {
    mockRun(running, true)
    const { result } = renderHook(() => useUpgradeProgress())
    expect(result.current.reconnecting).toBe(true)
  })

  it('does not claim to be reconnecting when no upgrade is running', () => {
    mockRun({ ...running, phase: 'succeeded' }, true)
    const { result } = renderHook(() => useUpgradeProgress())
    expect(result.current.reconnecting).toBe(false)
  })

  it('announces each phase exactly once, even across a remount', () => {
    mockRun(running)
    const first = renderHook(() => useUpgradeProgress())
    expect(toastInfo).toHaveBeenCalledTimes(1)

    // Re-render with the same phase: no second toast.
    first.rerender()
    expect(toastInfo).toHaveBeenCalledTimes(1)

    // A fresh mount stands in for the reload the upgrade itself causes. The
    // dedupe lives in sessionStorage precisely so this does not re-announce.
    first.unmount()
    renderHook(() => useUpgradeProgress())
    expect(toastInfo).toHaveBeenCalledTimes(1)
  })

  it('announces success after the reload that lost all in-memory state', () => {
    // The run started in a previous page life — nothing in memory knows about
    // it — and completed while this tab was reconnecting. This is the single
    // most important notification, and an in-memory previous-phase ref would
    // drop it entirely.
    mockRun({ ...running, phase: 'succeeded' })
    renderHook(() => useUpgradeProgress())
    expect(toastSuccess).toHaveBeenCalledTimes(1)
    expect(toastSuccess.mock.calls[0][0]).toContain('1.0.4')
  })

  it('makes a failure notification permanent', () => {
    mockRun({ ...running, phase: 'failed', message: 'helm upgrade timed out' })
    renderHook(() => useUpgradeProgress())
    expect(toastError).toHaveBeenCalledTimes(1)
    const opts = toastError.mock.calls[0][1] as { duration: number; description: string }
    // A failed upgrade that quietly expired from the corner of the screen is
    // indistinguishable from one that never happened.
    expect(opts.duration).toBe(Infinity)
    expect(opts.description).toContain('helm upgrade timed out')
  })

  it('stays silent on pending — the mutation already acknowledged the click', () => {
    mockRun({ ...running, phase: 'pending' })
    renderHook(() => useUpgradeProgress())
    expect(toastInfo).not.toHaveBeenCalled()
    expect(toastSuccess).not.toHaveBeenCalled()
    expect(toastError).not.toHaveBeenCalled()
  })

  it('announces a second run even though the first is already recorded', () => {
    mockRun(running)
    const { unmount } = renderHook(() => useUpgradeProgress())
    unmount()
    mockRun({ jobName: 'job-2', targetVersion: '1.0.5', phase: 'running' })
    renderHook(() => useUpgradeProgress())
    expect(toastInfo).toHaveBeenCalledTimes(2)
  })
})
