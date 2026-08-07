import { useState } from 'react'
import { ChevronRight } from 'lucide-react'
import type { Session } from '../lib/transcript'
import { ConversationTurn } from './ConversationTurn'

interface Props {
  session: Session
  // Whether the session starts expanded. Defaults to collapsed.
  defaultExpanded?: boolean
}

function sameDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate()
}

// Header label as a start → end range. Same calendar day → the date once with
// both times; spanning days → the date on each end. A session that ran into
// today reads as today even if it started the night before (the reason a
// long-running session used to look like it was "missing today").
function formatRange(startedAt: string, endedAt: string): string {
  const start = startedAt ? new Date(startedAt) : null
  const end = endedAt ? new Date(endedAt) : null
  const vStart = start && !Number.isNaN(start.getTime()) ? start : null
  const vEnd = end && !Number.isNaN(end.getTime()) ? end : null
  if (!vStart) return 'unknown time'
  const date = (d: Date) => d.toLocaleDateString([], { month: 'short', day: 'numeric' })
  const time = (d: Date) => d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })
  if (!vEnd || sameDay(vStart, vEnd)) {
    const startT = time(vStart)
    const endT = vEnd ? time(vEnd) : ''
    // Only append the end time when it renders differently (skip "3:00 → 3:00").
    const tail = endT && endT !== startT ? ` → ${endT}` : ''
    return `${date(vStart)}, ${startT}${tail}`
  }
  return `${date(vStart)}, ${time(vStart)} → ${date(vEnd)}, ${time(vEnd)}`
}

// A collapsible session accordion: a clickable header (time range, an "Active
// today" tag when it ran today, turn count, and a first-message preview when
// collapsed) that toggles the session's turns. Self-contained open state.
export function SessionSection({ session, defaultExpanded = false }: Props) {
  const [open, setOpen] = useState(defaultExpanded)
  const label = formatRange(session.startedAt, session.endedAt)
  const end = session.endedAt ? new Date(session.endedAt) : null
  const activeToday = !!end && !Number.isNaN(end.getTime()) && sameDay(end, new Date())

  return (
    <section className="my-3">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        className="flex w-full items-center gap-2 border-b border-border-subtle pb-1 text-left text-xs font-medium text-text-muted hover:text-text-primary"
      >
        <ChevronRight
          className={`h-3.5 w-3.5 shrink-0 transition-transform ${open ? 'rotate-90' : ''}`}
        />
        <span className="shrink-0 tabular-nums">{label}</span>
        {activeToday && (
          <span className="shrink-0 rounded-full border border-accent-ring px-1.5 py-px text-[10px] font-semibold uppercase tracking-wide text-accent">
            Active today
          </span>
        )}
        <span className="shrink-0 text-text-muted">· {session.turns.length} turns</span>
        {!open && session.firstUserText && (
          <span className="truncate text-text-muted">— {session.firstUserText}</span>
        )}
      </button>

      {open && (
        <div className="mt-2 space-y-2">
          {session.turns.map((t, i) => (
            <ConversationTurn key={`${session.id}-${i}`} turn={t} />
          ))}
        </div>
      )}
    </section>
  )
}
