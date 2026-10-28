import type { OperatorCredential, OperatorCredentialUsage } from '../lib/types'
import { formatUsageCount, summarizeCredentialUsage, usageActionLabels } from '../lib/operator-usage'

export function OperatorKeyUsageRow({ credential, data, enabled }: { credential: OperatorCredential; data?: OperatorCredentialUsage; enabled: boolean }) {
  if (!enabled) return <p className="mt-1 text-xs text-text-muted">Last use unavailable</p>
  if (credential.activityUnavailableReason === 'shared-key') return <p className="mt-2 text-xs text-text-muted">Activity is recorded under the identity shown above.</p>
  if (credential.activityUnavailableReason === 'identity-shared') return <p className="mt-2 text-xs text-warning">Activity cannot be distinguished for keys sharing this identity.</p>
  if (!credential.activityTrackable) return <p className="mt-2 text-xs text-text-muted">A complete key identity is needed to record activity separately.</p>
  if (!data || !data.coverage.since) return <p className="mt-2 text-xs text-text-muted">Last use unavailable</p>
  const summary = summarizeCredentialUsage(data, credential)
  if (!summary) return <p className="mt-2 text-xs text-text-muted">Activity for this key has not been loaded.</p>
  const requestTypes = [...summary.actions].filter(([, count]) => count > 0n).sort((a, b) => a[1] === b[1] ? a[0].localeCompare(b[0]) : a[1] > b[1] ? -1 : 1)
  const methods: Record<string, string> = { bearer: 'API key', 'browser-session': 'Browser session', 'archive-capability': 'Download link' }
  return <div className="mt-3 space-y-1 text-xs text-text-muted">
    <p>{summary.lastAcceptedAt ? `Last observed use ${new Date(summary.lastAcceptedAt).toLocaleString()}` : summary.denied > 0n ? 'Last observed-use time unavailable' : 'No accepted use recorded in this window.'}</p>
    <p className="break-words"><span className="text-text-primary">{formatUsageCount(summary.authenticated)}</span> authenticated requests · <span className="text-text-primary">{formatUsageCount(summary.denied)}</span> denied after authentication</p>
    {requestTypes.length > 0 && <details><summary className="cursor-pointer">Observed request types</summary><ul className="mt-2 space-y-1">{requestTypes.map(([action, count]) => <li key={action} className="flex items-start justify-between gap-3"><span>{typeof usageActionLabels[action] === 'string' ? usageActionLabels[action] : 'Other API requests'}</span><span className="break-all tabular-nums">{formatUsageCount(count)}</span></li>)}</ul>
      <ul className="mt-3 space-y-1 border-t border-border-subtle pt-2">{[...summary.transports].filter(([, count]) => count > 0n).map(([method, count]) => <li key={method}>{methods[method] ?? 'Other authentication'}: {formatUsageCount(count)}</li>)}</ul>
    </details>}
  </div>
}

export function OperatorKeyUsageCoverage({ data }: { data: OperatorCredentialUsage }) {
  const lossPending = data.local && data.local.lossReportsAwaitingConfirmation !== '0'
  const delayed = !data.coverage.currentObserverFresh || data.local?.closed
  return <div className="mb-4 space-y-2 rounded-lg border border-border-subtle p-3 text-xs text-text-muted">
    <p className="font-medium text-text-primary">Observed key activity · Partial coverage</p>
    {data.coverage.since ? <p>Observed since {new Date(data.coverage.since).toLocaleString()}.{data.coverage.latestProgress && <> Updated {new Date(data.coverage.latestProgress).toLocaleString()}.</>}</p> : <p role="status">Activity collection is starting.</p>}
    <p>Authenticated requests show a key was accepted. Action outcomes are in Recent access.</p>
    {data.coverage.since && delayed && <p role="status" className="text-warning">This server’s activity reports are delayed.</p>}
    {(data.coverage.dropped !== '0' || lossPending) && <p role="alert" className="text-warning">Some activity observations were lost.</p>}
    {!!data.local && data.local.pendingObservations >= 256 && <p role="status">Activity observations are awaiting confirmation.</p>}
    {data.truncated && <p role="status">Activity for some keys has not been loaded.</p>}
    <details><summary className="cursor-pointer">Coverage details</summary><div className="mt-2 space-y-1">
      <p>Includes operator API requests, browser sign-ins and archive-link downloads. Long-lived connections count their initial request.</p>
      <p>Lost observations recorded: {formatUsageCount(BigInt(data.coverage.dropped))}</p>
      <p>Accepted requests without a stable identity: {formatUsageCount(BigInt(data.coverage.unattributed))}</p>
      <p>Rejected requests without an identified key: {formatUsageCount(BigInt(data.coverage.unrecognized))}</p>
      <p>Unavailable authentication checks: {formatUsageCount(BigInt(data.coverage.unavailable))}</p>
      {data.local && <p>Observations awaiting confirmation: {data.local.pendingObservations.toLocaleString()}. Loss reports awaiting confirmation: {formatUsageCount(BigInt(data.local.lossReportsAwaitingConfirmation))}.</p>}
      {data.coverage.staleObservers > 0 && <p>Some previous collection runs stopped reporting.</p>}
      <p>Shared keys identify their configured consumer.</p>
    </div></details>
  </div>
}
