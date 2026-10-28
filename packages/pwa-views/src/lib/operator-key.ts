// These values live only in the active form. Never place prepared keys or
// mutation variables in query caches, storage, URLs, telemetry or logs.
export function prepareOperatorKey(): { key: string; operationId: string } {
  const random = globalThis.crypto
  if (!random?.getRandomValues) throw new Error('Secure key generation is unavailable in this browser.')
  const material = random.getRandomValues(new Uint8Array(32))
  const key = btoa(String.fromCharCode(...material)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
  return { key, operationId: newOperatorOperationId() }
}

export function newOperatorOperationId(): string {
  const random = globalThis.crypto
  if (!random?.getRandomValues) throw new Error('Secure operation generation is unavailable in this browser.')
  const id = random.getRandomValues(new Uint8Array(16))
  id[6] = (id[6] & 0x0f) | 0x40
  id[8] = (id[8] & 0x3f) | 0x80
  const hex = [...id].map((value) => value.toString(16).padStart(2, '0')).join('')
  const operationId = `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
  return operationId
}
