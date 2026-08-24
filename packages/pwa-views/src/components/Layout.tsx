import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { Link, NavLink } from 'react-router-dom'
import {
  ArrowLeft,
  LayoutDashboard,
  Server,
  Bot,
  BarChart3,
  ScrollText,
  Settings,
  Gem,
} from 'lucide-react'
import { useKeyboardShortcuts } from '../hooks/useKeyboardShortcuts'
import { useCommandPaletteShortcut } from '../hooks/useCommandPaletteShortcut'
import { ShortcutHelpOverlay } from './ShortcutHelpOverlay'
import { CommandPalette } from './CommandPalette'
import { useBackTo, usePrefixedPath } from '../lib/route-prefix'
import { ClusterIdentifier } from './ClusterIdentifier'
import { UpgradeBanner } from './UpgradeBanner'
import { useCluster } from '../lib/cluster-context'

interface Props {
  children: ReactNode
}

function NavItem({
  to,
  label,
  icon: Icon,
  exact,
  horizontal,
}: {
  to: string
  label: string
  icon: typeof LayoutDashboard
  exact: boolean
  horizontal?: boolean
}) {
  return (
    <NavLink
      to={to}
      end={exact}
      className={({ isActive }) =>
        `kyber-nav-item flex items-center gap-2 rounded-lg px-3 py-2 text-sm font-medium transition-colors min-h-[44px] ${
          isActive
            ? 'bg-accent-muted text-accent ring-1 ring-inset ring-accent-ring'
            : 'text-text-muted hover:text-text-primary hover:bg-surface-overlay'
        } ${horizontal ? 'flex-1 flex-col justify-center gap-1 text-xs py-2' : ''}`
      }
    >
      <Icon className="h-5 w-5 shrink-0" />
      <span>{label}</span>
    </NavLink>
  )
}

export function Layout({ children }: Props) {
  const cluster = useCluster()
  const prefixed = usePrefixedPath()
  const backTo = useBackTo()
  const [helpOpen, setHelpOpen] = useState(false)
  const [paletteOpen, setPaletteOpen] = useState(false)
  const showHelp = useCallback(() => setHelpOpen(true), [])
  const togglePalette = useCallback(() => setPaletteOpen((v) => !v), [])

  const navItems = [
    { to: prefixed('/'), label: 'Dashboard', icon: LayoutDashboard, exact: true },
    { to: prefixed('/machines'), label: 'Machines', icon: Server, exact: false },
    { to: prefixed('/agents'), label: 'Agents', icon: Bot, exact: false },
    { to: prefixed('/metrics'), label: 'Metrics', icon: BarChart3, exact: false },
    { to: prefixed('/logs'), label: 'Logs', icon: ScrollText, exact: false },
    { to: prefixed('/settings'), label: 'Settings', icon: Settings, exact: false },
  ]

  // Disable the global hook while either modal is open so their own
  // Esc/?/⌘K handlers are the only ones that fire (avoids close-then-reopen).
  useKeyboardShortcuts({
    onShowHelp: showHelp,
    enabled: !helpOpen && !paletteOpen,
  })
  useCommandPaletteShortcut({ onToggle: togglePalette, enabled: !helpOpen })

  useEffect(() => {
    const previousTitle = document.title
    document.title = cluster.name ? `Kyber: ${cluster.name}` : 'Kyber'
    return () => {
      document.title = previousTitle
    }
  }, [cluster.name])

  return (
    <div className="flex h-dvh flex-col bg-surface-base text-text-primary lg:flex-row">
      {/* Top bar — mobile */}
      <header className="flex items-center gap-2 border-b border-border-subtle bg-surface-raised px-4 pb-3 pt-[calc(env(safe-area-inset-top)+0.75rem)] lg:hidden">
        {backTo && (
          <Link
            to={backTo.href}
            data-testid="back-to-host"
            aria-label={`Back to ${backTo.label}`}
            className="-ml-1 flex h-9 w-9 shrink-0 items-center justify-center rounded-lg text-text-muted transition-colors hover:bg-surface-overlay hover:text-text-primary"
          >
            <ArrowLeft className="h-5 w-5" />
          </Link>
        )}
        <Gem className="h-5 w-5 shrink-0 text-accent" strokeWidth={1.5} />
        <h1 className="font-mono text-sm font-semibold uppercase tracking-[0.2em] text-text-primary">
          Kyber
        </h1>
        <ClusterIdentifier inline />
      </header>

      {/* Sidebar — desktop */}
      <nav className="hidden lg:flex lg:w-56 lg:flex-col lg:border-r lg:border-border-subtle lg:bg-surface-raised lg:p-4 lg:gap-1">
        {backTo && (
          <Link
            to={backTo.href}
            data-testid="back-to-host"
            className="mb-2 flex items-center gap-2 rounded-lg px-3 py-2 font-mono text-[11px] uppercase tracking-[0.15em] text-text-muted transition-colors hover:bg-surface-overlay hover:text-text-primary"
          >
            <ArrowLeft className="h-4 w-4 shrink-0" />
            <span>{backTo.label}</span>
          </Link>
        )}
        <div className="mb-6 px-3 py-2">
          <div className="flex items-center gap-2">
            <Gem className="h-5 w-5 text-accent" strokeWidth={1.5} />
            <h1 className="font-mono text-base font-semibold uppercase tracking-[0.2em] text-text-primary">
              Kyber
            </h1>
          </div>
          <ClusterIdentifier />
        </div>
        {navItems.map((item) => (
          <NavItem key={item.to} {...item} />
        ))}
      </nav>

      {/* Main content */}
      <main className="flex-1 overflow-y-auto pb-20 lg:pb-0">
        {/* App-wide, so an operator who navigated away mid-upgrade still sees
            what is happening to the cluster under them. */}
        <UpgradeBanner />
        <div className="mx-auto max-w-4xl px-4 py-6">{children}</div>
      </main>

      {/* Bottom tab bar — mobile */}
      <nav className="fixed bottom-0 left-0 right-0 border-t border-border-subtle bg-surface-raised pb-[env(safe-area-inset-bottom)] lg:hidden">
        <div className="flex justify-around">
          {navItems.map((item) => (
            <NavItem key={item.to} {...item} horizontal />
          ))}
        </div>
      </nav>

      <ShortcutHelpOverlay open={helpOpen} onClose={() => setHelpOpen(false)} />
      <CommandPalette open={paletteOpen} onOpenChange={setPaletteOpen} />
    </div>
  )
}
