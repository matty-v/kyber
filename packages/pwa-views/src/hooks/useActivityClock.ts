// One subscription timer shared by every activity consumer. Lease ageing never
// waits for a successful refetch, including after an offline transition.
import { useSyncExternalStore } from 'react'
let current = 0
let timer: ReturnType<typeof setInterval> | undefined
const subscribers = new Set<() => void>()
function subscribe(listener: () => void) {
  subscribers.add(listener)
  if (!timer) timer = setInterval(() => { current++; for (const notify of subscribers) notify() }, 5000)
  return () => { subscribers.delete(listener); if (!subscribers.size && timer) { clearInterval(timer); timer = undefined } }
}
export function useActivityClock() {
  useSyncExternalStore(subscribe, () => current, () => 0)
  return performance.now()
}
