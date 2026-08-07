import { useState } from 'react'
import { ChevronRight } from 'lucide-react'
import type { Turn } from '../lib/transcript'
import { ConversationTurn } from './ConversationTurn'

// The most recent handful of turns, surfaced above the full session list so the
// latest exchange is visible without expanding a session. Collapsible, but open
// by default (the one section that starts expanded).
export function RecentConversation({ turns }: { turns: Turn[] }) {
  const [open, setOpen] = useState(true)
  if (turns.length === 0) return null

  return (
    <section className="rounded-xl border border-border-subtle">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        className="flex w-full items-center gap-2 p-3 text-left"
      >
        <span className="text-[11px] font-semibold uppercase tracking-wider text-accent">
          Recent conversation
        </span>
        <ChevronRight
          className={`ml-auto h-4 w-4 shrink-0 text-text-muted transition-transform ${open ? 'rotate-90' : ''}`}
        />
      </button>

      {open && (
        <div className="space-y-2 border-t border-border-subtle p-3 pt-2">
          {turns.map((t, i) => (
            <ConversationTurn key={i} turn={t} />
          ))}
        </div>
      )}
    </section>
  )
}
