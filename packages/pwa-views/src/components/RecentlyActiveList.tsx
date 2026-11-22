import { useContext, useId, useLayoutEffect, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { ChevronDown, ChevronRight } from 'lucide-react'
import type { Agent } from '../lib/types'
import { Card } from './Card'
import { AgentGoalLine } from './AgentGoalLine'
import { AgentAvatar } from './AgentAvatar'
import { usePrefixedPath } from '../lib/route-prefix'
import { ClusterContext } from '../lib/cluster-context'
import { ACTIVITY_GROUPS, activityServerNow, activityGroups, projectWork, workReasons, type ActivityGroup } from '../lib/activity'
import { useActivityClock } from '../hooks/useActivityClock'
import { formatAgo, DEFAULT_LIST_LIMIT } from '../lib/dashboard'

const headings: Record<ActivityGroup, string> = { working: 'Working now', background: 'Background work', waiting: 'Waiting', unavailable: 'Activity unavailable' }
const colors: Record<ActivityGroup, string> = { working: 'text-success', background: 'text-info', waiting: 'text-text-secondary', unavailable: 'text-warn' }
const defaults: Record<ActivityGroup, boolean> = { working: true, background: true, waiting: false, unavailable: true }

function Group({ group, agents, limit, storageKey }: { group: ActivityGroup; agents: Agent[]; limit: number; storageKey: string }) {
  const prefixed = usePrefixedPath()
  const contentId = useId()
  const [open, setOpen] = useState(() => {
    try { const v = JSON.parse(localStorage.getItem(storageKey) ?? '{}')[group]; return typeof v === 'boolean' ? v : defaults[group] } catch { return defaults[group] }
  })
  function toggle() {
    const value = !open
    setOpen(value)
    try {
      const stored: unknown = JSON.parse(localStorage.getItem(storageKey) ?? '{}')
      const saved: Partial<Record<ActivityGroup, boolean>> = {}
      for (const key of ACTIVITY_GROUPS) if (stored && typeof stored === 'object' && typeof (stored as Record<string, unknown>)[key] === 'boolean') saved[key] = (stored as Record<string, boolean>)[key]
      localStorage.setItem(storageKey, JSON.stringify({ ...saved, [group]: value }))
    } catch { /* Private browsing/storage denial must not block toggling. */ }
  }
  return <section className="border-t border-border-subtle px-3 first:border-0">
    <button type="button" data-group-button={group} aria-expanded={open} aria-controls={contentId} onClick={toggle} className={`flex min-h-12 w-full items-center gap-2 rounded text-left text-xs font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring ${colors[group]}`}>
      <span>{headings[group]}</span>{' '}<span className="rounded border border-border-subtle px-1.5 text-text-secondary">{agents.length}</span>
      {group === 'unavailable' && agents.length > 0 && <span className="text-warn" aria-label="Activity signals need attention">△</span>}
      {open ? <ChevronDown className="ml-auto h-4 w-4" /> : <ChevronRight className="ml-auto h-4 w-4" />}
    </button>
    <div id={contentId} hidden={!open}>
      {!agents.length && <p className="pb-3 text-xs text-text-muted">No agents in this group.</p>}
      <ul>
        {agents.slice(0, limit).map(agent => {
          const work = projectWork(agent)
          const primary = work.state === 'background' ? `${work.backgroundCount} ${work.backgroundCount === 1 ? 'shell' : 'shells'} running` : work.state === 'working' ? 'Working' : work.needsInput ? 'Needs your reply' : work.state === 'waiting' ? 'Ready for a request' : workReasons[work.reason ?? ''] ?? 'Activity unavailable'
          const at = work.state === 'unavailable' ? agent.activity?.lastHeartbeatAt : work.state === 'waiting' ? work.lastWorkedAt : work.observedAt
          const timeLabel = work.state === 'unavailable' ? 'Last contact' : work.state === 'waiting' ? 'Last worked' : work.state === 'background' ? 'Job checked' : 'Work observed'
          return <li key={agent.id} className="min-w-0 border-t border-border-subtle first:border-0">
            <Link data-activity-agent={agent.id} data-activity-group={group} to={prefixed(`/agents/${agent.id}`)} className="flex min-w-0 items-start gap-2 rounded py-3 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring">
              <AgentAvatar name={agent.id} alias={agent.profile?.alias} avatarUrl={agent.profile?.avatarUrl} size="sm" />
              <div className="min-w-0 flex-1">
                <span className="block truncate text-sm font-medium text-text-primary">{agent.profile?.alias || agent.id}</span>
                <span className={`block text-xs ${colors[group]}`}>{primary}{work.state === 'working' && work.backgroundCount > 0 ? ` · +${work.backgroundCount} background jobs` : ''}</span>
                {work.needsInput && work.state !== 'waiting' && <span className="block text-xs text-warn">Needs your reply</span>}
                {agent.goal?.summary && <div className="block"><span className="text-[10px] text-text-muted">Latest request</span><AgentGoalLine goal={agent.goal} /></div>}
                <span className="mt-1 block text-[10px] text-text-muted">{at ? `${timeLabel} ${formatAgo(at, activityServerNow(agent))}` : group === 'unavailable' ? 'Last contact not recorded' : 'No work time recorded'}</span>
              </div>
              <ChevronRight className="mt-1 h-3.5 w-3.5 shrink-0 text-text-disabled" />
            </Link>
          </li>
        })}
      </ul>
      {agents.length > limit && <p className="pb-3 text-xs text-text-muted">Showing {limit} of {agents.length}. <Link className="text-accent" to={prefixed('/agents')}>View all agents</Link></p>}
    </div>
  </section>
}

export function RecentlyActiveList({ agents, limit = DEFAULT_LIST_LIMIT }: { agents: Agent[]; limit?: number; now?: number }) {
  const cluster = useContext(ClusterContext)
  const monotonicNow = useActivityClock()
  const prefixed = usePrefixedPath()
  const groups = activityGroups(agents, monotonicNow)
  const clusterKey = cluster?.id ?? 'embedded'
  const storageKey = `kyber:activity-groups:v1:${clusterKey}`
  const root = useRef<HTMLDivElement>(null)
  const focused = useRef<{ id: string; group: ActivityGroup } | null>(null)
  useLayoutEffect(() => { focused.current = null }, [clusterKey])
  useLayoutEffect(() => {
    const previous = focused.current
    if (!previous || document.activeElement !== document.body) return
    const group = ACTIVITY_GROUPS.find(key => groups[key].some(a => a.id === previous.id)) ?? previous.group
    const row = Array.from(root.current?.querySelectorAll<HTMLAnchorElement>('a[data-activity-agent]') ?? []).find(link => link.dataset.activityAgent === previous.id)
    if (row && !row.closest('[hidden]')) row.focus()
    else root.current?.querySelector<HTMLButtonElement>(`[data-group-button="${group}"]`)?.focus()
  }, [groups])
  return <div ref={root} onFocusCapture={event => {
    const row = (event.target as HTMLElement).closest<HTMLAnchorElement>('a[data-activity-agent]')
    focused.current = row ? { id: row.dataset.activityAgent!, group: row.dataset.activityGroup as ActivityGroup } : null
  }} onBlurCapture={event => { if (event.relatedTarget && !event.currentTarget.contains(event.relatedTarget as Node)) focused.current = null }}><Card className="min-w-0 max-w-full">
    <div className="mb-2 flex items-center justify-between gap-2"><h2 className="text-sm font-semibold text-text-primary">Agent activity</h2><Link to={prefixed('/agents')} className="text-xs text-accent">View all agents</Link></div>
    {ACTIVITY_GROUPS.map(group => <Group key={`${clusterKey}:${group}`} group={group} agents={groups[group]} limit={Math.max(1, limit)} storageKey={storageKey} />)}
  </Card></div>
}
