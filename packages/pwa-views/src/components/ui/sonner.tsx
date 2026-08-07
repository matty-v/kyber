import { Toaster as SonnerPrimitive } from 'sonner'
import type { ComponentProps } from 'react'

type ToasterProps = ComponentProps<typeof SonnerPrimitive>

export function Toaster(props: ToasterProps) {
  return (
    <SonnerPrimitive
      theme="dark"
      position="bottom-right"
      richColors={false}
      closeButton
      toastOptions={{
        classNames: {
          toast:
            'group !bg-surface-overlay !text-text-primary !border !border-border-default !rounded-lg !shadow-xl',
          description: '!text-text-muted',
          actionButton:
            '!bg-accent !text-surface-base hover:!opacity-90',
          cancelButton:
            '!bg-surface-raised !text-text-secondary hover:!text-text-primary',
          closeButton:
            '!bg-surface-raised !border-border-subtle !text-text-muted hover:!text-text-primary',
          success:
            '!border-success/40 [&_[data-icon]]:!text-success',
          error:
            '!border-danger/40 [&_[data-icon]]:!text-danger',
          info:
            '!border-info/40 [&_[data-icon]]:!text-info',
          warning:
            '!border-warn/40 [&_[data-icon]]:!text-warn',
        },
      }}
      {...props}
    />
  )
}
