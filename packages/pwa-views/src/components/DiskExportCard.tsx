import { useState } from 'react'
import { Card } from './Card'
import { Button } from './Button'
import { ConfirmDialog } from './ConfirmDialog'
import { useAgentExports, useCancelAgentExport, useDeleteArchive, useExportDownloadLink, useStartAgentExport } from '../hooks/useAPI'
import type { ArchiveJob, ArchivesCapability } from '../lib/types'

// Phases an export can start from. Mirrors exportablePhases in
// pkg/api/archives.go; transient phases are refused server-side too.
const EXPORTABLE_PHASES = ['Running', 'Stopped', 'Failed', 'NeedsAuth', 'BrokenRuntime', 'DiskExhausted', 'MemoryExhausted']

export function formatArchiveBytes(value: number): string {
  if (value >= 1024 ** 3) return `${(value / 1024 ** 3).toFixed(1)} GiB`
  if (value >= 1024 ** 2) return `${(value / 1024 ** 2).toFixed(1)} MiB`
  return `${Math.max(0, Math.round(value / 1024))} KiB`
}

function stepLabel(job: ArchiveJob): string {
  if (job.message) return job.message
  return job.state
}

function isActive(job: ArchiveJob) {
  return job.state === 'queued' || job.state === 'running'
}

// DiskExportCard starts and tracks exports of an agent's persistent disk
// (MAT-87). It deliberately names what the archive does NOT hold: Secrets and
// platform-managed mounts are listed as excluded, never as backed up.
export function DiskExportCard({ agentName, phase, capability }: { agentName: string; phase: string; capability?: ArchivesCapability }) {
  const available = capability?.available === true
  const exports = useAgentExports(agentName, available)
  const start = useStartAgentExport()
  const cancel = useCancelAgentExport()
  const link = useExportDownloadLink()
  const remove = useDeleteArchive()
  const [confirming, setConfirming] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<ArchiveJob | null>(null)
  const [showDetails, setShowDetails] = useState<string | null>(null)

  const jobs = exports.data ?? []
  const active = jobs.find(isActive)
  const canStart = available && !active && EXPORTABLE_PHASES.includes(phase)

  async function download(job: ArchiveJob) {
    const l = await link.mutateAsync({ name: agentName, id: job.id })
    // A plain navigation streams the archive to disk; fetch would buffer
    // the whole thing in memory first.
    const a = document.createElement('a')
    a.href = l.url
    a.download = l.filename
    a.rel = 'noopener noreferrer'
    document.body.appendChild(a)
    a.click()
    a.remove()
  }

  return (
    <Card>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-sm font-semibold text-text-primary">Disk export</h2>
          <p className="mt-1 max-w-prose text-xs text-text-muted">
            Download the agent's persistent disk as a portable ZIP with a manifest of every file. A running agent
            is stopped while its disk is read and started again afterwards. Kubernetes Secrets are not included.
          </p>
        </div>
        <Button size="sm" variant="secondary" disabled={!canStart} loading={start.isPending} onClick={() => setConfirming(true)}>
          Export disk
        </Button>
      </div>
      {!available && (
        <p className="mt-3 text-xs text-text-muted" data-testid="export-unavailable">
          {capability?.unavailableReason ?? 'Disk export is not available on this installation.'}
        </p>
      )}
      {jobs.length > 0 && (
        <ul className="mt-4 divide-y divide-border-subtle border-t border-border-subtle text-sm">
          {jobs.slice(0, 5).map((job) => (
            <li key={job.id} className="py-2" data-testid={`export-${job.id}`}>
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="min-w-0">
                  <span className="font-medium text-text-primary">{stepLabel(job)}</span>
                  <span className="ml-2 text-xs text-text-muted">{new Date(job.createdAt).toLocaleString()}</span>
                  {isActive(job) && job.bytesDone > 0 && (
                    <span className="ml-2 text-xs text-text-muted">{formatArchiveBytes(job.bytesDone)} read</span>
                  )}
                  {job.state === 'completed' && job.sizeBytes !== undefined && (
                    <span className="ml-2 text-xs text-text-muted">{formatArchiveBytes(job.sizeBytes)}</span>
                  )}
                </div>
                <div className="flex gap-2">
                  {job.downloadable && (
                    <Button size="sm" variant="primary" loading={link.isPending} onClick={() => void download(job)}>Download</Button>
                  )}
                  {job.summary && (
                    <Button size="sm" variant="ghost" onClick={() => setShowDetails(showDetails === job.id ? null : job.id)}>
                      {showDetails === job.id ? 'Hide contents' : 'Contents'}
                    </Button>
                  )}
                  {job.state === 'completed' && (
                    <Button size="sm" variant="ghost" onClick={() => setPendingDelete(job)}>Delete</Button>
                  )}
                  {job.cancelable && (
                    <Button size="sm" variant="ghost" loading={cancel.isPending} onClick={() => cancel.mutate({ name: agentName, id: job.id })}>Cancel</Button>
                  )}
                </div>
              </div>
              {job.error && <p className="mt-1 text-xs text-danger">{job.error}</p>}
              {job.state === 'completed' && job.expiresAt && (
                <p className="mt-1 text-xs text-text-muted">Available until {new Date(job.expiresAt).toLocaleString()}</p>
              )}
              {showDetails === job.id && job.summary && <ExportContents job={job} />}
            </li>
          ))}
        </ul>
      )}
      <ConfirmDialog
        open={confirming}
        title={`Export ${agentName}'s disk?`}
        message={
          <div className="space-y-2">
            <p>The agent is stopped while its disk is archived, then returned to its previous state. Its current session ends.</p>
            <p>The archive can contain local credentials and private data. Anyone with the download can read them.</p>
          </div>
        }
        confirmLabel="Export"
        loading={start.isPending}
        onConfirm={() => {
          setConfirming(false)
          start.mutate(agentName)
        }}
        onCancel={() => setConfirming(false)}
      />
      <ConfirmDialog
        open={pendingDelete !== null}
        title="Delete this export?"
        message="The archive is deleted now rather than when its retention ends. Download links stop working, and it can no longer be imported. An import that is reading it must finish first."
        confirmLabel="Delete export"
        dangerous
        loading={remove.isPending}
        onConfirm={() => {
          const job = pendingDelete!
          remove.mutate({ agent: agentName, id: job.id }, { onSettled: () => setPendingDelete(null) })
        }}
        onCancel={() => setPendingDelete(null)}
      />
    </Card>
  )
}

function ExportContents({ job }: { job: ArchiveJob }) {
  const s = job.summary!
  return (
    <div className="mt-3 space-y-3 rounded-lg bg-surface-overlay p-3 text-xs" data-testid="export-contents">
      <p className="text-text-primary">
        {s.totals.files.toLocaleString()} files, {s.totals.dirs.toLocaleString()} folders, {s.totals.symlinks.toLocaleString()} links,{' '}
        {formatArchiveBytes(s.totals.bytes)} · format {s.formatVersion}
      </p>
      <div>
        <h3 className="mb-1 font-medium text-text-primary">Mounts</h3>
        <ul className="space-y-1">
          {s.mounts.map((m) => (
            <li key={`${m.path}-${m.kind}`}>
              <span className={m.archived ? 'text-success' : 'text-text-muted'}>{m.archived ? 'Included' : 'Not included'}</span>{' '}
              <span className="font-mono text-text-primary">{m.path}</span> <span className="text-text-muted">— {m.reason}</span>
            </li>
          ))}
        </ul>
      </div>
      {(s.excluded?.length ?? 0) > 0 && (
        <div>
          <h3 className="mb-1 font-medium text-text-primary">Left out</h3>
          <ul className="space-y-1">
            {s.excluded!.slice(0, 20).map((x) => (
              <li key={x.path}><span className="font-mono text-text-primary">{x.path}</span> <span className="text-text-muted">— {x.reason}</span></li>
            ))}
          </ul>
        </div>
      )}
      {(s.login?.length ?? 0) > 0 && (
        <div>
          <h3 className="mb-1 font-medium text-text-primary">Harness login (never restored)</h3>
          <ul className="space-y-1 font-mono text-text-primary">
            {s.login!.slice(0, 20).map((p) => <li key={p}>{p}</li>)}
          </ul>
        </div>
      )}
      {(s.sensitive?.length ?? 0) > 0 && (
        <div>
          <h3 className="mb-1 font-medium text-warn">Looks like credentials</h3>
          <ul className="space-y-1 font-mono text-text-primary">
            {s.sensitive!.slice(0, 20).map((p) => <li key={p}>{p}</li>)}
          </ul>
        </div>
      )}
    </div>
  )
}
