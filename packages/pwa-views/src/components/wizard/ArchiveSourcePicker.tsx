import { useEffect, useState } from 'react'
import { Upload } from 'lucide-react'
import { useArchives, useArchiveUpload, useUploadArchive } from '../../hooks/useAPI'
import { formatArchiveBytes } from '../DiskExportCard'
import type { ArchiveJob, ArchivesCapability } from '../../lib/types'
import type { WizardSetter, WizardState } from './types'

// requiredDiskGi mirrors requiredDiskBytes in pkg/api/archives_import.go: the
// archived bytes plus 10% and 1 GiB, rounded up to whole GiB.
export function requiredDiskGi(bytes: number): number {
  return Math.ceil((bytes + bytes / 10 + 1024 ** 3) / 1024 ** 3)
}

function archiveLabel(job: ArchiveJob): string {
  const when = new Date(job.createdAt).toLocaleString()
  const size = job.sizeBytes !== undefined ? ` · ${formatArchiveBytes(job.sizeBytes)}` : ''
  return job.kind === 'upload' ? `Uploaded archive · ${when}${size}` : `${job.agent} · exported ${when}${size}`
}

function diskGi(disk: string): number {
  const m = /^(\d+(?:\.\d+)?)\s*(Gi|G|Ti|T)?$/.exec(disk.trim())
  if (!m) return 0
  const n = Number(m[1])
  return m[2]?.startsWith('T') ? n * 1024 : n
}

// ArchiveSourcePicker is the Create Agent wizard's "start from" choice
// (MAT-88): an empty disk, or a completed export or uploaded archive whose
// disk the new agent is restored from.
export function ArchiveSourcePicker({ state, set, capability }: { state: WizardState; set: WizardSetter; capability?: ArchivesCapability }) {
  const available = capability?.available === true
  const fromArchive = state.archiveSource !== undefined
  const archives = useArchives(available && fromArchive)
  const upload = useUploadArchive()
  const [uploadId, setUploadId] = useState<string>()
  const [progress, setProgress] = useState<number | null>(null)
  const uploaded = useArchiveUpload(uploadId)

  function choose(job: ArchiveJob | undefined) {
    if (!job) {
      set('archiveSource', { label: '' })
      return
    }
    set('archiveSource', {
      exportId: job.kind === 'export' ? job.id : undefined,
      uploadId: job.kind === 'upload' ? job.id : undefined,
      label: archiveLabel(job),
      summary: job.summary,
    })
    if (job.summary?.source.runtime && state.runtimes?.some((r) => r.id === job.summary!.source.runtime)) {
      set('runtime', job.summary.source.runtime)
    }
    if (job.summary) {
      const need = requiredDiskGi(job.summary.totals.bytes)
      if (diskGi(state.disk) < need) set('disk', `${need}Gi`)
    }
  }

  // Select an upload as soon as it has been verified.
  useEffect(() => {
    const job = uploaded.data
    if (job?.state === 'completed' && state.archiveSource?.uploadId !== job.id) choose(job)
    // choose is recreated every render; the upload's state is the trigger.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [uploaded.data?.state, uploaded.data?.id])

  if (!available) return null
  const selectedId = state.archiveSource?.exportId ?? state.archiveSource?.uploadId ?? ''
  const summary = state.archiveSource?.summary

  return (
    <fieldset className="space-y-2" data-testid="archive-source">
      <legend className="mb-1 block text-sm font-medium text-text-primary">Start from</legend>
      <label className="flex items-center gap-2 text-sm text-text-primary">
        <input type="radio" name="start-from" checked={!fromArchive} onChange={() => set('archiveSource', undefined)} />
        An empty disk
      </label>
      <label className="flex items-center gap-2 text-sm text-text-primary">
        <input type="radio" name="start-from" checked={fromArchive} onChange={() => choose(undefined)} />
        A disk archive
      </label>
      {fromArchive && (
        <div className="space-y-3 rounded-lg border border-border-subtle p-3">
          <select
            aria-label="Archive"
            className="w-full rounded-lg border border-border-default bg-surface-base px-3 py-2 text-sm text-text-primary"
            value={selectedId}
            onChange={(e) => choose((archives.data ?? []).find((j) => j.id === e.target.value))}
          >
            <option value="">{archives.isLoading ? 'Loading archives…' : 'Choose an export or upload'}</option>
            {(archives.data ?? []).map((j) => (
              <option key={j.id} value={j.id}>{archiveLabel(j)}</option>
            ))}
          </select>
          <label className="flex cursor-pointer items-center gap-2 rounded-lg border border-dashed border-border-default px-3 py-2 text-xs text-text-secondary">
            <Upload className="h-4 w-4" />
            {progress !== null ? `Uploading… ${progress}%` : uploaded.data && uploaded.data.state !== 'completed' ? (uploaded.data.error ?? uploaded.data.message ?? 'Verifying…') : 'Upload a Kyber disk archive (.zip)'}
            <input
              type="file"
              accept=".zip,application/zip"
              className="hidden"
              disabled={upload.isPending}
              onChange={(e) => {
                const file = e.target.files?.[0]
                if (!file) return
                setProgress(0)
                upload.mutate(
                  { file, onProgress: (loaded, total) => setProgress(total ? Math.round((loaded / total) * 100) : 0) },
                  { onSuccess: (job) => setUploadId(job.id), onSettled: () => setProgress(null) },
                )
              }}
            />
          </label>
          {summary && (
            <div className="space-y-1 text-xs text-text-muted" data-testid="archive-summary">
              <p className="text-text-primary">
                From {summary.source.agent} ({summary.source.runtime}{summary.source.runtimeVersion ? ` ${summary.source.runtimeVersion}` : ''}) ·{' '}
                {summary.totals.files.toLocaleString()} files · {formatArchiveBytes(summary.totals.bytes)}
              </p>
              <p>The new agent needs a disk of at least {requiredDiskGi(summary.totals.bytes)} GiB. It starts only after the restore has been checked against the archive.</p>
              {summary.source.runtime && summary.source.runtime !== state.runtime && (
                <p className="text-warn">The archive came from a {summary.source.runtime} agent. Its sessions will not resume under {state.runtime}.</p>
              )}
              <label className="flex items-start gap-2 pt-1 text-text-primary">
                <input
                  type="checkbox"
                  className="mt-0.5"
                  checked={state.keepCredentialFiles}
                  onChange={(e) => set('keepCredentialFiles', e.target.checked)}
                />
                <span>
                  Also restore files that look like credentials{summary.sensitive?.length ? ` (${summary.sensitive.length})` : ''}
                  <span className="block text-text-muted">
                    Off by default: the new agent gets its own logins and keys, and two agents sharing one login sign each other out.
                  </span>
                </span>
              </label>
              {(summary.cron?.length ?? 0) > 0 && (
                <label className="flex items-start gap-2 pt-1 text-text-primary">
                  <input
                    type="checkbox"
                    className="mt-0.5"
                    checked={state.keepCrontabs}
                    onChange={(e) => set('keepCrontabs', e.target.checked)}
                  />
                  <span>
                    Also restore the {summary.cron!.length} crontab(s) {summary.source.agent} installed itself
                    <span className="block text-text-muted">Off by default, so the same scheduled work does not run on both agents.</span>
                  </span>
                </label>
              )}
            </div>
          )}
        </div>
      )}
    </fieldset>
  )
}
