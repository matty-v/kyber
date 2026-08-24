// Skills tab for AgentDetail — a read-only view of what this agent can
// actually do.
//
// The list comes from a scan of the agent's own pod: its identity repo, the
// packages vendored into it, and the capability cookbooks the runtime image
// bakes in. It deliberately does NOT come from GitHub. During kyber#691 every
// identity repo on the fleet held a full set of skills and not one of them was
// linked into a path the runtime read; a repo-sourced list would have looked
// perfectly healthy for the entire outage. So this shows what is loadable, and
// says plainly when something is present but dead.
//
// There is no add/edit/remove here on purpose. Skills are managed by talking to
// the agent, which writes them into its identity repo and pushes; an operator
// editing them from here would put the repo and the pod out of sync, which is
// the exact state this tab exists to reveal. The only interactive element is
// the per-skill disclosure.

import { useState } from 'react'
import {
  AlertCircle,
  AlertTriangle,
  BookOpen,
  CheckCircle2,
  ChevronRight,
  Package,
  Puzzle,
} from 'lucide-react'
import { useAgentSkills } from '../hooks/useAPI'
import type { AgentSkill, AgentSkillIssue, AgentSkillSource } from '../lib/types'
import { Button } from './Button'
import { Card } from './Card'
import { EmptyState } from './EmptyState'

interface Props {
  agentName: string
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : 'Unknown error'
}

function formatTimestamp(iso: string | undefined): string {
  if (!iso) return '—'
  try {
    return new Date(iso).toLocaleString(undefined, { dateStyle: 'short', timeStyle: 'short' })
  } catch {
    return iso
  }
}

const SOURCE_LABEL: Record<AgentSkillSource, string> = {
  identity: 'Own',
  vendor: 'Vendored',
  platform: 'Platform',
}

const SOURCE_HINT: Record<AgentSkillSource, string> = {
  identity: "This agent's own skill, in its identity repo",
  vendor: 'Vendored into the identity repo from a shared package',
  platform: 'Built into the Kyber runtime image and enabled by a sidecar',
}

/** Runtime identifier → the name an operator recognises. */
const RUNTIME_LABEL: Record<string, string> = {
  'claude-code': 'Claude Code',
  codex: 'Codex',
}

function SourceBadge({ skill }: { skill: AgentSkill }) {
  const label =
    skill.source === 'vendor' && skill.sourcePackage
      ? `${SOURCE_LABEL.vendor} · ${skill.sourcePackage}`
      : SOURCE_LABEL[skill.source] ?? skill.source
  return (
    <span
      title={SOURCE_HINT[skill.source]}
      className="shrink-0 rounded-full bg-surface px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-text-muted"
    >
      {label}
    </span>
  )
}

/** True when any issue means the skill does not work, as opposed to merely
 *  being imperfect. Drives the row's icon and the summary counts. */
function isBroken(issues: AgentSkillIssue[]): boolean {
  return issues.some((i) => i.severity === 'error')
}

function IssueList({ issues }: { issues: AgentSkillIssue[] }) {
  return (
    <ul className="mt-2 space-y-1">
      {issues.map((issue, i) => {
        const error = issue.severity === 'error'
        return (
          <li
            key={`${issue.code}-${i}`}
            className={`flex gap-2 text-xs ${error ? 'text-danger' : 'text-warn'}`}
          >
            {error ? (
              <AlertCircle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden="true" />
            ) : (
              <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden="true" />
            )}
            <span>
              <span className="font-mono">{issue.code}</span>
              {' — '}
              {issue.detail}
            </span>
          </li>
        )
      })}
    </ul>
  )
}

function SkillRow({ skill }: { skill: AgentSkill }) {
  const issues = skill.issues ?? []
  const broken = isBroken(issues)
  const healthy = issues.length === 0
  const runtimes = skill.linked.map((r) => RUNTIME_LABEL[r] ?? r)

  // A skill with something wrong opens by default. Collapsing is for making a
  // healthy list scannable — it must never be the reason a problem goes unseen,
  // which is the one thing this tab exists to prevent.
  const [open, setOpen] = useState(!healthy)
  const detailId = `skill-detail-${skill.source}-${skill.sourcePackage ?? ''}-${skill.name}`

  return (
    <li className="border-b border-border-subtle last:border-b-0">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        aria-controls={detailId}
        className="flex w-full items-center gap-2 px-3 py-2.5 text-left hover:bg-surface-raised"
      >
        <ChevronRight
          className={`h-3.5 w-3.5 shrink-0 text-text-muted transition-transform ${open ? 'rotate-90' : ''}`}
          aria-hidden="true"
        />
        {broken ? (
          <AlertCircle className="h-4 w-4 shrink-0 text-danger" aria-label="Broken" />
        ) : healthy ? (
          <CheckCircle2 className="h-4 w-4 shrink-0 text-success" aria-label="Loadable" />
        ) : (
          <AlertTriangle className="h-4 w-4 shrink-0 text-warn" aria-label="Has warnings" />
        )}
        <span className="font-mono text-sm text-text-primary">{skill.name}</span>
        <SourceBadge skill={skill} />
        {/* The count rides the collapsed row so a folded-away problem still
            announces itself without the detail being open. */}
        {!healthy && (
          <span
            className={`shrink-0 rounded-full px-2 py-0.5 text-[10px] font-medium ${
              broken ? 'bg-danger-muted text-danger' : 'bg-warn-muted text-warn'
            }`}
          >
            {issues.length} {issues.length === 1 ? 'problem' : 'problems'}
          </span>
        )}
        {!open && skill.description && (
          <span className="min-w-0 flex-1 truncate text-xs text-text-muted">{skill.description}</span>
        )}
      </button>

      {open && (
        <div id={detailId} className="px-3 pb-3 pl-11">
          {skill.description ? (
            <p className="text-xs text-text-secondary">{skill.description}</p>
          ) : (
            <p className="text-xs italic text-text-muted">No description</p>
          )}
          <p className="mt-1 text-[11px] text-text-muted">
            {runtimes.length > 0 ? (
              <>Loadable in {runtimes.join(', ')}</>
            ) : (
              <span className="text-danger">Not loadable by any runtime</span>
            )}
            {' · '}
            <span className="font-mono">{skill.path}</span>
          </p>
          {!healthy && <IssueList issues={issues} />}
        </div>
      )}
    </li>
  )
}

export function SkillsTab({ agentName }: Props) {
  const { data, isLoading, error, refetch } = useAgentSkills(agentName, true)

  if (isLoading) {
    return <Card className="text-sm text-text-muted">Loading skills…</Card>
  }

  if (error) {
    return (
      <Card className="border-danger/40 bg-danger-muted text-sm text-danger">
        Failed to load skills: {errorMessage(error)}
        <div className="mt-2">
          <Button variant="ghost" size="sm" onClick={() => void refetch()}>
            Retry
          </Button>
        </div>
      </Card>
    )
  }

  // null is the 404 case: the agent has never reported. That is a different
  // fact from "reported zero skills", and saying so points at the real cause
  // (the pod has not booted since this shipped) instead of implying the agent
  // has nothing.
  if (!data) {
    return (
      <EmptyState
        icon={<Puzzle className="h-7 w-7" />}
        title="No report yet"
        description="This agent has not reported its skills. Agents report at boot and on every identity sync — restart the session or the pod to collect one."
      />
    )
  }

  const { skills, issues, summary } = data

  return (
    <div className="space-y-3">
      <Card>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div className="flex items-center gap-2">
            <BookOpen className="h-4 w-4 text-text-muted" aria-hidden="true" />
            <span className="text-sm text-text-primary">
              {summary.total} skill{summary.total === 1 ? '' : 's'}
            </span>
            {summary.broken > 0 && (
              <span className="rounded-full bg-danger-muted px-2 py-0.5 text-[11px] font-medium text-danger">
                {summary.broken} broken
              </span>
            )}
            {summary.warnings > 0 && (
              <span className="rounded-full bg-warn-muted px-2 py-0.5 text-[11px] font-medium text-warn">
                {summary.warnings} with warnings
              </span>
            )}
          </div>
          <span className="text-xs text-text-muted">
            As of {formatTimestamp(data.reportedAt)}
          </span>
        </div>
        <p className="mt-2 text-xs text-text-muted">
          Scanned from the agent&apos;s own filesystem, so this is what it can actually
          invoke. Skills are added and removed by asking the agent — it saves them to
          its identity repo, which is the only place they survive a reprovision.
        </p>
      </Card>

      {issues.length > 0 && (
        <Card className="border-warn/40 bg-warn-muted">
          <div className="flex items-center gap-2">
            <Package className="h-4 w-4 text-warn" aria-hidden="true" />
            <h2 className="text-sm font-semibold text-text-primary">
              Skill state outside the identity repo
            </h2>
          </div>
          <p className="mt-1 text-xs text-text-muted">
            These are not committed anywhere, so they disappear when the pod is
            reprovisioned.
          </p>
          <ul className="mt-2 space-y-1">
            {issues.map((issue, i) => (
              <li key={`${issue.code}-${i}`} className="text-xs text-text-secondary">
                <span className="font-mono text-warn">{issue.code}</span>
                {' — '}
                {issue.detail}
              </li>
            ))}
          </ul>
        </Card>
      )}

      {skills.length === 0 ? (
        <EmptyState
          icon={<Puzzle className="h-7 w-7" />}
          title="No skills"
          description="This agent has no skills installed. Ask it to create or download one — it will save it to its identity repo."
        />
      ) : (
        <Card className="p-0">
          <ul>
            {skills.map((skill) => (
              <SkillRow key={`${skill.source}-${skill.sourcePackage ?? ''}-${skill.name}`} skill={skill} />
            ))}
          </ul>
        </Card>
      )}
    </div>
  )
}
