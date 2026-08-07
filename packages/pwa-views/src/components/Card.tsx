import type { ReactNode, KeyboardEvent } from 'react'

interface Props {
  children: ReactNode
  className?: string
  onClick?: () => void
}

export function Card({ children, className = '', onClick }: Props) {
  const base =
    'rounded-xl border border-border-subtle bg-surface-raised p-4'
  const interactive = onClick
    ? 'cursor-pointer hover:border-border-default transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring'
    : ''

  function handleKeyDown(e: KeyboardEvent<HTMLDivElement>) {
    if (!onClick) return
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      onClick()
    }
  }

  return (
    <div
      // kyber-card: stable class hook so [data-density="compact"] selectors
      // in index.css can override the p-4 padding without depending on the
      // exact Tailwind class string here. See #105.
      className={`kyber-card ${base} ${interactive} ${className}`}
      onClick={onClick}
      onKeyDown={onClick ? handleKeyDown : undefined}
      role={onClick ? 'button' : undefined}
      tabIndex={onClick ? 0 : undefined}
    >
      {children}
    </div>
  )
}
