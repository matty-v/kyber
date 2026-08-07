import { useState } from 'react'
import { ChevronRight, Bot } from 'lucide-react'
import type { Turn } from '../lib/transcript'
import { ConversationTurn } from './ConversationTurn'

// A subagent (behind-the-scenes) invocation, rendered as a collapsible block —
// same expand/collapse affordance as a tool call, but clearly marked as a
// subagent (violet accent + badge) and containing its own nested turns.
export function SubagentBlock({ name, turns }: { name: string; turns: Turn[] }) {
  const [open, setOpen] = useState(false)
  const toolCount = turns.filter((t) => t.kind === 'tool').length

  return (
    <div className="rounded-lg border border-[#a78bfa]/40 border-l-2 border-l-[#a78bfa] bg-[#a78bfa]/[0.06] text-sm">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        className="flex w-full items-center gap-2 p-2.5 text-left"
      >
        <ChevronRight
          className={`h-3.5 w-3.5 shrink-0 text-[#a78bfa] transition-transform ${open ? 'rotate-90' : ''}`}
        />
        <Bot className="h-3.5 w-3.5 shrink-0 text-[#a78bfa]" />
        <span className="shrink-0 rounded-full border border-[#a78bfa]/50 bg-[#a78bfa]/10 px-1.5 py-px text-[10px] font-bold uppercase tracking-wide text-[#a78bfa]">
          Subagent
        </span>
        <span className="truncate font-medium text-text-primary">{name}</span>
        <span className="ml-auto shrink-0 text-xs text-text-muted">
          {turns.length} {turns.length === 1 ? 'step' : 'steps'}
          {toolCount > 0 && ` · ${toolCount} ${toolCount === 1 ? 'tool' : 'tools'}`}
        </span>
      </button>

      {open && (
        <div className="space-y-2 border-t border-[#a78bfa]/20 p-2.5">
          {turns.map((t, i) => (
            <ConversationTurn key={i} turn={t} />
          ))}
        </div>
      )}
    </div>
  )
}
