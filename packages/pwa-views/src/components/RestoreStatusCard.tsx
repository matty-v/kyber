import { Card } from './Card'
import { Button } from './Button'
import { useAgentImports, useCancelAgentImport } from '../hooks/useAPI'
import { formatArchiveBytes } from './DiskExportCard'
import type { ArchivesCapability } from '../lib/types'

// RestoreStatusCard shows the restore that created this agent from a disk
// archive (MAT-88): progress while it runs, and afterwards the cutover
// checklist of things Kyber deliberately did not copy or activate.
export function RestoreStatusCard({ agentName, capability }: { agentName: string; capability?: ArchivesCapability }) {
  const imports = useAgentImports(agentName, capability?.available === true)
  const cancel = useCancelAgentImport()
  const job = imports.data?.[0]
  if (!job) return null
  const running = job.state === 'queued' || job.state === 'running'
  return (
    <Card>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-sm font-semibold text-text-primary">Restored from a disk archive</h2>
          <p className="mt-1 text-xs text-text-muted">
            {job.summary ? `From ${job.summary.source.agent} · ${job.summary.totals.files.toLocaleString()} files · ${formatArchiveBytes(job.summary.totals.bytes)}` : 'Disk archive'}
          </p>
        </div>
        {job.cancelable && (
          <Button size="sm" variant="ghost" loading={cancel.isPending} onClick={() => cancel.mutate(job.id)}>
            Cancel restore
          </Button>
        )}
      </div>
      <p className={`mt-3 text-sm ${job.state === 'failed' ? 'text-danger' : 'text-text-primary'}`} data-testid="restore-state">
        {job.message ?? job.state}
        {running && job.bytesDone > 0 ? ` · ${formatArchiveBytes(job.bytesDone)}` : ''}
      </p>
      {job.error && <p className="mt-1 text-xs text-danger">{job.error}</p>}
      {running && !job.restored && (
        <p className="mt-1 text-xs text-text-muted">Canceling deletes this agent and its new disk. The source agent is never touched.</p>
      )}
      {(job.cutover?.length ?? 0) > 0 && (
        <div className="mt-3" data-testid="restore-cutover">
          <h3 className="mb-1 text-xs font-medium text-text-primary">Before this agent takes over</h3>
          <ul className="list-disc space-y-1 pl-5 text-xs text-text-secondary">
            {job.cutover!.map((item) => <li key={item}>{item}</li>)}
          </ul>
        </div>
      )}
    </Card>
  )
}
