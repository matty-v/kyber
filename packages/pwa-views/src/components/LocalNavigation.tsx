import { useEffect, useRef, useState, type ReactNode } from 'react'
import { Menu, X } from 'lucide-react'
import { Button } from './Button'

export type LocalNavigationItem<T extends string> = {
  id: T
  label: string
  description: string
}

export type LocalNavigationGroup<T extends string> = {
  label: string
  items: LocalNavigationItem<T>[]
}

function NavigationList<T extends string>({
  groups,
  value,
  onChange,
}: {
  groups: LocalNavigationGroup<T>[]
  value: T
  onChange: (value: T) => void
}) {
  return (
    <nav aria-label="Section pages" className="space-y-5">
      {groups.map((group) => (
        <div key={group.label}>
          <h2 className="mb-1 px-3 text-[11px] font-semibold uppercase tracking-wider text-text-muted">{group.label}</h2>
          <div className="space-y-1">
            {group.items.map((item) => {
              const active = item.id === value
              return (
                <button
                  key={item.id}
                  type="button"
                  aria-current={active ? 'page' : undefined}
                  onClick={() => onChange(item.id)}
                  className={`w-full rounded-lg px-3 py-2 text-left transition-colors ${active ? 'bg-accent-muted text-accent' : 'text-text-secondary hover:bg-surface-overlay hover:text-text-primary'}`}
                >
                  <span className="block text-sm font-medium">{item.label}</span>
                  <span className={`mt-0.5 block text-xs leading-snug ${active ? 'text-accent' : 'text-text-muted'}`}>{item.description}</span>
                </button>
              )
            })}
          </div>
        </div>
      ))}
    </nav>
  )
}

export function LocalNavigation<T extends string>({
  title,
  value,
  groups,
  onChange,
  children,
}: {
  title: string
  value: T
  groups: LocalNavigationGroup<T>[]
  onChange: (value: T) => void
  children: ReactNode
}) {
  const [mobileOpen, setMobileOpen] = useState(false)
  const triggerRef = useRef<HTMLButtonElement>(null)
  const closeRef = useRef<HTMLButtonElement>(null)
  const items = groups.flatMap((group) => group.items)
  const activeItem = items.find((item) => item.id === value) ?? items[0]

  useEffect(() => {
    if (!mobileOpen) return

    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    closeRef.current?.focus()

    function handleKeyDown(event: KeyboardEvent) {
      if (event.key === 'Escape') setMobileOpen(false)
    }

    window.addEventListener('keydown', handleKeyDown)
    return () => {
      window.removeEventListener('keydown', handleKeyDown)
      document.body.style.overflow = previousOverflow
      triggerRef.current?.focus()
    }
  }, [mobileOpen])

  if (!activeItem) return <main className="min-w-0">{children}</main>

  function select(nextValue: T) {
    setMobileOpen(false)
    onChange(nextValue)
  }

  return (
    <>
      <button
        ref={triggerRef}
        type="button"
        onClick={() => setMobileOpen(true)}
        className="mb-4 flex w-full items-center justify-between rounded-lg border border-border-default bg-surface-raised px-4 py-3 text-left sm:hidden"
        aria-haspopup="dialog"
      >
        <span>
          <span className="block text-sm font-semibold text-text-primary">{activeItem.label}</span>
          <span className="block text-xs text-text-muted">{activeItem.description}</span>
        </span>
        <Menu className="h-5 w-5 text-text-secondary" />
      </button>

      <div className="sm:grid sm:grid-cols-[12rem_minmax(0,1fr)] sm:items-start sm:gap-6">
        <aside className="hidden sm:sticky sm:top-4 sm:block">
          <NavigationList groups={groups} value={value} onChange={select} />
        </aside>
        <main className="min-w-0">{children}</main>
      </div>

      {mobileOpen && (
        <div className="fixed inset-0 z-50 bg-surface-base sm:hidden" role="dialog" aria-modal="true" aria-label={`${title} navigation`}>
          <div className="flex items-center justify-between border-b border-border-subtle px-4 py-3">
            <div>
              <p className="text-sm font-semibold text-text-primary">{title}</p>
              <p className="text-xs text-text-muted">Navigation</p>
            </div>
            <Button ref={closeRef} variant="ghost" size="sm" onClick={() => setMobileOpen(false)} aria-label="Close navigation">
              <X className="h-5 w-5" />
            </Button>
          </div>
          <div className="h-[calc(100dvh-4rem)] overflow-y-auto p-4 pb-24">
            <NavigationList groups={groups} value={value} onChange={select} />
          </div>
        </div>
      )}
    </>
  )
}
