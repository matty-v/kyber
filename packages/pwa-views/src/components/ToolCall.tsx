import { useState } from 'react'
import { ChevronRight, Wrench } from 'lucide-react'

interface Props {
  name: string
  input: unknown
  result?: string
  isError?: boolean
}

export function ToolCall({ name, input, result, isError }: Props) {
  const [open, setOpen] = useState(false)
  return (
    <div className="text-sm">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        className={`inline-flex items-center gap-1 hover:text-text-primary ${isError ? 'text-danger' : 'text-text-muted'}`}
      >
        <ChevronRight className={`h-3.5 w-3.5 transition-transform ${open ? 'rotate-90' : ''}`} />
        <Wrench className="h-3.5 w-3.5" />
        <span className="font-medium">{name}</span>
      </button>
      {open && (
        <div className="mt-1 space-y-1">
          <pre className="max-h-64 overflow-y-auto whitespace-pre-wrap rounded-md bg-surface-sunken p-2 text-xs text-text-muted">{JSON.stringify(input, null, 2)}</pre>
          {result !== undefined && (
            <pre className={`max-h-64 overflow-y-auto whitespace-pre-wrap rounded-md p-2 text-xs ${isError ? 'bg-danger-muted text-danger' : 'bg-surface-sunken text-text-muted'}`}>{result}</pre>
          )}
        </div>
      )}
    </div>
  )
}
