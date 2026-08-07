# PWA — developer notes

## Component testing

### Setup

Testing infrastructure lives in `src/test/setup.ts`. It registers
`@testing-library/jest-dom` matchers (e.g. `toBeInTheDocument`,
`toHaveTextContent`) via `@testing-library/jest-dom/vitest` so those
matchers are available in every `*.test.tsx` file without a per-file import.

Vitest is configured with `environment: 'jsdom'` (`vitest.config.ts`).
The repo convention is **explicit imports** of `describe`, `it`, `expect`,
`vi` from `vitest` — matches every existing `*.test.ts` and `*.test.tsx`
in the repo. (`globals: true` is also enabled in the config so unimported
references would resolve, but no test in the repo relies on that today.)

### What to import

```tsx
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
```

### Typical pattern

```tsx
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MyComponent } from './MyComponent'

describe('MyComponent', () => {
  it('renders the expected text', () => {
    render(<MyComponent label="Hello" />)
    expect(screen.getByText('Hello')).toBeInTheDocument()
  })

  it('calls the handler on click', async () => {
    const user = userEvent.setup()
    const handleClick = vi.fn()
    render(<MyComponent onClick={handleClick} />)
    await user.click(screen.getByRole('button'))
    expect(handleClick).toHaveBeenCalledOnce()
  })
})
```

### Radix UI components

Components that use Radix Tooltip must be wrapped in `<TooltipProvider>`.
Components that use Radix Popover render into a Portal — `screen` queries
still find the content because jsdom maintains a single document.

### Mocking hooks

For components that call data-fetching hooks, mock the entire module
at the top of the test file and control return values per test:

```tsx
vi.mock('../hooks/useAPI', () => ({
  useMyHook: vi.fn(),
}))

import * as useAPIModule from '../hooks/useAPI'

beforeEach(() => {
  vi.mocked(useAPIModule.useMyHook).mockReturnValue({
    data: [],
    isLoading: false,
    error: null,
  } as unknown as ReturnType<typeof useAPIModule.useMyHook>)
})
```

The `as unknown as ReturnType<...>` cast is required for hooks that return
React Query types (`UseQueryResult`/`UseMutationResult`). Those types are
wide discriminated unions with 20+ fields, so partial fixtures don't
structurally overlap them and TS rejects the direct cast (TS2352).

### Running tests

```bash
# All tests
npm test

# Single file
npm test -- src/components/MyComponent.test.tsx

# Type-check (must stay clean)
npm run lint
```
