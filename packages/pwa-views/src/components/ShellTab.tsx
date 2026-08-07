// Shell tab for AgentDetail (#125) — pure interactive shell into the agent
// pod, landing as root inside the chroot-visible namespace via nsenter.
// Header carries just a refresh button + help-icon popover (per-alias copy
// buttons), nothing else. The heavy lifting lives in ExecTerminal.

import { useState, useCallback } from 'react'
import { Copy, HelpCircle, RotateCcw } from 'lucide-react'
import { ExecTerminal } from './ExecTerminal'
import { Button } from './Button'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'

interface Alias {
  name: string
  description: string
}

// Mirror of /etc/profile.d/kyber.sh — keep in sync with
// images/agent-base/profile-kyber.sh. Descriptions are trimmed for
// compactness in the popover.
const ALIASES: Alias[] = [
  { name: 'agent', description: 'attach to the Claude Code tmux (read/write)' },
  { name: 'peek', description: 'attach read-only — safer mid-task' },
  { name: 'as-agent', description: 'become the kyber user' },
  { name: 'creds', description: 'show ~/.claude/.credentials.json' },
  { name: 'tg', description: 'show ~/.claude/channels/telegram/access.json' },
  { name: 'boot', description: 'tail the bootstrap log' },
  { name: 'tokens', description: 'tail the token-reporter log' },
  { name: 'cron-log', description: 'tail /persist/var/log/kyber-jobs.log' },
  { name: 'jobs', description: 'show the rendered agent-jobs crontab' },
  { name: 'restart-agent', description: 'kill the tmux session — pod restarts' },
]

interface Props {
  agentName: string
}

export function ShellTab({ agentName }: Props) {
  const [nonce, setNonce] = useState(0)
  const [copied, setCopied] = useState<string | null>(null)

  const refresh = useCallback(() => setNonce((n) => n + 1), [])

  const copyAlias = useCallback(async (name: string) => {
    try {
      await navigator.clipboard.writeText(name)
      setCopied(name)
      // Fade the "copied" state; 1s is enough to register without blocking
      // rapid-fire copies of different aliases.
      setTimeout(() => setCopied((c) => (c === name ? null : c)), 1000)
    } catch {
      // Safari mobile blocks clipboard writes outside a user gesture —
      // the popover click IS a gesture, so this should rarely fail. If it
      // does, silently swallow; the user can still select + copy manually.
    }
  }, [])

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between gap-2">
        <p className="text-xs text-text-muted">
          Root shell in the agent pod&apos;s chroot-visible namespace.
        </p>
        <div className="flex items-center gap-1">
          <Popover>
            <PopoverTrigger asChild>
              <Button variant="ghost" size="sm" title="Debug aliases" aria-label="Show debug aliases">
                <HelpCircle className="h-3.5 w-3.5" />
              </Button>
            </PopoverTrigger>
            <PopoverContent className="w-80">
              <div className="mb-2 text-xs font-medium text-text-secondary">
                Debug aliases
              </div>
              <p className="mb-3 text-xs text-text-muted">
                Tap a row to copy the alias to your clipboard, then paste into the shell.
              </p>
              <ul className="space-y-1">
                {ALIASES.map((a) => (
                  <li key={a.name}>
                    <button
                      type="button"
                      onClick={() => copyAlias(a.name)}
                      className="group flex w-full items-start gap-2 rounded px-2 py-1 text-left hover:bg-surface-overlay focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring"
                    >
                      <Copy
                        className={`mt-0.5 h-3 w-3 shrink-0 ${
                          copied === a.name ? 'text-success' : 'text-text-muted'
                        }`}
                      />
                      <span className="min-w-0 flex-1">
                        <code className="font-mono text-xs text-text-primary">{a.name}</code>
                        <span className="ml-2 text-xs text-text-muted">{a.description}</span>
                      </span>
                    </button>
                  </li>
                ))}
              </ul>
            </PopoverContent>
          </Popover>
          <Button
            variant="ghost"
            size="sm"
            onClick={refresh}
            title="Refresh — tear down the current shell and open a fresh one"
          >
            <RotateCcw className="h-3.5 w-3.5" />
            Refresh
          </Button>
        </div>
      </div>
      <ExecTerminal key={`shell-${nonce}`} kind="agent" name={agentName} mode="shell" />
    </div>
  )
}
