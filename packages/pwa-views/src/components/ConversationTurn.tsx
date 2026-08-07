import type { Turn } from '../lib/transcript'
import { ChannelChip } from './ChannelChip'
import { ThinkingBlock } from './ThinkingBlock'
import { ToolCall } from './ToolCall'
import { SubagentBlock } from './SubagentBlock'

// Local time-of-day for a turn's timestamp (the session header carries the date).
function turnTime(ts: string): string {
  if (!ts) return ''
  const d = new Date(ts)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })
}

function Timestamp({ ts }: { ts: string }) {
  const t = turnTime(ts)
  if (!t) return null
  return <span className="ml-auto shrink-0 font-mono text-[11px] tabular-nums text-text-muted">{t}</span>
}

export function ConversationTurn({ turn }: { turn: Turn }) {
  switch (turn.kind) {
    case 'user':
      return (
        <div className="rounded-lg border border-border-subtle p-3">
          <div className="mb-1 flex items-center gap-2 text-xs font-medium text-text-muted">
            <span>You</span>
            {turn.channel && <ChannelChip channel={turn.channel} />}
            <Timestamp ts={turn.ts} />
          </div>
          <div className="whitespace-pre-wrap text-sm text-text-primary">{turn.text}</div>
        </div>
      )
    case 'assistant':
      return (
        <div className="rounded-lg bg-surface-sunken p-3">
          <div className="mb-1 flex items-center gap-2 text-xs font-medium text-text-muted">
            <span>Assistant</span>
            <Timestamp ts={turn.ts} />
          </div>
          <div className="whitespace-pre-wrap text-sm text-text-primary">{turn.text}</div>
        </div>
      )
    case 'thinking':
      return <ThinkingBlock text={turn.text} />
    case 'tool':
      return <ToolCall name={turn.name} input={turn.input} result={turn.result} isError={turn.isError} />
    case 'subagent':
      return <SubagentBlock name={turn.name} turns={turn.turns} />
    default: {
      const _exhaustive: never = turn
      return _exhaustive
    }
  }
}
