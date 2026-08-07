// IdentitySection tests — the section now drives data from
// useComputeConfig + useGitHubRepos + useGitHubRepoExists, so we mock the
// hooks module rather than mounting MSW. Tests cover the three modes,
// the existing-mode dropdown, the freeform fallback, and the live
// collision badge wiring back into wizard state.

import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

vi.mock('../../hooks/useAPI', () => ({
  useComputeConfig: vi.fn(),
  useGitHubRepos: vi.fn(),
  useGitHubRepoExists: vi.fn(),
}))

import * as useAPIModule from '../../hooks/useAPI'
import { IdentitySection } from './IdentitySection'
import { initialWizardState } from './types'

function makeQueryClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } })
}

function renderWithQuery(ui: React.ReactElement) {
  const queryClient = makeQueryClient()
  return render(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>)
}

// `as unknown as ReturnType<...>` per pwa/CLAUDE.md — React Query types are
// too wide to construct from a partial.
function configResult(repoOwner = 'matty-v') {
  return {
    data: { compute: { provider: 'mock' }, models: [], identity: { repoOwner } },
    isSuccess: true,
    isLoading: false,
    error: null,
  } as unknown as ReturnType<typeof useAPIModule.useComputeConfig>
}

function reposResult(
  repos: string[] = [],
  opts: { isSuccess?: boolean; isLoading?: boolean; error?: unknown } = {},
) {
  return {
    data: { repos: repos.map((fullName) => ({ fullName })), templates: [] },
    isSuccess: opts.isSuccess ?? true,
    isLoading: opts.isLoading ?? false,
    error: opts.error ?? null,
  } as unknown as ReturnType<typeof useAPIModule.useGitHubRepos>
}

function existsResult(
  opts: { exists?: boolean; isSuccess?: boolean; isFetching?: boolean } = {},
) {
  return {
    data: opts.exists === undefined ? undefined : { exists: opts.exists },
    isSuccess: opts.isSuccess ?? false,
    isFetching: opts.isFetching ?? false,
    error: null,
  } as unknown as ReturnType<typeof useAPIModule.useGitHubRepoExists>
}

beforeEach(() => {
  vi.mocked(useAPIModule.useComputeConfig).mockReturnValue(configResult())
  vi.mocked(useAPIModule.useGitHubRepos).mockReturnValue(reposResult())
  vi.mocked(useAPIModule.useGitHubRepoExists).mockReturnValue(existsResult())
})

describe('IdentitySection', () => {
  it('shows the existing-repo input only when mode === "existing"', async () => {
    const user = userEvent.setup()
    const set = vi.fn()
    const { rerender } = renderWithQuery(
      <IdentitySection state={initialWizardState([])} set={set} />,
    )
    expect(screen.queryByLabelText(/^repository$/i)).not.toBeInTheDocument()

    await user.selectOptions(screen.getByLabelText(/identity repo/i), 'existing')
    expect(set).toHaveBeenCalledWith('identityRepoMode', 'existing')

    rerender(
      <QueryClientProvider client={makeQueryClient()}>
        <IdentitySection
          state={{ ...initialWizardState([]), identityRepoMode: 'existing' }}
          set={set}
        />
      </QueryClientProvider>,
    )
    expect(screen.getByLabelText(/^repository$/i)).toBeInTheDocument()
  })

  it('renders the computed <agent>-agent template-name preview when mode === "template" and name set', () => {
    renderWithQuery(
      <IdentitySection
        state={{ ...initialWizardState([]), name: 'alice', identityRepoMode: 'template' }}
        set={vi.fn()}
      />,
    )
    expect(screen.getByText(/matty-v\/alice-agent/)).toBeInTheDocument()
  })

  it('shows nothing extra when mode === "none"', () => {
    renderWithQuery(
      <IdentitySection
        state={{ ...initialWizardState([]), identityRepoMode: 'none' }}
        set={vi.fn()}
      />,
    )
    expect(screen.queryByLabelText(/^repository$/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/will create/i)).not.toBeInTheDocument()
  })

  // ---- new behavior for #134 ----

  it('renders the existing-mode dropdown populated from useGitHubRepos', () => {
    vi.mocked(useAPIModule.useGitHubRepos).mockReturnValue(
      reposResult(['matty-v/alice-agent', 'matty-v/han-agent']),
    )
    renderWithQuery(
      <IdentitySection
        state={{ ...initialWizardState([]), identityRepoMode: 'existing' }}
        set={vi.fn()}
      />,
    )
    const select = screen.getByLabelText(/^repository$/i) as HTMLSelectElement
    const labels = Array.from(select.options).map((o) => o.textContent ?? '')
    expect(labels).toContain('matty-v/alice-agent')
    expect(labels).toContain('matty-v/han-agent')
    expect(labels.some((l) => /Other/i.test(l))).toBe(true)
  })

  it('selecting a repo updates state.identityRepoExisting via the setter', async () => {
    const user = userEvent.setup()
    const set = vi.fn()
    vi.mocked(useAPIModule.useGitHubRepos).mockReturnValue(
      reposResult(['matty-v/alice-agent', 'matty-v/han-agent']),
    )
    renderWithQuery(
      <IdentitySection
        state={{ ...initialWizardState([]), identityRepoMode: 'existing' }}
        set={set}
      />,
    )
    await user.selectOptions(screen.getByLabelText(/^repository$/i), 'matty-v/alice-agent')
    expect(set).toHaveBeenCalledWith('identityRepoExisting', 'matty-v/alice-agent')
  })

  it('falls back to free-text when the repos list is empty', () => {
    vi.mocked(useAPIModule.useGitHubRepos).mockReturnValue(reposResult([]))
    renderWithQuery(
      <IdentitySection
        state={{ ...initialWizardState([]), identityRepoMode: 'existing' }}
        set={vi.fn()}
      />,
    )
    expect(screen.getByPlaceholderText(/owner\/repo/i)).toBeInTheDocument()
  })

  it('falls back to free-text on a fetch error', () => {
    vi.mocked(useAPIModule.useGitHubRepos).mockReturnValue(
      reposResult([], { isSuccess: false, error: new Error('upstream') }),
    )
    renderWithQuery(
      <IdentitySection
        state={{ ...initialWizardState([]), identityRepoMode: 'existing' }}
        set={vi.fn()}
      />,
    )
    expect(screen.getByPlaceholderText(/owner\/repo/i)).toBeInTheDocument()
  })

  it('renders the available badge in template mode when /exists returns false', () => {
    vi.mocked(useAPIModule.useGitHubRepoExists).mockReturnValue(
      existsResult({ exists: false, isSuccess: true }),
    )
    renderWithQuery(
      <IdentitySection
        state={{ ...initialWizardState([]), name: 'alice', identityRepoMode: 'template' }}
        set={vi.fn()}
      />,
    )
    expect(screen.getByTestId('identity-collision-available')).toBeInTheDocument()
  })

  it('renders the taken badge in template mode when /exists returns true', () => {
    vi.mocked(useAPIModule.useGitHubRepoExists).mockReturnValue(
      existsResult({ exists: true, isSuccess: true }),
    )
    renderWithQuery(
      <IdentitySection
        state={{ ...initialWizardState([]), name: 'alice', identityRepoMode: 'template' }}
        set={vi.fn()}
      />,
    )
    expect(screen.getByTestId('identity-collision-taken')).toBeInTheDocument()
  })

  it('mirrors the collision result into wizard state via the setter', () => {
    const set = vi.fn()
    vi.mocked(useAPIModule.useGitHubRepoExists).mockReturnValue(
      existsResult({ exists: true, isSuccess: true }),
    )
    renderWithQuery(
      <IdentitySection
        state={{ ...initialWizardState([]), name: 'alice', identityRepoMode: 'template' }}
        set={set}
      />,
    )
    expect(set).toHaveBeenCalledWith('identityRepoCollision', true)
  })
})
