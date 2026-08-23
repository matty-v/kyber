import { useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Download, FileText } from 'lucide-react'
import { useQuery } from '@tanstack/react-query'
import { Button } from '../components/Button'
import { Card } from '../components/Card'
import { EmptyState } from '../components/EmptyState'
import { createApiClient } from '../lib/api'
import { useCluster } from '../lib/cluster-context'
import type { LoggingReadOptions, LoggingTarget } from '../lib/types'

type Source = 'kubelet' | 'archive'
const MAX_LINES = 5000
const MAX_BUFFER_BYTES = 8 << 20
const LIVE_REFRESH_MS = 5000
const READ_CONCURRENCY = 2

interface Selection {
  target: LoggingTarget
  container: LoggingTarget['containers'][number]
}

export function filterLogSelections(targets: LoggingTarget[], agent: string, component: string, source: Source): Selection[] {
  return targets.flatMap((target) => {
    if (agent && target.agent !== agent) return []
    return target.containers
      .filter((container) => !component || (container.component || target.component) === component)
      .filter(() => source === 'kubelet' ? target.liveAvailable : target.archiveAvailable)
      .map((container) => ({ target, container }))
  })
}

export function logSelectionKey(selections: Selection[], source: Source): string {
  const sources = selections.map(({ target, container }) => `${target.podUid}/${container.name}`).sort()
  return `${source}:${sources.join(',')}`
}

export function isAtLogTail(scrollTop: number, scrollHeight: number, clientHeight: number): boolean {
  return scrollHeight - scrollTop - clientHeight < 40
}

export interface DisplayLogLine {
  id: string
  timestamp: string
  timestampMs: number | null
  level: string
  agent: string
  component: string
  source: string
  message: string
  raw: string
}

function compareLogLines(a: DisplayLogLine, b: DisplayLogLine): number {
  if (a.timestampMs === null && b.timestampMs === null) return a.source.localeCompare(b.source)
  if (a.timestampMs === null) return 1
  if (b.timestampMs === null) return -1
  return a.timestampMs - b.timestampMs
}

export class BoundedLogBuffer {
  private lines: DisplayLogLine[] = []
  private bytes = 0
  truncated = false

  add(batch: DisplayLogLine[]) {
    this.lines.push(...batch)
    this.bytes += batch.reduce((total, line) => total + line.raw.length * 2, 0)
    if (this.lines.length <= MAX_LINES && this.bytes <= MAX_BUFFER_BYTES) return
    this.lines.sort(compareLogLines)
    while (this.lines.length > MAX_LINES || this.bytes > MAX_BUFFER_BYTES) {
      const removed = this.lines.shift()
      if (!removed) break
      this.bytes -= removed.raw.length * 2
      this.truncated = true
    }
  }

  snapshot(): DisplayLogLine[] {
    return [...this.lines].sort(compareLogLines)
  }
}

function localValue(date: Date): string {
  const shifted = new Date(date.getTime() - date.getTimezoneOffset() * 60_000)
  return shifted.toISOString().slice(0, 16)
}

function stringField(value: unknown): string {
  if (typeof value === 'string') return value
  if (value === undefined || value === null) return ''
  return JSON.stringify(value)
}

export function formatLogLine(raw: string, selection: Selection, index: number): DisplayLogLine {
  let timestamp = ''
  let level = ''
  let message = raw
  try {
    const parsed = JSON.parse(raw) as Record<string, unknown>
    timestamp = stringField(parsed.time ?? parsed.timestamp)
    level = stringField(parsed.level ?? parsed.severity).toLowerCase()
    message = stringField(parsed.msg ?? parsed.message) || raw
  } catch {
    const match = raw.match(/^(\d{4}-\d\d-\d\dT\S+)\s+(?:(debug|info|warn|warning|error|fatal)\s+)?(.*)$/i)
    if (match) {
      timestamp = match[1]
      level = (match[2] ?? '').toLowerCase()
      message = match[3]
    }
  }
  const timestampMs = timestamp ? Date.parse(timestamp) : Number.NaN
  return {
    id: `${selection.target.podUid}:${selection.container.name}:${index}:${raw}`,
    timestamp,
    timestampMs: Number.isNaN(timestampMs) ? null : timestampMs,
    level,
    agent: selection.target.agent ?? 'platform',
    component: selection.container.component || selection.target.component,
    source: `${selection.target.pod}/${selection.container.name}`,
    message,
    raw,
  }
}

function Select({ label, value, onChange, children }: React.PropsWithChildren<{
  label: string
  value: string
  onChange: (value: string) => void
}>) {
  return <label className="flex min-w-44 flex-1 flex-col gap-1 text-xs text-text-muted">
    {label}
    <select aria-label={label} value={value} onChange={(event) => onChange(event.target.value)} className="rounded-md border border-border-subtle bg-surface-base px-2 py-2 font-mono text-xs text-text-primary">
      {children}
    </select>
  </label>
}

async function readTextStream(
  stream: ReadableStream<string>,
  activeReaders: Set<ReadableStreamDefaultReader<string>>,
  onLines: (lines: string[]) => void,
): Promise<void> {
  const reader = stream.getReader()
  activeReaders.add(reader)
  let buffer = ''
  try {
    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      buffer += value
      const chunks = buffer.split('\n')
      buffer = chunks.pop() ?? ''
      const complete = chunks.filter(Boolean)
      if (complete.length) onLines(complete)
    }
    if (buffer) onLines([buffer])
  } finally {
    activeReaders.delete(reader)
    reader.releaseLock()
  }
}

function levelClass(level: string): string {
  if (level === 'error' || level === 'fatal') return 'text-danger'
  if (level === 'warn' || level === 'warning') return 'text-warning'
  if (level === 'debug') return 'text-text-disabled'
  return 'text-accent'
}

export function Logs() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const [params, setParams] = useSearchParams()
  const machine = params.get('machine') ?? ''
  const settings = useQuery({ queryKey: ['cluster', cluster.id, 'logging-settings'], queryFn: api.getLoggingSettings })
  const targets = useQuery({ queryKey: ['cluster', cluster.id, 'logging-targets'], queryFn: api.getLoggingTargets, refetchInterval: 30_000 })
  const allTargets = useMemo(() => targets.data?.targets ?? [], [targets.data?.targets])
  const agents = [...new Set(allTargets.flatMap((target) => target.agent ? [target.agent] : []))].sort()
  const components = [...new Set(allTargets.flatMap((target) => target.containers.map((container) => container.component || target.component)))].sort()
  const agent = params.get('agent') ?? ''
  const component = params.get('component') ?? ''
  const [source, setSource] = useState<Source>('kubelet')
  const [paused, setPaused] = useState(false)
  const [since, setSince] = useState(localValue(new Date(Date.now() - 60 * 60_000)))
  const [until, setUntil] = useState(localValue(new Date()))
  const [loadKey, setLoadKey] = useState(0)
  const [refreshKey, setRefreshKey] = useState(0)
  const [lines, setLines] = useState<DisplayLogLine[]>([])
  const [errors, setErrors] = useState<string[]>([])
  const [truncated, setTruncated] = useState(false)
  const [loading, setLoading] = useState(false)
  const boxRef = useRef<HTMLDivElement>(null)
  const autoScrollRef = useRef(true)
  const selectionKeyRef = useRef('')

  const selections = useMemo(
    () => filterLogSelections(allTargets.filter((target) => !machine || target.machine === machine), agent, component, source),
    [allTargets, machine, agent, component, source],
  )

  function setFilter(name: 'agent' | 'component', value: string) {
    const next = Object.fromEntries(params.entries())
    if (value) next[name] = value; else delete next[name]
    setParams(next, { replace: true })
  }

  useEffect(() => {
    if (source !== 'kubelet' || paused || loading) return
    const timer = window.setTimeout(() => setRefreshKey((value) => value + 1), LIVE_REFRESH_MS)
    return () => window.clearTimeout(timer)
  }, [source, paused, loading])

  useEffect(() => {
    if (!selections.length) {
      selectionKeyRef.current = ''
      autoScrollRef.current = true
      setLines([]); setErrors([]); setLoading(false)
      return
    }
    let cancelled = false
    const selectionKey = logSelectionKey(selections, source)
    const selectionChanged = selectionKeyRef.current !== selectionKey
    selectionKeyRef.current = selectionKey
    setLoading(true); setErrors([]); setTruncated(false)
    if (selectionChanged) {
      autoScrollRef.current = true
      setLines([])
    }
    const buffer = new BoundedLogBuffer()
    const activeReaders = new Set<ReadableStreamDefaultReader<string>>()
    const failures: string[] = []
    let wasTruncated = false
    let nextIndex = 0

    async function worker() {
      while (!cancelled) {
        const index = nextIndex++
        if (index >= selections.length) return
        const selection = selections[index]
        const archive = source === 'archive'
        const opts: LoggingReadOptions = {
          pod: selection.target.pod,
          podUid: selection.target.podUid,
          container: selection.container.name,
          component: selection.target.component,
          workload: selection.target.workload,
          source,
          follow: false,
          tail: archive ? undefined : 200,
          since: archive ? new Date(since).toISOString() : undefined,
          until: archive ? new Date(until).toISOString() : undefined,
        }
        try {
          const result = await api.loggingStream(opts)
          wasTruncated ||= result.truncated
          let lineIndex = 0
          await readTextStream(result.stream, activeReaders, (rawLines) => {
            buffer.add(rawLines.map((raw) => formatLogLine(raw, selection, lineIndex++)))
          })
        } catch (error) {
          if (!cancelled && (error as Error).name !== 'AbortError') {
            failures.push(`${selection.target.pod}/${selection.container.name}: ${(error as Error).message}`)
          }
        }
      }
    }

    void Promise.all(Array.from({ length: Math.min(READ_CONCURRENCY, selections.length) }, worker)).then(() => {
      if (cancelled) return
      setLines(buffer.snapshot()); setErrors(failures); setTruncated(wasTruncated || buffer.truncated); setLoading(false)
    })
    return () => {
      cancelled = true
      for (const reader of activeReaders) void reader.cancel()
    }
  }, [api, selections, source, loadKey, refreshKey])

  useEffect(() => {
    if (autoScrollRef.current && boxRef.current) boxRef.current.scrollTop = boxRef.current.scrollHeight
  }, [lines])

  function handleScroll() {
    if (!boxRef.current) return
    const { scrollTop, scrollHeight, clientHeight } = boxRef.current
    autoScrollRef.current = isAtLogTail(scrollTop, scrollHeight, clientHeight)
  }

  function downloadVisible(format: 'ndjson' | 'text') {
    const output = lines.map((line) => format === 'ndjson'
      ? JSON.stringify({ timestamp: line.timestamp || undefined, level: line.level || undefined, agent: line.agent, component: line.component, source: line.source, message: line.message, raw: line.raw })
      : `${line.timestamp || '-'} ${line.level || '-'} ${line.agent} ${line.component} ${line.message}`
    ).join('\n') + '\n'
    const href = URL.createObjectURL(new Blob([output], { type: format === 'ndjson' ? 'application/x-ndjson' : 'text/plain' }))
    const link = document.createElement('a')
    link.href = href; link.download = `kyber-visible-logs.${format === 'ndjson' ? 'ndjson' : 'log'}`; link.click()
    URL.revokeObjectURL(href)
  }

  if (targets.isLoading) return <p className="text-sm text-text-muted">Discovering log targets…</p>
  if (targets.isError) return <EmptyState icon={<FileText className="h-7 w-7" />} title="Log discovery failed" description={(targets.error as Error).message} />
  if (!allTargets.length) return <EmptyState icon={<FileText className="h-7 w-7" />} title="No log targets" description="No Kyber-managed pods are currently discoverable." />

  return <div className="space-y-5">
    <div><h2 className="font-display text-2xl font-semibold">Fleet Logs</h2><p className="mt-1 text-sm text-text-muted">One time-ordered view across every Kyber workload.</p></div>
    <Card className="space-y-4">
      <div className="flex flex-wrap gap-3">
        <Select label="Agent" value={agent} onChange={(value) => setFilter('agent', value)}><option value="">All agents and platform</option>{agents.map((value) => <option key={value}>{value}</option>)}</Select>
        <Select label="Component" value={component} onChange={(value) => setFilter('component', value)}><option value="">All components</option>{components.map((value) => <option key={value}>{value}</option>)}</Select>
      </div>
      <div className="flex flex-wrap items-center gap-x-5 gap-y-2 border-t border-border-subtle pt-3 text-xs text-text-muted">
        <span><strong className="font-mono text-text-primary">{selections.length}</strong> log sources</span>
        <span>Archive retention: <strong className="font-mono text-text-primary">{settings.data?.archiveRetentionDays ?? '…'} days</strong></span>
        <span>Managed by: <strong className="font-mono text-text-primary">{settings.data?.managedBy ?? '…'}</strong></span>
      </div>
    </Card>
    <Card className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <Button size="sm" variant={source === 'kubelet' ? 'primary' : 'secondary'} onClick={() => setSource('kubelet')}>Live</Button>
        <Button size="sm" variant={source === 'archive' ? 'primary' : 'secondary'} onClick={() => setSource('archive')}>Archive</Button>
        {source === 'kubelet' && <Button size="sm" variant="ghost" onClick={() => setPaused((value) => !value)}>{paused ? 'Resume refresh' : 'Pause refresh'}</Button>}
        <Button size="sm" variant="ghost" disabled={!lines.length} onClick={() => downloadVisible('ndjson')}><Download className="h-4 w-4" /> Visible NDJSON</Button>
        <Button size="sm" variant="ghost" disabled={!lines.length} onClick={() => downloadVisible('text')}><Download className="h-4 w-4" /> Visible text</Button>
        <span className="ml-auto text-xs text-text-muted">{loading ? 'Refreshing… · ' : ''}{lines.length} lines</span>
      </div>
      {source === 'archive' && <div className="flex flex-wrap items-end gap-2">
        <label className="text-xs text-text-muted">From<input aria-label="From" type="datetime-local" value={since} onChange={(event) => setSince(event.target.value)} className="ml-2 rounded border border-border-subtle bg-surface-base p-1.5 font-mono" /></label>
        <label className="text-xs text-text-muted">To<input aria-label="To" type="datetime-local" value={until} onChange={(event) => setUntil(event.target.value)} className="ml-2 rounded border border-border-subtle bg-surface-base p-1.5 font-mono" /></label>
        <Button size="sm" variant="secondary" onClick={() => setLoadKey((value) => value + 1)}>Load window</Button>
      </div>}
      {(truncated || lines.length === MAX_LINES) && <p role="status" className="rounded bg-warning/10 px-3 py-2 text-xs text-warning">Output was truncated to protect the control plane and browser.</p>}
      {!!errors.length && <p role="alert" className="rounded bg-danger/10 px-3 py-2 text-xs text-danger">{errors.length} source{errors.length === 1 ? '' : 's'} could not be read. <span className="font-mono">{errors[0]}</span></p>}
      <div ref={boxRef} onScroll={handleScroll} className="h-[32rem] overflow-auto rounded-lg border border-border-subtle bg-surface-sunken font-mono text-xs text-text-secondary">
        {!lines.length && !errors.length ? <p className="p-3 text-text-disabled">{loading ? 'Loading telemetry…' : 'No logs match these filters.'}</p> : lines.map((line) => <div key={line.id} className="grid grid-cols-[8rem_4rem_7rem_10rem_14rem_minmax(18rem,1fr)] gap-2 border-b border-border-subtle/60 px-3 py-1.5 last:border-0">
          <span className="truncate text-text-muted" title={line.timestamp}>{line.timestamp ? new Date(line.timestamp).toLocaleTimeString() : '—'}</span>
          <span className={`truncate uppercase ${levelClass(line.level)}`}>{line.level || '—'}</span>
          <span className="truncate text-text-primary" title={line.agent}>{line.agent}</span>
          <span className="truncate" title={line.component}>{line.component}</span>
          <span className="truncate text-text-muted" title={line.source}>{line.source}</span>
          <details className="min-w-0"><summary className="cursor-pointer whitespace-pre-wrap break-words marker:text-text-disabled">{line.message}</summary><pre className="mt-2 whitespace-pre-wrap break-all rounded bg-surface-base p-2 text-text-muted">{line.raw}</pre></details>
        </div>)}
      </div>
    </Card>
  </div>
}
