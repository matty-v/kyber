import type { TokenUsage } from '../lib/types'
import { Card } from './Card'

type Props = {
  data: TokenUsage | null | undefined
  isLoading: boolean
}

export function TokenUsageCard({ data, isLoading }: Props) {
  if (isLoading) {
    return (
      <Card>
        <h2 className="text-sm font-medium text-text-muted mb-3">Token budget</h2>
        <div className="text-xs text-text-muted">Scanning…</div>
      </Card>
    )
  }
  if (!data) {
    return (
      <Card>
        <h2 className="text-sm font-medium text-text-muted mb-3">Token budget</h2>
        <div className="text-xs text-text-muted">
          No token data yet — reporter starts on first assistant message.
        </div>
      </Card>
    )
  }

  const { tokens, percentage, model, effortLevel, speed } = data
  // #396: when the context window is unknown (model absent from the operator
  // ConfigMap → 200K floor), the % is a guess against a placeholder limit, so
  // mark it as an estimate instead of a confident number / "over budget".
  // Only an explicit `false` counts as unknown (older payloads omit the field).
  const windowUnknown = data.contextWindowKnown === false
  const overBudget = percentage > 100 && !windowUnknown
  const barColor =
    windowUnknown ? 'bg-text-muted'
    : percentage >= 90 ? 'bg-danger'
    : percentage >= 75 ? 'bg-warn'
    : 'bg-accent'

  return (
    <Card>
      <h2 className="text-sm font-medium text-text-muted mb-3">Token budget</h2>
      <div className="flex gap-2 flex-wrap mb-3">
        <span className="text-xs font-mono px-2 py-0.5 rounded bg-accent-muted text-accent">
          {model}
        </span>
        {effortLevel && (
          <span className={`text-xs font-mono px-2 py-0.5 rounded ${
            effortLevel === 'high' ? 'bg-success-muted text-success'
            : effortLevel === 'medium' ? 'bg-warn-muted text-warn'
            : 'bg-surface-overlay text-text-muted'
          }`}>
            {effortLevel}
          </span>
        )}
        {speed === 'fast' && (
          <span className="text-xs font-mono px-2 py-0.5 rounded bg-warn-muted text-warn">
            fast
          </span>
        )}
      </div>
      <div className="flex justify-between text-xs text-text-muted mb-1">
        <span>
          {formatTokens(tokens.used)} / {formatTokens(tokens.limit)}
          {windowUnknown && (
            <span className="ml-1 italic" title="This model's context window isn't in the kyber-model-context-windows ConfigMap, so the limit is an estimated 200K floor. Add the model to the ConfigMap to correct it.">
              · unverified window
            </span>
          )}
        </span>
        <span className={overBudget ? 'text-danger font-medium' : undefined}>
          {windowUnknown ? '≈ ' : ''}{percentage.toFixed(1)}%{overBudget && ' · over budget'}
        </span>
      </div>
      <div className="h-2 bg-surface-overlay rounded overflow-hidden">
        <div
          className={`h-full ${barColor} transition-all`}
          style={{ width: `${Math.min(100, percentage)}%` }}
        />
      </div>
    </Card>
  )
}

function formatTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`
  return String(n)
}
