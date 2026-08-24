// Global test setup — runs before every test file.
// Registers @testing-library/jest-dom matcher extensions (toBeInTheDocument, etc.)
// so they're available in all *.test.tsx files without a per-file import.
import '@testing-library/jest-dom/vitest'
import { vi } from 'vitest'

// Node 26 exposes an experimental process-global localStorage whose getter is
// unavailable without --localstorage-file. Supply the browser contract tests
// need instead of allowing Node's process-global implementation to shadow it.
const storage = new Map<string, string>()
vi.stubGlobal('localStorage', {
  get length() { return storage.size },
  clear: () => storage.clear(),
  getItem: (key: string) => storage.get(key) ?? null,
  key: (index: number) => [...storage.keys()][index] ?? null,
  removeItem: (key: string) => storage.delete(key),
  setItem: (key: string, value: string) => storage.set(key, String(value)),
} satisfies Storage)
