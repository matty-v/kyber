// Jobs tab for AgentDetail — list, create, edit, delete, and run-now
// scheduled prompts declared on the Agent CR's spec.jobs. See issue #135
// for protocol and reference implementation context.

import { useState } from 'react'
import { Clock, Play, Pencil, Plus, Trash2 } from 'lucide-react'
import { useAgent } from '../hooks/useAPI'
import { usePatchAgentJobs, useRunAgentJob } from '../hooks/useAPI'
import type { AgentJob, AgentJobRun } from '../lib/types'
import { humanizeCron } from '../lib/cron'
import { Button } from './Button'
import { Card } from './Card'
import { ConfirmDialog } from './ConfirmDialog'
import { EmptyState } from './EmptyState'

// 5-field cron pattern for client-side sanity check before submit. Server
// re-validates; this just catches typos early.
const FIVE_FIELD = /^\s*\S+\s+\S+\s+\S+\s+\S+\s+\S+\s*$/

// Job name regex mirrors the CRD validator — DNS-1123 lowercase.
const JOB_NAME_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/

interface Props {
  agentName: string
}

type DraftJob = {
  name: string
  schedule: string
  prompt: string
  exclusive: boolean
  clearContextAfter: boolean
}

function emptyDraft(): DraftJob {
  return { name: '', schedule: '0 9 * * *', prompt: '', exclusive: false, clearContextAfter: false }
}

function formatTimestamp(iso: string | undefined): string {
  if (!iso) return '—'
  try {
    return new Date(iso).toLocaleString(undefined, { dateStyle: 'short', timeStyle: 'short' })
  } catch {
    return iso
  }
}

function runByName(runs: AgentJobRun[] | undefined, name: string): AgentJobRun | undefined {
  return runs?.find((r) => r.jobName === name)
}

// outcomeBadge returns a small coloured pill matching the StatusBadge style.
function OutcomeBadge({ run }: { run: AgentJobRun | undefined }) {
  if (!run) {
    return <span className="text-text-secondary">—</span>
  }
  const cls =
    run.outcome === 'success'
      ? 'text-success bg-success-muted'
      : run.outcome === 'failed'
        ? 'text-danger bg-danger-muted'
        : 'text-text-secondary bg-surface-subtle'
  const label =
    run.outcome === 'success' ? '✓ success' : run.outcome === 'failed' ? '✗ failed' : '⤴ skipped'
  const tooltip = run.error ? `${run.error}` : undefined
  return (
    <span className={`inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-xs ${cls}`} title={tooltip}>
      {label}
    </span>
  )
}

function truncate(s: string, n: number): string {
  if (s.length <= n) return s
  return s.slice(0, n - 1) + '…'
}

// JobCard renders a single scheduled prompt as a stacked card for the
// mobile (<md) viewport (#243). Top: humanized schedule + exclusive
// pill, with the raw cron in a small mono subline so operators still
// have the canonical form. Body: prompt with line-clamp-3 + tap to
// expand. Footer: last-run badge on the left, full-size action
// buttons on the right.
function JobCard({
  job,
  lastRun,
  runDisabled,
  onRun,
  onEdit,
  onDelete,
}: {
  job: AgentJob
  lastRun: AgentJobRun | undefined
  runDisabled: boolean
  onRun: () => void
  onEdit: () => void
  onDelete: () => void
}) {
  const [expanded, setExpanded] = useState(false)
  const human = humanizeCron(job.schedule)
  return (
    <Card className="space-y-3">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2 flex-wrap">
            <span className="font-mono text-sm text-text-primary truncate">{job.name}</span>
            {job.exclusive ? (
              <span className="rounded bg-surface-subtle px-1 text-[10px] uppercase text-text-secondary">
                exclusive
              </span>
            ) : null}
            {job.clearContextAfter ? (
              <span className="rounded bg-surface-subtle px-1 text-[10px] uppercase text-text-secondary">
                clears context
              </span>
            ) : null}
          </div>
          <p className="mt-0.5 text-xs text-text-secondary">
            {human ?? <span className="font-mono">{job.schedule}</span>}
          </p>
          {human && (
            <p className="text-[11px] font-mono text-text-disabled">{job.schedule}</p>
          )}
        </div>
      </div>

      <button
        type="button"
        onClick={() => setExpanded((v) => !v)}
        aria-expanded={expanded}
        className="block w-full text-left text-sm text-text-primary"
      >
        <span className={expanded ? '' : 'line-clamp-3'}>{job.prompt}</span>
      </button>

      <div className="flex items-center justify-between gap-2">
        <div className="text-xs">
          <div className="text-text-muted">{formatTimestamp(lastRun?.startedAt)}</div>
          <OutcomeBadge run={lastRun} />
        </div>
        <div className="flex items-center gap-2">
          <Button
            variant="secondary"
            size="sm"
            onClick={onRun}
            disabled={runDisabled}
            aria-label="Run now"
          >
            <Play className="h-4 w-4" />
          </Button>
          <Button variant="secondary" size="sm" onClick={onEdit} aria-label="Edit">
            <Pencil className="h-4 w-4" />
          </Button>
          <Button variant="secondary" size="sm" onClick={onDelete} aria-label="Delete">
            <Trash2 className="h-4 w-4 text-danger" />
          </Button>
        </div>
      </div>
    </Card>
  )
}

export function JobsTab({ agentName }: Props) {
  const { data: agent, isLoading, error } = useAgent(agentName)
  const patchJobs = usePatchAgentJobs()
  const runJob = useRunAgentJob()

  const jobs: AgentJob[] = agent?.jobs ?? []
  const runs: AgentJobRun[] | undefined = agent?.lastJobRuns

  const [editing, setEditing] = useState<{ mode: 'create' } | { mode: 'edit'; index: number } | null>(null)
  const [draft, setDraft] = useState<DraftJob>(emptyDraft())
  const [formError, setFormError] = useState<string | null>(null)
  const [pendingDelete, setPendingDelete] = useState<number | null>(null)

  function openCreate() {
    setDraft(emptyDraft())
    setFormError(null)
    setEditing({ mode: 'create' })
  }

  function openEdit(index: number) {
    const j = jobs[index]
    setDraft({
      name: j.name,
      schedule: j.schedule,
      prompt: j.prompt,
      exclusive: !!j.exclusive,
      clearContextAfter: !!j.clearContextAfter,
    })
    setFormError(null)
    setEditing({ mode: 'edit', index })
  }

  function closeEditor() {
    setEditing(null)
    setDraft(emptyDraft())
    setFormError(null)
  }

  function validate(d: DraftJob, editingIndex: number | null): string | null {
    if (!JOB_NAME_RE.test(d.name)) {
      return 'Name must be lowercase DNS-1123 (start/end alphanumeric, hyphens in middle)'
    }
    if (!FIVE_FIELD.test(d.schedule)) {
      return 'Schedule must be a 5-field cron expression (min hr dom mon dow)'
    }
    if (d.prompt.trim() === '') {
      return 'Prompt is required'
    }
    const duplicate = jobs.findIndex((j, i) => j.name === d.name && i !== editingIndex)
    if (duplicate !== -1) {
      return `Name "${d.name}" already exists on this agent`
    }
    return null
  }

  async function saveDraft() {
    if (!editing) return
    const editingIndex = editing.mode === 'edit' ? editing.index : null
    const err = validate(draft, editingIndex)
    if (err) {
      setFormError(err)
      return
    }
    const next = [...jobs]
    const rendered: AgentJob = {
      name: draft.name,
      schedule: draft.schedule,
      prompt: draft.prompt,
      exclusive: draft.exclusive || undefined,
      clearContextAfter: draft.clearContextAfter || undefined,
    }
    if (editing.mode === 'create') {
      next.push(rendered)
    } else {
      next[editing.index] = rendered
    }
    try {
      await patchJobs.mutateAsync({ name: agentName, jobs: next })
      closeEditor()
    } catch (e) {
      setFormError(e instanceof Error ? e.message : 'Update failed')
    }
  }

  async function confirmDelete() {
    if (pendingDelete === null) return
    const next = jobs.filter((_, i) => i !== pendingDelete)
    await patchJobs.mutateAsync({ name: agentName, jobs: next })
    setPendingDelete(null)
  }

  return (
    <div className="space-y-3">
      {isLoading && (
        <Card className="text-sm text-text-muted">Loading jobs…</Card>
      )}

      {error && (
        <Card className="border-danger/40 bg-danger-muted text-sm text-danger">
          Failed to load jobs: {error instanceof Error ? error.message : 'Unknown error'}
        </Card>
      )}

      {!isLoading && !error && jobs.length === 0 && (
        <EmptyState
          icon={<Clock className="h-6 w-6" strokeWidth={1.5} />}
          title="No scheduled prompts"
          description="Add a job to deliver a prompt into this agent on a cron schedule."
          action={
            <Button variant="primary" size="sm" onClick={openCreate}>
              <Plus className="h-3.5 w-3.5" /> Add job
            </Button>
          }
        />
      )}

      {!isLoading && jobs.length > 0 && (
        <>
        <div className="flex items-center justify-between">
          <Button variant="primary" size="sm" onClick={openCreate}>
            <Plus className="h-3.5 w-3.5" /> Add job
          </Button>
        </div>

        {/*
         * Mobile (<md): card-per-job (#243). The raw <table> overflows
         * the 390px viewport, word-stacks long prompts, and shrinks the
         * action buttons below tap-target size. AgentList uses the same
         * dual-view pattern.
         */}
        <div className="space-y-3 md:hidden">
          {jobs.map((j, i) => {
            const last = runByName(runs, j.name)
            return (
              <JobCard
                key={j.name}
                job={j}
                lastRun={last}
                runDisabled={runJob.isPending}
                onRun={() => runJob.mutate({ name: agentName, jobName: j.name })}
                onEdit={() => openEdit(i)}
                onDelete={() => setPendingDelete(i)}
              />
            )
          })}
        </div>

        {/* Desktop (md+): keep the existing table. */}
        <Card className="hidden md:block">
          <table className="w-full text-sm">
            <thead className="border-b border-border text-left text-xs uppercase text-text-secondary">
              <tr>
                <th className="p-3">Name</th>
                <th className="p-3">Schedule</th>
                <th className="p-3">Prompt</th>
                <th className="p-3">Last run</th>
                <th className="p-3 text-right">Actions</th>
              </tr>
            </thead>
            <tbody>
              {jobs.map((j, i) => {
                const last = runByName(runs, j.name)
                const human = humanizeCron(j.schedule)
                return (
                  <tr key={j.name} className="border-b border-border last:border-b-0">
                    <td className="p-3 font-mono">{j.name}</td>
                    <td className="p-3 text-xs" title={j.schedule}>
                      <div>{human ?? <span className="font-mono">{j.schedule}</span>}</div>
                      {human && (
                        <div className="font-mono text-[11px] text-text-disabled">
                          {j.schedule}
                        </div>
                      )}
                      {j.exclusive ? (
                        <span className="mt-1 inline-block rounded bg-surface-subtle px-1 text-[10px] uppercase">
                          exclusive
                        </span>
                      ) : null}
                      {j.clearContextAfter ? (
                        <span className="mt-1 ml-1 inline-block rounded bg-surface-subtle px-1 text-[10px] uppercase">
                          clears context
                        </span>
                      ) : null}
                    </td>
                    <td className="p-3 text-text-secondary" title={j.prompt}>
                      {truncate(j.prompt.replace(/\n/g, ' '), 60)}
                    </td>
                    <td className="p-3 text-xs">
                      <div>{formatTimestamp(last?.startedAt)}</div>
                      <OutcomeBadge run={last} />
                    </td>
                    <td className="p-3">
                      <div className="flex items-center justify-end gap-1">
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => runJob.mutate({ name: agentName, jobName: j.name })}
                          disabled={runJob.isPending}
                          title="Run now"
                        >
                          <Play className="h-4 w-4" />
                        </Button>
                        <Button variant="ghost" size="sm" onClick={() => openEdit(i)} title="Edit">
                          <Pencil className="h-4 w-4" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => setPendingDelete(i)}
                          title="Delete"
                        >
                          <Trash2 className="h-4 w-4 text-danger" />
                        </Button>
                      </div>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </Card>
        </>
      )}

      {editing !== null ? (
        <Card className="space-y-3 p-4">
          <h4 className="text-sm font-medium">
            {editing.mode === 'create' ? 'New scheduled prompt' : 'Edit scheduled prompt'}
          </h4>
          <div className="grid gap-3 sm:grid-cols-2">
            <label className="text-xs text-text-secondary">
              Name
              <input
                className="mt-1 w-full rounded border border-border bg-surface px-2 py-1 font-mono text-sm"
                value={draft.name}
                onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                placeholder="morning-pr-triage"
                disabled={editing.mode === 'edit'}
                autoFocus={editing.mode === 'create'}
              />
            </label>
            <label className="text-xs text-text-secondary">
              Schedule (5-field cron)
              <input
                className="mt-1 w-full rounded border border-border bg-surface px-2 py-1 font-mono text-sm"
                value={draft.schedule}
                onChange={(e) => setDraft({ ...draft, schedule: e.target.value })}
                placeholder="0 9 * * *"
              />
            </label>
          </div>
          <label className="block text-xs text-text-secondary">
            Prompt
            <textarea
              className="mt-1 h-32 w-full resize-y rounded border border-border bg-surface px-2 py-1 text-sm"
              value={draft.prompt}
              onChange={(e) => setDraft({ ...draft, prompt: e.target.value })}
              placeholder="Check overnight PRs and summarize the changes."
            />
          </label>
          <label className="flex items-center gap-2 text-xs text-text-secondary">
            <input
              type="checkbox"
              checked={draft.exclusive}
              onChange={(e) => setDraft({ ...draft, exclusive: e.target.checked })}
            />
            <span>
              Exclusive — skip the next fire while the previous run of this job is still being
              worked.
            </span>
          </label>
          <label className="flex items-center gap-2 text-xs text-text-secondary">
            <input
              type="checkbox"
              checked={draft.clearContextAfter}
              onChange={(e) => setDraft({ ...draft, clearContextAfter: e.target.checked })}
            />
            <span>
              Clear context after — start each fire from a clean conversation.
            </span>
          </label>
          {formError ? <div className="text-xs text-danger">{formError}</div> : null}
          <div className="flex items-center justify-end gap-2">
            <Button variant="ghost" size="sm" onClick={closeEditor} disabled={patchJobs.isPending}>
              Cancel
            </Button>
            <Button onClick={saveDraft} disabled={patchJobs.isPending} size="sm">
              {patchJobs.isPending ? 'Saving…' : 'Save'}
            </Button>
          </div>
        </Card>
      ) : null}

      <ConfirmDialog
        open={pendingDelete !== null}
        title="Delete scheduled prompt?"
        message={
          pendingDelete !== null
            ? `"${jobs[pendingDelete]?.name}" will stop firing within ~90s of this change propagating to the pod.`
            : ''
        }
        confirmLabel="Delete"
        dangerous
        onConfirm={confirmDelete}
        onCancel={() => setPendingDelete(null)}
      />
    </div>
  )
}
