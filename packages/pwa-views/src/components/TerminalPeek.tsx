import { useMemo, useState } from 'react'
import { RefreshCw } from 'lucide-react'
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
  const [view, setView] = useState<'live' | 'history'>('live')
  const [historyNonce, setHistoryNonce] = useState(0)
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
        <div className="flex rounded-md border border-border-subtle p-0.5 text-[10px]">
          <button type="button" onClick={() => setView('live')} className={`rounded px-2 py-1 ${view === 'live' ? 'bg-success-muted text-success' : 'text-text-muted hover:text-text-primary'}`}>
            Live
          </button>
          <button type="button" onClick={() => setView('history')} className={`rounded px-2 py-1 ${view === 'history' ? 'bg-accent-muted text-accent' : 'text-text-muted hover:text-text-primary'}`}>
            History
          </button>
        </div>
        {view === 'live' ? (
          <span className="inline-flex items-center gap-1 text-[10px] text-success">
            <span className="h-1.5 w-1.5 rounded-full bg-success" aria-hidden /> read-only
          </span>
        ) : (
          <button type="button" onClick={() => setHistoryNonce((value) => value + 1)} className="inline-flex items-center gap-1 rounded px-2 py-1 text-[10px] text-text-muted hover:bg-surface-hover hover:text-text-primary">
            <RefreshCw size={11} /> Refresh
          </button>
        )}
      </div>
      {visible ? (
        <ExecTerminal
          key={`${name}-${view}-${historyNonce}`}
          kind="agent"
          name={name}
          mode={view === 'live' ? 'attach' : 'history'}
          heightClassName="h-64"
        />
      ) : (
        <div className="flex h-64 items-center justify-center rounded-lg border border-border-subtle bg-surface-sunken text-xs text-text-muted">
          Paused — return to this tab to resume the live view.
        </div>
      )}
    </Card>
  )
}
