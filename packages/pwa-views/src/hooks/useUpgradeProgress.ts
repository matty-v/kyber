import { useEffect } from 'react'
import { toast } from 'sonner'
import { useUpdates } from './useAPI'
import type { UpdateRun } from '../lib/types'

/**
 * useUpgradeProgress watches the in-flight upgrade and turns phase changes into
 * operator-facing notifications.
 *
 * The whole design of this hook is shaped by one fact: **the control plane is
 * the process being replaced, and it is also what serves this page.** So during
 * an upgrade the API goes away for roughly a minute, and the operator may well
 * reload the tab (the header offers a refresh once the cluster has moved on)
 * before the run finishes. Nothing here reloads the page automatically.
 *
 * Two consequences:
 *
 *   - **Phase is read from the polled Job, never from local state.** `useUpdates`
 *     keeps polling across the outage and react-query retains the last data on
 *     error, so `lastRun.phase` stays truthful through the blackout and the
 *     terminal phase is observed on reconnect. A `useRef` of the previous phase
 *     would be wiped by the reload and the "upgrade finished" notification —
 *     the single most important one — would be silently lost.
 *
 *   - **Dedupe is persisted, not in-memory.** We record the (job, phase) pairs
 *     already announced in sessionStorage, so each transition fires exactly once
 *     even across a reload, and re-mounting the hook does not re-announce a run
 *     the operator has already seen.
 *
 *   - **Terminal phases are announced only while they are news.** `lastRun` is
 *     not "a run that just happened" — it is the newest upgrade Job in the
 *     namespace, and those are kept for seven days (`upgradeJobTTL`). Since
 *     sessionStorage is per-tab-session, without a recency bound every new tab
 *     and every cold start of the PWA would re-announce a week-old outcome —
 *     including a `duration: Infinity` failure toast for an upgrade that was
 *     resolved days ago.
 *
 * sessionStorage rather than localStorage: an upgrade is a per-tab, per-session
 * concern, and a stale key surviving into next week would suppress a real
 * notification.
 */

/**
 * How recently a run must have finished for its outcome to still be worth
 * announcing. Comfortably longer than an upgrade's own control-plane blackout,
 * so a tab that reloads mid-run still reports the result on the way back; far
 * shorter than the seven days the Job itself is retained.
 */
const TERMINAL_RECENCY_MS = 10 * 60 * 1000

/** Key under which announced "<jobName>:<phase>" pairs are recorded. */
const ANNOUNCED_KEY = 'kyber.upgrade.announced'

/** How many announcements to remember. Comfortably more than one run's worth. */
const ANNOUNCED_LIMIT = 24

function readAnnounced(): string[] {
  try {
    const raw = sessionStorage.getItem(ANNOUNCED_KEY)
    if (!raw) return []
    const parsed: unknown = JSON.parse(raw)
    return Array.isArray(parsed) ? parsed.filter((v): v is string => typeof v === 'string') : []
  } catch {
    // Private-mode denial or corrupt JSON. Degrading to "announce again" is
    // strictly better than throwing inside an effect.
    return []
  }
}

function markAnnounced(key: string): void {
  try {
    const next = [...readAnnounced().filter((k) => k !== key), key].slice(-ANNOUNCED_LIMIT)
    sessionStorage.setItem(ANNOUNCED_KEY, JSON.stringify(next))
  } catch {
    // Ignore: losing the dedupe record costs a duplicate toast, nothing more.
  }
}

function hasAnnounced(key: string): boolean {
  return readAnnounced().includes(key)
}

export interface UpgradeProgress {
  /** The run being tracked, or null when no upgrade has ever run. */
  run: UpdateRun | null
  /** True while a run is pending or running — the "don't touch anything" window. */
  inFlight: boolean
  /**
   * True when a run is in flight AND the control plane is currently unreachable.
   * This is the expected mid-upgrade blackout, not a fault, and the UI must say
   * so rather than rendering a generic connection error.
   */
  reconnecting: boolean
  /** The version this run is installing, when one is in flight. */
  targetVersion: string | null
}

export function useUpgradeProgress(): UpgradeProgress {
  const { data, isError } = useUpdates()
  const run = data?.lastRun ?? null
  const phase = run?.phase ?? null
  const inFlight = phase === 'pending' || phase === 'running'

  useEffect(() => {
    if (!run || !phase) return
    const key = `${run.jobName}:${phase}`
    if (hasAnnounced(key)) return

    const version = run.targetVersion || 'the new version'

    // A finished run stays on the API for a week. Only announce an outcome
    // that is still news — otherwise every new tab for the next seven days
    // reports Monday's failure as though it just happened.
    if (phase === 'succeeded' || phase === 'failed') {
      const finished = run.finishedAt ? Date.parse(run.finishedAt) : NaN
      // No/unparseable timestamp: announce. A missing field should not silence
      // a real failure, and the sessionStorage dedupe still caps it at once.
      if (Number.isFinite(finished) && Date.now() - finished > TERMINAL_RECENCY_MS) {
        markAnnounced(key)
        return
      }
    }

    switch (phase) {
      case 'running':
        toast.info(`Installing ${version}`, {
          description:
            'The control plane restarts first — this page will go offline briefly. Agents restart after it.',
          duration: 8000,
        })
        break

      case 'succeeded':
        toast.success(`${version} is live`, {
          description: 'Agents have been restarted onto the new version.',
          duration: 10000,
        })
        break

      case 'failed':
        // Deliberately never auto-dismisses. A failed upgrade that expired from
        // the corner of the screen while nobody was looking is indistinguishable
        // from one that never happened.
        toast.error(`Upgrade to ${version} failed`, {
          description:
            run.message ||
            'Kyber rolled the cluster back to the previous version. Check the upgrade job log.',
          duration: Infinity,
        })
        break

      // 'pending' is not announced: the mutation that started the run already
      // toasted, and a second notification for the same click is noise.
      default:
        return
    }

    markAnnounced(key)
  }, [run, phase])

  return {
    run,
    inFlight,
    reconnecting: inFlight && isError,
    targetVersion: inFlight ? (run?.targetVersion ?? null) : null,
  }
}
