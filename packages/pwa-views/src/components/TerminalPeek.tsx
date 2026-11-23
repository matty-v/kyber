import { useMemo, useState, type ReactNode } from 'react'
import type { Agent, AgentGoalStatus } from '../lib/types'
import { Card } from './Card'
import { EmptyState } from './EmptyState'
import { ExecTerminal } from './ExecTerminal'
import { activityEligible, activityGroups } from '../lib/activity'
import { useActivityClock } from '../hooks/useActivityClock'
import { usePageVisible } from '../hooks/usePageVisible'
import { Select, SelectTrigger, SelectValue, SelectContent, SelectItem } from './ui/select'
import { AgentGoalLine } from './AgentGoalLine'
import { AgentAvatar } from './AgentAvatar'

function PeekFrame({
  name,
  selector,
  heightClassName,
  goal,
}: {
  name: string
  selector?: ReactNode
  heightClassName: string
  goal?: AgentGoalStatus
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
      <AgentGoalLine goal={goal} className="mb-2" />
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
  const now = useActivityClock()
  const defaultName = useMemo(() => {
    const eligible = agents.filter(a => activityEligible(a) && a.status?.podName)
    const groups = activityGroups(eligible, now)
    return groups.working[0]?.id ?? groups.background[0]?.id ?? eligible[0]?.id ?? ''
  }, [agents, now])
  const [selected, setSelected] = useState('')
  const name = selected || defaultName
  const goal = agents.find((agent) => agent.id === name)?.goal

  if (!name) {
    return (
      <Card>
        <EmptyState title="No running agents to preview" />
      </Card>
    )
  }

  const selector = (
    <Select value={name} onValueChange={setSelected}>
          {/* The trigger mirrors the selected item, avatar included. */}
          <SelectTrigger className="h-7 w-44 text-xs">
            <SelectValue placeholder="Select agent" />
          </SelectTrigger>
          <SelectContent>
            {agents.map((agent) => (
              <SelectItem key={agent.id} value={agent.id}>
                <span className="flex items-center gap-2">
                  <AgentAvatar name={agent.id} alias={agent.profile?.alias} avatarUrl={agent.profile?.avatarUrl} size="xs" />
                  {agent.id}
                </span>
              </SelectItem>
            ))}
          </SelectContent>
    </Select>
  )

  const target = agents.find(agent => agent.id === name)
  if (!target || !activityEligible(target) || !target.status?.podName) return <Card>{selector}<EmptyState title="Terminal unavailable" description="The selected agent has no running pod. Choose a running agent to preview." /></Card>
  return <PeekFrame name={name} selector={selector} heightClassName="h-64" goal={goal} />
}

export function AgentTerminalPeek({ agentName, hasPod, goal }: { agentName: string; hasPod: boolean; goal?: AgentGoalStatus }) {
  if (!agentName || !hasPod) {
    return (
      <Card>
        <AgentGoalLine goal={goal} className="mb-2" />
        <EmptyState title="Terminal unavailable" description="The live terminal appears while this agent has a running pod." />
      </Card>
    )
  }

  return <PeekFrame name={agentName} heightClassName="h-56 sm:h-80" goal={goal} />
}
