import { useState } from 'react'
import { Brain, ChevronRight } from 'lucide-react'

export function ThinkingBlock({ text }: { text: string }) {
  const [open, setOpen] = useState(false)
  return (
    <div className="text-sm">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        className="inline-flex items-center gap-1 text-text-muted hover:text-text-primary"
      >
        <ChevronRight className={`h-3.5 w-3.5 transition-transform ${open ? 'rotate-90' : ''}`} />
        <Brain className="h-3.5 w-3.5" />
        Thinking
      </button>
      {open && <pre className="mt-1 whitespace-pre-wrap rounded-md bg-surface-sunken p-2 text-xs text-text-muted">{text}</pre>}
    </div>
  )
}
