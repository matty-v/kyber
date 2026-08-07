// DebugResultsPanel (#208 Phase 3) — shared rendering for the inbound-debug
// Decision diagnostic. Used by both the standalone InboundDebugger in the
// WebhooksTab and the "Test payload" affordance embedded in AddWebhookWizard's
// Action step. Keeps both surfaces visually identical so operators learn the
// shape once.

import { Check, X } from 'lucide-react'
import type { InboundDebugResponse } from '../lib/types'

interface Props {
  result: InboundDebugResponse
}

export function DebugResultsPanel({ result }: Props) {
  const matched = result.match
  return (
    <div className="space-y-3">
      <MatchBanner match={matched} dropReason={result.dropReason} />

      <div>
        <div className="text-[11px] font-medium uppercase tracking-wide text-text-muted">
          Resolved event
        </div>
        <div className="mt-0.5 font-mono text-xs text-text-primary">
          {result.resolvedEvent || '—'}
        </div>
      </div>

      <FilterResults filters={result.filterResults} />
      <FieldResults fields={result.fieldResults} />

      {matched && result.envelope !== undefined && (
        <div>
          <div className="text-[11px] font-medium uppercase tracking-wide text-text-muted">
            Rendered envelope
          </div>
          <pre className="mt-1 overflow-x-auto rounded-lg border border-border-subtle bg-surface-base p-3 text-xs font-mono text-text-secondary">
            {result.envelope}
          </pre>
        </div>
      )}
    </div>
  )
}

function MatchBanner({ match, dropReason }: { match: boolean; dropReason?: string }) {
  if (match) {
    return (
      <div
        role="status"
        aria-label="Matched"
        className="flex items-center gap-2 rounded-lg border border-success/40 bg-success/10 px-3 py-2 text-sm text-success"
      >
        <Check className="h-4 w-4" strokeWidth={2.5} />
        <span className="font-medium">Matched</span>
      </div>
    )
  }
  return (
    <div
      role="status"
      aria-label="Dropped"
      className="flex items-center gap-2 rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger"
    >
      <X className="h-4 w-4" strokeWidth={2.5} />
      <span className="font-medium">
        Dropped{dropReason ? `: ${dropReason}` : ''}
      </span>
    </div>
  )
}

function FilterResults({ filters }: { filters: InboundDebugResponse['filterResults'] }) {
  return (
    <div>
      <div className="text-[11px] font-medium uppercase tracking-wide text-text-muted">
        Filters
      </div>
      {filters.length === 0 ? (
        <div className="mt-0.5 text-xs text-text-muted">No filters configured.</div>
      ) : (
        <ul className="mt-1 space-y-1">
          {filters.map((f, i) => (
            <li
              key={`${f.jsonPath}-${i}`}
              className={`flex items-start gap-2 rounded-md border px-2.5 py-1.5 text-xs ${
                f.passed
                  ? 'border-success/30 bg-success/5'
                  : 'border-danger/30 bg-danger/5'
              }`}
            >
              <span className="mt-0.5 shrink-0">
                {f.passed ? (
                  <Check className="h-3.5 w-3.5 text-success" strokeWidth={2.5} />
                ) : (
                  <X className="h-3.5 w-3.5 text-danger" strokeWidth={2.5} />
                )}
              </span>
              <div className="min-w-0 flex-1">
                <div className="font-mono text-text-primary">{f.jsonPath}</div>
                <div className="mt-0.5 text-text-muted">
                  value:{' '}
                  <span className="font-mono text-text-secondary">
                    {f.extractedValue || '—'}
                  </span>
                </div>
                {!f.passed && f.reason && (
                  <div className="mt-0.5 text-danger">{f.reason}</div>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function FieldResults({ fields }: { fields: InboundDebugResponse['fieldResults'] }) {
  return (
    <div>
      <div className="text-[11px] font-medium uppercase tracking-wide text-text-muted">
        Fields
      </div>
      {fields.length === 0 ? (
        <div className="mt-0.5 text-xs text-text-muted">No fields configured.</div>
      ) : (
        <ul className="mt-1 space-y-1">
          {fields.map((f, i) => (
            <li
              key={`${f.label}-${i}`}
              className="rounded-md border border-border-subtle bg-surface-base px-2.5 py-1.5 text-xs"
            >
              <div className="flex items-baseline justify-between gap-2">
                <span className="font-medium text-text-primary">{f.label}</span>
                {f.truncated && (
                  <span className="rounded-full bg-warn/15 px-1.5 py-0.5 text-[10px] font-medium text-warn">
                    truncated
                  </span>
                )}
              </div>
              <div className="mt-0.5 font-mono text-[11px] text-text-muted">{f.jsonPath}</div>
              <div className="mt-0.5 font-mono text-text-secondary break-all">
                {f.extractedValue || '—'}
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
