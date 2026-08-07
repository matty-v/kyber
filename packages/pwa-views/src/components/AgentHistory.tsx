import { useState } from 'react'
import { RotateCcw, Download, History } from 'lucide-react'
import { useAgentTranscript } from '../hooks/useAPI'
import { transcriptToText } from '../lib/transcript'
import { RecentConversation } from './RecentConversation'
import { SessionSection } from './SessionSection'
import { Button } from './Button'

interface Props {
  agentName: string
}

// How many of the latest turns the "Recent conversation" section surfaces.
const RECENT_TURNS = 5

// The windows "Load earlier" steps through, narrowest first (kyber#669). The
// panel opens on the narrowest: a busy agent's 7-day transcript was 84.7 MB and
// simply never rendered, and in practice the question being asked here is "what
// is this agent doing right now", not "what did it do last Tuesday". Widening
// stays one click away for when it genuinely is the latter.
const WINDOW_STEPS = [
  { days: 1, label: 'last 24 hours' },
  { days: 3, label: 'last 3 days' },
  { days: 7, label: 'last 7 days' },
] as const

// Widens the fetched window one step. Renders nothing at the widest step, so the
// panel can never be talked into re-fetching beyond the last WINDOW_STEPS entry.
function LoadEarlier({
  next,
  busy,
  onClick,
}: {
  next?: (typeof WINDOW_STEPS)[number]
  busy: boolean
  onClick: () => void
}) {
  if (!next) return null
  return (
    <div className="flex justify-center pt-1">
      <Button
        variant="ghost"
        size="sm"
        onClick={onClick}
        disabled={busy}
        title={`Widen the window to the ${next.label}`}
      >
        <History className="h-3.5 w-3.5" />
        Load earlier ({next.label})
      </Button>
    </div>
  )
}

// The agent's structured, multi-session conversation history, with tool-call and
// subagent detail inline. A "Recent conversation" section pins the latest
// exchange up top; the full sessions sit below, collapsed. Refresh re-fetches;
// Export downloads the loaded history as a .txt file.
export function AgentHistory({ agentName }: Props) {
  const [stepIndex, setStepIndex] = useState(0)
  const step = WINDOW_STEPS[stepIndex]
  const nextStep = WINDOW_STEPS[stepIndex + 1]
  const { data, isLoading, error, refetch, isFetching } = useAgentTranscript(agentName, step.days)
  const sessions = data?.sessions ?? []
  // The tail of the most recent session — the latest back-and-forth.
  const recentTurns = sessions[0]?.turns.slice(-RECENT_TURNS) ?? []

  function exportTxt() {
    const blob = new Blob([transcriptToText(sessions)], { type: 'text/plain;charset=utf-8' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `${agentName}-activity.txt`
    document.body.appendChild(a)
    a.click()
    a.remove()
    setTimeout(() => URL.revokeObjectURL(url), 4000)
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <span className="text-xs text-text-muted">
          {sessions.length > 0
            ? `${sessions.length} ${sessions.length === 1 ? 'session' : 'sessions'} · ${step.label}`
            : ''}
        </span>
        <div className="ml-auto flex items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => void refetch()}
            disabled={isFetching}
            title="Re-fetch the latest history"
          >
            <RotateCcw className={`h-3.5 w-3.5 ${isFetching ? 'animate-spin' : ''}`} />
            Refresh
          </Button>
          <Button
            variant="ghost"
            size="sm"
            onClick={exportTxt}
            disabled={sessions.length === 0}
            title="Download the full history as a .txt file"
          >
            <Download className="h-3.5 w-3.5" />
            Export .txt
          </Button>
        </div>
      </div>

      {isLoading ? (
        <div className="p-4 text-sm text-text-muted">Loading history…</div>
      ) : error ? (
        <div className="p-4 text-sm text-danger">Failed to load history. Try Refresh.</div>
      ) : sessions.length === 0 ? (
        <div className="space-y-3">
          <div className="p-4 text-sm text-text-muted">
            No conversation history in the {step.label}.
          </div>
          <LoadEarlier next={nextStep} busy={isFetching} onClick={() => setStepIndex((i) => i + 1)} />
        </div>
      ) : (
        <div className="space-y-3">
          {data?.truncated && (
            <div className="rounded-md border border-border-subtle bg-surface-sunken p-2 text-xs text-text-muted">
              This agent produced more history than one response can carry, so the {step.label} is
              shown from the most recent activity backwards — the earliest part of the window is
              missing. Export .txt covers what is loaded here.
            </div>
          )}
          <RecentConversation turns={recentTurns} />
          {/* Sessions are newest-first (parseTranscript sorts by startedAt desc); all collapsed. */}
          {sessions.map((s) => (
            <SessionSection key={s.id} session={s} />
          ))}
          <LoadEarlier next={nextStep} busy={isFetching} onClick={() => setStepIndex((i) => i + 1)} />
        </div>
      )}
    </div>
  )
}
