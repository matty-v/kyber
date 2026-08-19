import { useEffect, useId } from 'react'
import { useNavigate } from 'react-router-dom'
import { Command } from 'cmdk'
import { ArrowLeft, LayoutDashboard, Server, Bot, Settings } from 'lucide-react'
import { useAgents, useMachines } from '../hooks/useAPI'
import { useBackTo, usePrefixedPath } from '../lib/route-prefix'

// ⌘K command palette (#106 Slice B). Read-only V1: fuzzy search over routes,
// agents, and machines. Write actions (Start/Stop/Restart/
// Delete) and the contextual-resource concept were deferred per the issue's
// refinement comment — they need confirm dialogs and a fresh design pass.
//
// Modal frame mirrors the ShortcutHelpOverlay pattern (full-bleed backdrop +
// centered panel) for visual consistency. cmdk's <Command> handles fuzzy
// matching, arrow-key navigation, and Enter-to-select; we just feed it the
// item list. Esc closes via a local listener (matches ShortcutHelpOverlay).

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function CommandPalette({ open, onOpenChange }: Props) {
  const navigate = useNavigate()
  const prefixed = usePrefixedPath()
  const backTo = useBackTo()

  const ROUTE_COMMANDS = [
    ...(backTo
      ? [{ id: 'route:back', label: backTo.label, path: backTo.href, icon: ArrowLeft }]
      : []),
    { id: 'route:fleet', label: 'Fleet', path: prefixed('/'), icon: LayoutDashboard },
    { id: 'route:machines', label: 'Machines', path: prefixed('/machines'), icon: Server },
    { id: 'route:agents', label: 'Agents', path: prefixed('/agents'), icon: Bot },
    { id: 'route:settings', label: 'Settings', path: prefixed('/settings'), icon: Settings },
  ]
  const titleId = useId()
  const { data: agents = [], isLoading: agentsLoading } = useAgents()
  const { data: machines = [], isLoading: machinesLoading } = useMachines()

  // Esc closes — same hook shape as ShortcutHelpOverlay so the two modals
  // behave identically. cmdk doesn't intercept Esc on its own when not using
  // <CommandDialog>, so we own it.
  useEffect(() => {
    if (!open) return
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        e.preventDefault()
        onOpenChange(false)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onOpenChange])

  if (!open) return null

  function handleSelect(action: () => void) {
    return () => {
      action()
      onOpenChange(false)
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center p-4 pt-[15vh]"
      role="presentation"
    >
      <div
        className="absolute inset-0 bg-surface-sunken/60 backdrop-blur-sm"
        onClick={() => onOpenChange(false)}
        aria-hidden="true"
      />
      <Command
        label="Command palette"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className="relative z-10 w-full max-w-xl overflow-hidden rounded-xl border border-border-default bg-surface-overlay shadow-xl"
      >
        <h2 id={titleId} className="sr-only">
          Command palette
        </h2>
        <Command.Input
          autoFocus
          placeholder="Type to jump to a route, agent, or machine…"
          className="w-full border-b border-border-subtle bg-transparent px-4 py-3 text-sm text-text-primary placeholder:text-text-muted focus:outline-none"
        />
        <Command.List className="max-h-[60vh] overflow-y-auto p-2">
          <Command.Empty className="px-3 py-6 text-center text-sm text-text-muted">
            No matches.
          </Command.Empty>

          <Command.Group
            heading="Routes"
            className="kyber-cmd-group [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:pb-1 [&_[cmdk-group-heading]]:pt-2 [&_[cmdk-group-heading]]:text-[11px] [&_[cmdk-group-heading]]:font-semibold [&_[cmdk-group-heading]]:uppercase [&_[cmdk-group-heading]]:tracking-wider [&_[cmdk-group-heading]]:text-text-muted"
          >
            {ROUTE_COMMANDS.map((r) => {
              const Icon = r.icon
              return (
                <Command.Item
                  key={r.id}
                  value={`route ${r.label}`}
                  onSelect={handleSelect(() => navigate(r.path))}
                  className="flex cursor-pointer items-center gap-3 rounded-md px-3 py-2 text-sm text-text-primary aria-selected:bg-accent-muted aria-selected:text-accent"
                >
                  <Icon className="h-4 w-4 text-text-muted" />
                  <span>{r.label}</span>
                  <span className="ml-auto font-mono text-xs text-text-muted">{r.path}</span>
                </Command.Item>
              )
            })}
          </Command.Group>

          <Command.Group
            heading="Agents"
            className="kyber-cmd-group [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:pb-1 [&_[cmdk-group-heading]]:pt-3 [&_[cmdk-group-heading]]:text-[11px] [&_[cmdk-group-heading]]:font-semibold [&_[cmdk-group-heading]]:uppercase [&_[cmdk-group-heading]]:tracking-wider [&_[cmdk-group-heading]]:text-text-muted"
          >
            {agentsLoading ? (
              <Command.Loading className="px-3 py-2 text-xs text-text-muted">
                Loading agents…
              </Command.Loading>
            ) : (
              agents.map((a) => (
                <Command.Item
                  key={`agent:${a.id}`}
                  value={`agent ${a.id}`}
                  onSelect={handleSelect(() => navigate(prefixed(`/agents/${a.id}`)))}
                  className="flex cursor-pointer items-center gap-3 rounded-md px-3 py-2 text-sm text-text-primary aria-selected:bg-accent-muted aria-selected:text-accent"
                >
                  <Bot className="h-4 w-4 text-text-muted" />
                  <span className="font-mono">{a.id}</span>
                  <span className="ml-auto text-xs text-text-muted">{a.phase}</span>
                </Command.Item>
              ))
            )}
          </Command.Group>

          <Command.Group
            heading="Machines"
            className="kyber-cmd-group [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:pb-1 [&_[cmdk-group-heading]]:pt-3 [&_[cmdk-group-heading]]:text-[11px] [&_[cmdk-group-heading]]:font-semibold [&_[cmdk-group-heading]]:uppercase [&_[cmdk-group-heading]]:tracking-wider [&_[cmdk-group-heading]]:text-text-muted"
          >
            {machinesLoading ? (
              <Command.Loading className="px-3 py-2 text-xs text-text-muted">
                Loading machines…
              </Command.Loading>
            ) : (
              machines.map((m) => (
                <Command.Item
                  key={`machine:${m.id}`}
                  value={`machine ${m.id}`}
                  onSelect={handleSelect(() => navigate(prefixed(`/machines/${m.id}`)))}
                  className="flex cursor-pointer items-center gap-3 rounded-md px-3 py-2 text-sm text-text-primary aria-selected:bg-accent-muted aria-selected:text-accent"
                >
                  <Server className="h-4 w-4 text-text-muted" />
                  <span className="font-mono">{m.id}</span>
                  <span className="ml-auto text-xs text-text-muted">{m.phase}</span>
                </Command.Item>
              ))
            )}
          </Command.Group>

        </Command.List>
        <div className="flex items-center justify-between border-t border-border-subtle px-3 py-2 text-[11px] text-text-muted">
          <span>
            <kbd className="rounded border border-border-default bg-surface-raised px-1.5 py-0.5 font-mono">↑↓</kbd>{' '}
            navigate
            <kbd className="ml-3 rounded border border-border-default bg-surface-raised px-1.5 py-0.5 font-mono">↵</kbd>{' '}
            select
          </span>
          <span>
            <kbd className="rounded border border-border-default bg-surface-raised px-1.5 py-0.5 font-mono">Esc</kbd>{' '}
            close
          </span>
        </div>
      </Command>
    </div>
  )
}
