// SkillsTab tests. The data hook is mocked so these assert what the operator
// sees, not the network.

import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

vi.mock('../hooks/useAPI', () => ({
  useAgentSkills: vi.fn(),
}))

import * as useAPIModule from '../hooks/useAPI'
import { SkillsTab } from './SkillsTab'
import type { AgentSkill, AgentSkills } from '../lib/types'

function renderWithQuery(ui: React.ReactElement) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>)
}

function mockSkills(data: AgentSkills | null, overrides: Partial<{ isLoading: boolean; error: unknown }> = {}) {
  vi.mocked(useAPIModule.useAgentSkills).mockReturnValue({
    data,
    isLoading: overrides.isLoading ?? false,
    error: overrides.error ?? null,
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof useAPIModule.useAgentSkills>)
}

function skill(over: Partial<AgentSkill> = {}): AgentSkill {
  return {
    name: 'restart',
    description: 'Planned shutdown: save, commit, push.',
    source: 'identity',
    path: 'skills/restart',
    linked: ['claude-code', 'codex'],
    ...over,
  }
}

function report(over: Partial<AgentSkills> = {}): AgentSkills {
  const skills = over.skills ?? [skill()]
  const issues = over.issues ?? []
  const broken = skills.filter((s) => (s.issues ?? []).some((i) => i.severity === 'error')).length
  const warnings = skills.filter(
    (s) => (s.issues ?? []).length > 0 && !(s.issues ?? []).some((i) => i.severity === 'error'),
  ).length
  return {
    agent: 'dave',
    reportedAt: '2026-08-24T12:00:00Z',
    skills,
    issues,
    summary: {
      total: skills.length,
      broken,
      warnings,
      healthy: skills.length - broken - warnings,
      otherIssues: issues.length,
    },
    ...over,
  }
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe('SkillsTab', () => {
  it('lists a healthy skill with its description, origin, and runtimes', () => {
    mockSkills(report())
    renderWithQuery(<SkillsTab agentName="dave" />)

    expect(screen.getByText('restart')).toBeInTheDocument()
    expect(screen.getByText('Planned shutdown: save, commit, push.')).toBeInTheDocument()
    expect(screen.getByText('Own')).toBeInTheDocument()
    expect(screen.getByText(/Loadable in Claude Code, Codex/)).toBeInTheDocument()
    expect(screen.getByLabelText('Loadable')).toBeInTheDocument()
  })

  // The whole point of the tab: a skill that is committed and present but
  // loadable by nothing must read as broken, not as fine.
  it('flags a skill that exists in the repo but loads nowhere', () => {
    mockSkills(
      report({
        skills: [
          skill({
            linked: [],
            issues: [
              {
                code: 'not_linked',
                severity: 'error',
                detail: 'not loadable by claude-code — no link at ~/.claude/skills/restart',
              },
            ],
          }),
        ],
      }),
    )
    renderWithQuery(<SkillsTab agentName="dave" />)

    expect(screen.getByText('Not loadable by any runtime')).toBeInTheDocument()
    expect(screen.getByText('not_linked')).toBeInTheDocument()
    expect(screen.getByText(/no link at/)).toBeInTheDocument()
    expect(screen.getByText('1 broken')).toBeInTheDocument()
    expect(screen.getByLabelText('Broken')).toBeInTheDocument()
  })

  it('names the vendor package a skill came from', () => {
    mockSkills(
      report({ skills: [skill({ name: 'triage', source: 'vendor', sourcePackage: 'falcon-dev-common' })] }),
    )
    renderWithQuery(<SkillsTab agentName="dave" />)
    expect(screen.getByText('Vendored · falcon-dev-common')).toBeInTheDocument()
  })

  it('shows image-bundled skills as platform skills', () => {
    mockSkills(
      report({
        skills: [skill({ name: 'telegram-messaging', source: 'platform', linked: ['claude-code'] })],
      }),
    )
    renderWithQuery(<SkillsTab agentName="dave" />)
    expect(screen.getByText('Platform')).toBeInTheDocument()
    expect(screen.getByText(/Loadable in Claude Code/)).toBeInTheDocument()
  })

  // State written straight into a runtime skills home works right now and is
  // committed nowhere, so it dies at the next reprovision. That needs to be
  // visible, not silent.
  it('surfaces skill state that lives outside the identity repo', () => {
    mockSkills(
      report({
        issues: [
          {
            code: 'unmanaged',
            severity: 'warning',
            detail: '~/.claude/skills/handwritten is a real directory, not a link into the identity repo',
          },
        ],
      }),
    )
    renderWithQuery(<SkillsTab agentName="dave" />)

    expect(screen.getByText('Skill state outside the identity repo')).toBeInTheDocument()
    expect(screen.getByText('unmanaged')).toBeInTheDocument()
    expect(screen.getByText(/disappear when the pod is/)).toBeInTheDocument()
  })

  // "Never reported" and "has no skills" are different facts and must not
  // render the same — one points at a stale pod, the other at an empty repo.
  it('distinguishes never-reported from no-skills', () => {
    mockSkills(null)
    const { unmount } = renderWithQuery(<SkillsTab agentName="dave" />)
    expect(screen.getByText('No report yet')).toBeInTheDocument()
    unmount()

    mockSkills(report({ skills: [] }))
    renderWithQuery(<SkillsTab agentName="dave" />)
    expect(screen.getByText('No skills')).toBeInTheDocument()
  })

  // A missing description is worth saying and is not a failure. Rendering it
  // as loudly as "no runtime can load this" trains the operator to ignore both.
  it('separates a warning from a broken skill', () => {
    mockSkills(
      report({
        skills: [
          skill({
            issues: [{ code: 'missing_description', severity: 'warning', detail: 'frontmatter has no description' }],
          }),
        ],
      }),
    )
    renderWithQuery(<SkillsTab agentName="dave" />)

    expect(screen.getByText('1 with warnings')).toBeInTheDocument()
    expect(screen.queryByText(/broken/)).not.toBeInTheDocument()
    expect(screen.getByLabelText('Has warnings')).toBeInTheDocument()
  })

  it('renders the report timestamp so a stale tab is legible', () => {
    mockSkills(report())
    renderWithQuery(<SkillsTab agentName="dave" />)
    expect(screen.getByText(/^As of /)).toBeInTheDocument()
  })

  it('offers a retry when the fetch fails', () => {
    mockSkills(null, { error: new Error('boom') })
    renderWithQuery(<SkillsTab agentName="dave" />)
    expect(screen.getByText(/Failed to load skills: boom/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument()
  })

  // Read-only by design: skills are managed by talking to the agent. A write
  // control here would let an operator desync the repo from the pod.
  it('exposes no controls that add, edit, or remove a skill', () => {
    mockSkills(report())
    renderWithQuery(<SkillsTab agentName="dave" />)
    const buttons = screen.queryAllByRole('button')
    expect(buttons).toHaveLength(0)
  })
})
