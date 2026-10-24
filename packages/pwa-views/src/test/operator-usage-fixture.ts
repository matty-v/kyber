import type { OperatorCredential, OperatorCredentialUsage } from '../lib/types'

export const usageCredential: OperatorCredential = { id: 'admin', principalId: 'principal-admin', tenantId: 'installation', activityTrackable: true, name: 'Operations', displayName: 'Operations', accessLevel: 'administrator', state: 'accepted', source: 'managed', scopes: [], agentResources: ['*'], permissions: [], configured: false }
export function usageView(): OperatorCredentialUsage {
  const identity = { credentialId: 'admin', principalId: 'principal-admin', tenantId: 'installation', authority: 'registry' as const }
  return {
    version: 1, observedAt: '2026-10-24T12:05:00Z', partial: true, truncated: false,
    entryPoints: ['operator-api', 'browser-sign-in', 'archive-download'], identities: [identity],
    coverage: { since: '2026-10-24T12:00:00Z', latestProgress: '2026-10-24T12:05:00Z', currentObserverReported: true, currentObserverFresh: true, freshObservers: 1, staleObservers: 0, dropped: '0', unrecognized: '0', unattributed: '0', unavailable: '0' },
    local: { pendingObservations: 0, lossReportsAwaitingConfirmation: '0', closed: false },
    items: [{ ...identity, transport: 'browser-session', authenticated: '9007199254740993', denied: '2', generation: '2', lastAcceptedAt: '2026-10-24T12:04:00Z', actions: { 'agents:read': '9007199254740993' } }],
  }
}
