import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'

interface Props {
  icon?: ReactNode
  title: string
  description?: string
  action?: ReactNode
  className?: string
}

export function EmptyState({ icon, title, description, action, className }: Props) {
  return (
    <div
      className={cn(
        'flex flex-col items-center justify-center rounded-xl border border-border-subtle bg-surface-raised px-6 py-10 text-center',
        className,
      )}
    >
      <div className="relative mb-4 flex h-20 w-20 items-center justify-center">
        <CrystalGlyph className="absolute inset-0 h-full w-full text-accent opacity-[0.18]" />
        {icon && (
          <div className="relative z-10 text-text-secondary">{icon}</div>
        )}
      </div>
      <p className="font-mono text-[10px] uppercase tracking-[0.2em] text-text-muted">
        {title}
      </p>
      {description && (
        <p className="mt-2 max-w-xs text-sm text-text-muted">{description}</p>
      )}
      {action && <div className="mt-5">{action}</div>}
    </div>
  )
}

function CrystalGlyph({ className }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 48 48"
      fill="none"
      stroke="currentColor"
      strokeWidth="1"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden="true"
    >
      <path d="M12 18 L24 4 L36 18 L24 44 Z" />
      <path d="M12 18 L36 18" />
      <path d="M18 18 L24 4" opacity="0.6" />
      <path d="M30 18 L24 4" opacity="0.6" />
      <path d="M18 18 L24 44" opacity="0.6" />
      <path d="M30 18 L24 44" opacity="0.6" />
    </svg>
  )
}
