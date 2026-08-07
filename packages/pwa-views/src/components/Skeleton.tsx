import { cn } from '@/lib/utils'

interface Props {
  className?: string
}

export function Skeleton({ className }: Props) {
  return (
    <div
      className={cn('rounded-md bg-surface-overlay', className)}
      style={{
        backgroundImage:
          'linear-gradient(90deg, var(--color-surface-overlay) 0%, var(--color-border-default) 50%, var(--color-surface-overlay) 100%)',
        backgroundSize: '200% 100%',
        animation: 'kyber-shimmer 1.8s linear infinite',
      }}
    />
  )
}
