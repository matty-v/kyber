import type { OperatorCredential, OperatorCredentialUsage, OperatorCredentialUsageIdentity } from './types'

export const usageActionLabels: Record<string, string> = {
  'api:read': 'Read other Kyber data', 'api:write': 'Change other Kyber settings',
  'agents:read': 'Read Agents', 'agents:write': 'Manage Agents',
  'machines:read': 'Read Machines', 'machines:write': 'Manage Machines',
  'tasks:read': 'Read tasks', 'tasks:write': 'Submit or manage tasks',
  'requests:read': 'Read requests', 'requests:write': 'Submit requests',
  'usage:read': 'Read token usage', 'credentials:read': 'Read key metadata',
  'credentials:write': 'Manage keys', 'audit:read': 'Read access history',
  'agents:exec': 'Open Agent terminals', 'machines:exec': 'Open Machine terminals',
  'logs:read': 'Read logs', 'transcripts:read': 'Read conversations',
  'secrets:list': 'List Agent credentials', 'secrets:read': 'Read Agent credentials',
  'secrets:write': 'Change Agent credentials', 'archives:read': 'Read archive metadata',
  'archives:write': 'Manage archives', 'archives:download': 'Download archives',
  'browser:sign-in': 'Sign in to a browser',
}
const transports = new Set(['bearer', 'browser-session', 'archive-capability'])
const authorities = new Set(['configuration', 'registry'])
const entryPoints = new Set(['operator-api', 'browser-sign-in', 'archive-download'])
const countPattern = /^(0|[1-9][0-9]{0,79})$/
export function usageCount(value: unknown): bigint | undefined {
  return typeof value === 'string' && countPattern.test(value) ? BigInt(value) : undefined
}
export function formatUsageCount(value: bigint): string { return new Intl.NumberFormat().format(value) }
function record(value: unknown): value is Record<string, unknown> { return !!value && typeof value === 'object' && !Array.isArray(value) }
function date(value: unknown): value is string { return typeof value === 'string' && Number.isFinite(Date.parse(value)) }
function opaque(value: unknown): value is string { return typeof value === 'string' && value.length > 0 && value.length <= 512 && !/[\x00-\x1f\x7f]/.test(value) }
function identity(value: unknown): value is OperatorCredentialUsageIdentity {
  return record(value) && opaque(value.credentialId) && opaque(value.principalId) && opaque(value.tenantId) && typeof value.authority === 'string' && authorities.has(value.authority)
}
export function usageIdentityKey(value: Pick<OperatorCredentialUsageIdentity, 'credentialId' | 'principalId' | 'tenantId'>): string {
  return `${value.credentialId}\0${value.principalId}\0${value.tenantId}`
}
// Reject malformed/unsupported data instead of converting missing counters into
// successful zeroes. Decimal strings stay exact across the API/browser boundary.
export function validOperatorCredentialUsage(value: unknown): value is OperatorCredentialUsage {
  if (!record(value) || value.version !== 1 || value.partial !== true || typeof value.truncated !== 'boolean' || !date(value.observedAt)) return false
  if (!Array.isArray(value.entryPoints) || !value.entryPoints.every((item) => typeof item === 'string' && entryPoints.has(item))) return false
  if (!Array.isArray(value.identities) || value.identities.length > 128 || !value.identities.every(identity) || !Array.isArray(value.items) || value.items.length > 768) return false
  const known = new Set(value.identities.map(usageIdentityKey))
  if (known.size !== value.identities.length || !record(value.coverage)) return false
  const coverage = value.coverage
  if (typeof coverage.currentObserverReported !== 'boolean' || typeof coverage.currentObserverFresh !== 'boolean') return false
  if (![coverage.freshObservers, coverage.staleObservers].every((n) => Number.isSafeInteger(n) && Number(n) >= 0)) return false
  if ((coverage.since !== undefined && !date(coverage.since)) || (coverage.latestProgress !== undefined && !date(coverage.latestProgress))) return false
  if (![coverage.dropped, coverage.unrecognized, coverage.unattributed, coverage.unavailable].every((n) => usageCount(n) !== undefined)) return false
  if (value.local !== undefined) {
    if (!record(value.local) || typeof value.local.closed !== 'boolean' || !Number.isSafeInteger(value.local.pendingObservations) || Number(value.local.pendingObservations) < 0 || usageCount(value.local.lossReportsAwaitingConfirmation) === undefined) return false
  }
  const seen = new Set<string>()
  for (const item of value.items) {
    if (!identity(item) || !known.has(usageIdentityKey(item)) || !record(item) || typeof item.transport !== 'string' || !transports.has(item.transport)) return false
    const key = `${usageIdentityKey(item)}\0${item.authority}\0${item.transport}`
    if (seen.has(key)) return false
    seen.add(key)
    const authenticated = usageCount(item.authenticated), denied = usageCount(item.denied), generation = usageCount(item.generation)
    if (authenticated === undefined || denied === undefined || generation === undefined || !record(item.actions)) return false
    if (authenticated > 0n && (!date(item.lastAcceptedAt) || generation === 0n || generation > 18446744073709551615n)) return false
    if (item.lastAcceptedAt !== undefined && !date(item.lastAcceptedAt)) return false
    let total = 0n
    if (Object.keys(item.actions).length > 64) return false
    for (const [action, count] of Object.entries(item.actions)) {
      const parsed = usageCount(count)
      if (!/^[a-z][a-z0-9:-]{0,63}$/.test(action) || parsed === undefined) return false
      total += parsed
    }
    if (total !== authenticated) return false
  }
  return true
}
export function summarizeCredentialUsage(data: OperatorCredentialUsage, credential: OperatorCredential) {
  const key = usageIdentityKey({ credentialId: credential.id, principalId: credential.principalId ?? '', tenantId: credential.tenantId ?? '' })
  if (!data.identities.some((item) => usageIdentityKey(item) === key)) return undefined
  const summary = { authenticated: 0n, denied: 0n, lastAcceptedAt: undefined as string | undefined, actions: new Map<string, bigint>(), transports: new Map<string, bigint>() }
  for (const item of data.items) {
    if (usageIdentityKey(item) !== key) continue
    summary.authenticated += BigInt(item.authenticated); summary.denied += BigInt(item.denied)
    summary.transports.set(item.transport, (summary.transports.get(item.transport) ?? 0n) + BigInt(item.authenticated))
    for (const [action, count] of Object.entries(item.actions)) summary.actions.set(action, (summary.actions.get(action) ?? 0n) + BigInt(count))
    if (item.lastAcceptedAt && (!summary.lastAcceptedAt || Date.parse(item.lastAcceptedAt) > Date.parse(summary.lastAcceptedAt))) summary.lastAcceptedAt = item.lastAcceptedAt
  }
  return summary
}
