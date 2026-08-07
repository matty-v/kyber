import { forwardRef } from 'react'
import type { ReactNode, ButtonHTMLAttributes } from 'react'

type Variant = 'primary' | 'secondary' | 'danger' | 'ghost'
type Size = 'sm' | 'md' | 'lg'

interface Props extends ButtonHTMLAttributes<HTMLButtonElement> {
  children: ReactNode
  variant?: Variant
  size?: Size
  loading?: boolean
}

const variantClasses: Record<Variant, string> = {
  primary:
    'bg-accent text-surface-base hover:brightness-110 disabled:opacity-40',
  secondary:
    'bg-surface-overlay text-text-primary hover:bg-border-default disabled:opacity-40',
  danger:
    'bg-danger text-surface-base hover:brightness-110 disabled:opacity-40',
  ghost:
    'bg-transparent text-text-secondary hover:text-text-primary hover:bg-surface-overlay disabled:opacity-40',
}

const sizeClasses: Record<Size, string> = {
  sm: 'px-2.5 py-1.5 text-xs min-h-[32px]',
  md: 'px-3.5 py-2 text-sm min-h-[40px]',
  lg: 'px-5 py-2.5 text-base min-h-[44px]',
}

export const Button = forwardRef<HTMLButtonElement, Props>(function Button(
  {
    children,
    variant = 'secondary',
    size = 'md',
    loading = false,
    disabled,
    className = '',
    ...props
  },
  ref,
) {
  return (
    <button
      ref={ref}
      {...props}
      disabled={disabled ?? loading}
      className={`inline-flex items-center justify-center gap-1.5 rounded-lg font-medium transition-all focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring disabled:cursor-not-allowed ${variantClasses[variant]} ${sizeClasses[size]} ${className}`}
    >
      {loading && (
        <svg
          className="h-3.5 w-3.5 animate-spin"
          viewBox="0 0 24 24"
          fill="none"
        >
          <circle
            className="opacity-25"
            cx="12"
            cy="12"
            r="10"
            stroke="currentColor"
            strokeWidth="4"
          />
          <path
            className="opacity-75"
            fill="currentColor"
            d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"
          />
        </svg>
      )}
      {children}
    </button>
  )
})
