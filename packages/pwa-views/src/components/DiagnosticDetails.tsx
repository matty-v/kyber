import { Check, Clipboard } from 'lucide-react'
import { useState } from 'react'

interface Props {
  details: string
  testId?: string
}

export function DiagnosticDetails({ details, testId }: Props) {
  const [copied, setCopied] = useState(false)

  async function copy() {
    try {
      await navigator.clipboard.writeText(details)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      setCopied(false)
    }
  }

  return (
    <details className="rounded border border-border-subtle bg-surface-overlay">
      <summary className="cursor-pointer select-none px-3 py-2 text-xs font-medium text-text-secondary">
        Technical details
      </summary>
      <div className="border-t border-border-subtle p-2">
        <div className="mb-2 flex justify-end">
          <button
            type="button"
            onClick={() => void copy()}
            className="inline-flex items-center gap-1 rounded px-2 py-1 text-[11px] text-text-muted hover:bg-surface-base hover:text-text-primary"
          >
            {copied ? <Check className="h-3 w-3" /> : <Clipboard className="h-3 w-3" />}
            {copied ? 'Copied' : 'Copy details'}
          </button>
        </div>
        <pre data-testid={testId} className="overflow-x-auto whitespace-pre-wrap break-words font-mono text-[11px] text-text-secondary">
          {details}
        </pre>
      </div>
    </details>
  )
}
