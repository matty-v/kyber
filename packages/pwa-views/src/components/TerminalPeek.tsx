import { useMemo, useState, type ReactNode } from 'react'
import type { Agent } from '../lib/types'
import { Card } from './Card'
import { EmptyState } from './EmptyState'
import { ExecTerminal } from './ExecTerminal'
import { rankByActivity } from '../lib/dashboard'
import { usePageVisible } from '../hooks/usePageVisible'
import { Select, SelectTrigger, SelectValue, SelectContent, SelectItem } from './ui/select'

function PeekFrame({
  name,
  selector,
  heightClassName,
}: {
  name: string
  selector?: ReactNode
  heightClassName: string
}) {
  const visible = usePageVisible()

  return (
    <Card>
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <span className="font-mono text-[10px] uppercase tracking-[0.2em] text-text-muted">
          Terminal peek
        </span>
        {selector}
        <span className="inline-flex items-center gap-1 rounded-md border border-success/30 bg-success-muted px-2 py-0.5 text-[10px] text-success">
          <span className="h-1.5 w-1.5 rounded-full bg-success" aria-hidden /> LIVE · read-only
        </span>
      </div>
      {visible ? (
        <ExecTerminal key={name} kind="agent" name={name} mode="attach" heightClassName={heightClassName} />
      ) : (
        <div className={`flex ${heightClassName} items-center justify-center rounded-lg border border-border-subtle bg-surface-sunken text-xs text-text-muted`}>
          Paused — return to this tab to resume the live view.
        </div>
      )}
    </Card>
  )
}

export function TerminalPeek({ agents }: { agents: Agent[] }) {
  const defaultName = useMemo(
    () => rankByActivity(agents, 1)[0]?.id ?? agents[0]?.id ?? '',
    [agents],
  )
  const [selected, setSelected] = useState('')
  const name = selected || defaultName

  if (agents.length === 0) {
    return (
      <Card>
        <EmptyState title="No agents to watch" />
      </Card>
    )
  }

  const selector = (
    <Select value={name} onValueChange={setSelected}>
          <SelectTrigger className="h-7 w-44 text-xs">
            <SelectValue placeholder="Select agent" />
          </SelectTrigger>
          <SelectContent>
            {agents.map((agent) => (
              <SelectItem key={agent.id} value={agent.id}>{agent.id}</SelectItem>
            ))}
          </SelectContent>
    </Select>
  )

  return <PeekFrame name={name} selector={selector} heightClassName="h-64" />
}

export function AgentTerminalPeek({ agentName }: { agentName: string }) {
  if (!agentName) {
    return (
      <Card>
        <EmptyState title="Terminal unavailable" />
      </Card>
    )
  }

  return <PeekFrame name={agentName} heightClassName="h-80" />
}
