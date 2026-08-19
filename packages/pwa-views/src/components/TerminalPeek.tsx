import { useMemo, useState } from 'react'
import type { Agent } from '../lib/types'
import { Card } from './Card'
import { EmptyState } from './EmptyState'
import { ExecTerminal } from './ExecTerminal'
import { rankByActivity } from '../lib/dashboard'
import { usePageVisible } from '../hooks/usePageVisible'
import { Select, SelectTrigger, SelectValue, SelectContent, SelectItem } from './ui/select'

export function TerminalPeek({ agents }: { agents: Agent[] }) {
  const defaultName = useMemo(
    () => rankByActivity(agents, 1)[0]?.id ?? agents[0]?.id ?? '',
    [agents],
  )
  const [selected, setSelected] = useState('')
  const name = selected || defaultName
  const visible = usePageVisible()

  if (agents.length === 0) {
    return (
      <Card>
        <EmptyState title="No agents to watch" />
      </Card>
    )
  }

  return (
    <Card>
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <span className="font-mono text-[10px] uppercase tracking-[0.2em] text-text-muted">
          Terminal peek
        </span>
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
        <span className="inline-flex items-center gap-1 rounded-md border border-success/30 bg-success-muted px-2 py-0.5 text-[10px] text-success">
          <span className="h-1.5 w-1.5 rounded-full bg-success" aria-hidden /> LIVE · read-only
        </span>
      </div>
      {visible ? (
        <ExecTerminal key={name} kind="agent" name={name} mode="attach" heightClassName="h-64" />
      ) : (
        <div className="flex h-64 items-center justify-center rounded-lg border border-border-subtle bg-surface-sunken text-xs text-text-muted">
          Paused — return to this tab to resume the live view.
        </div>
      )}
    </Card>
  )
}
