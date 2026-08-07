// Global test setup — runs before every test file.
// Registers @testing-library/jest-dom matcher extensions (toBeInTheDocument, etc.)
// so they're available in all *.test.tsx files without a per-file import.
import '@testing-library/jest-dom/vitest'
