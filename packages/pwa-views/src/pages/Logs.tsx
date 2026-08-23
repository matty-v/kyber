import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { Download, FileText } from 'lucide-react'
import { useQuery } from '@tanstack/react-query'
import { Button } from '../components/Button'
import { Card } from '../components/Card'
import { EmptyState } from '../components/EmptyState'
import { createApiClient } from '../lib/api'
import { useCluster } from '../lib/cluster-context'
import { usePrefixedPath } from '../lib/route-prefix'
import type { LoggingExportOptions, LoggingTarget } from '../lib/types'

type Source = 'kubelet' | 'archive'
const MAX_LINES = 5000

function localValue(date: Date): string {
  const shifted = new Date(date.getTime() - date.getTimezoneOffset() * 60_000)
  return shifted.toISOString().slice(0, 16)
}

function Select({ label, value, onChange, children }: React.PropsWithChildren<{
  label: string
  value: string
  onChange: (value: string) => void
}>) {
  return <label className="flex min-w-36 flex-1 flex-col gap-1 text-xs text-text-muted">
    {label}
    <select value={value} onChange={(e) => onChange(e.target.value)} className="rounded-md border border-border-subtle bg-surface-base px-2 py-2 font-mono text-xs text-text-primary">
      {children}
    </select>
  </label>
}

export function Logs() {
  const cluster = useCluster()
  const prefixed = usePrefixedPath()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const [params, setParams] = useSearchParams()
  const settings = useQuery({ queryKey: ['cluster', cluster.id, 'logging-settings'], queryFn: api.getLoggingSettings })
  const targets = useQuery({ queryKey: ['cluster', cluster.id, 'logging-targets'], queryFn: api.getLoggingTargets, refetchInterval: 30_000 })
  const allTargets = targets.data?.targets ?? []
  const resourceTargets = allTargets.filter((t) =>
    (!params.get('agent') || t.agent === params.get('agent')) &&
    (!params.get('machine') || t.machine === params.get('machine')),
  )
  const visibleTargets = resourceTargets.length ? resourceTargets : allTargets
  const components = [...new Set(visibleTargets.map((t) => t.component))].sort()
  const [component, setComponent] = useState(params.get('component') ?? '')
  const componentTargets = visibleTargets.filter((t) => !component || t.component === component)
  const workloads = [...new Set(componentTargets.map((t) => t.workload))].sort()
  const [workload, setWorkload] = useState(params.get('workload') ?? '')
  const workloadTargets = componentTargets.filter((t) => !workload || t.workload === workload)
  const [pod, setPod] = useState(params.get('pod') ?? '')
  const target = workloadTargets.find((t) => t.pod === pod) ?? workloadTargets[0]
  const [container, setContainer] = useState(params.get('container') ?? '')
  const selectedContainer = target?.containers.find((c) => c.name === container) ?? target?.containers[0]
  const [source, setSource] = useState<Source>('kubelet')
  const [follow, setFollow] = useState(true)
  const [paused, setPaused] = useState(false)
  const [since, setSince] = useState(localValue(new Date(Date.now() - 60 * 60_000)))
  const [until, setUntil] = useState(localValue(new Date()))
  const [loadKey, setLoadKey] = useState(0)
  const [lines, setLines] = useState<string[]>([])
  const [error, setError] = useState<string | null>(null)
  const [truncated, setTruncated] = useState(false)
  const [exporting, setExporting] = useState(false)
  const boxRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!target || !selectedContainer) return
    const next = Object.fromEntries(params.entries())
    setParams({ ...next, component: target.component, workload: target.workload, pod: target.pod, container: selectedContainer.name }, { replace: true })
  }, [target?.podUid, selectedContainer?.name])

  useEffect(() => {
    if (!target || !selectedContainer) return
    setLines([]); setError(null); setTruncated(false); setPaused(false)
    const archive = source === 'archive'
    let reader: ReadableStreamDefaultReader<string> | undefined
    void (async () => {
      try {
        const result = await api.loggingStream({
      pod: target.pod, podUid: target.podUid, container: selectedContainer.name,
      component: target.component, workload: target.workload,
      source, follow: !archive && follow, tail: archive ? undefined : 500,
      since: archive ? new Date(since).toISOString() : undefined,
      until: archive ? new Date(until).toISOString() : undefined,
        })
        setTruncated(result.truncated)
        reader = result.stream.getReader()
        let buffer = ''
        while (true) {
          const { done, value } = await reader.read(); if (done) break
          buffer += value
          const chunks = buffer.split('\n'); buffer = chunks.pop() ?? ''
          if (chunks.length) setLines((old) => [...old, ...chunks].slice(-MAX_LINES))
        }
        if (buffer) setLines((old) => [...old, buffer].slice(-MAX_LINES))
      } catch (e) { if ((e as Error).name !== 'AbortError') setError((e as Error).message) }
    })()
    return () => { void reader?.cancel() }
  }, [api, target?.podUid, selectedContainer?.name, source, follow, loadKey])

  useEffect(() => {
    if (!paused && boxRef.current) boxRef.current.scrollTop = boxRef.current.scrollHeight
  }, [lines, paused])

  function chooseComponent(value: string) { setComponent(value); setWorkload(''); setPod(''); setContainer('') }
  function chooseWorkload(value: string) { setWorkload(value); setPod(''); setContainer('') }
  function choosePod(value: string) { setPod(value); setContainer('') }

  async function download(format: 'ndjson' | 'text') {
    if (!target || !selectedContainer) return
    setExporting(true); setError(null)
    try {
      const opts: LoggingExportOptions = {
        pod: target.pod, podUid: target.podUid, container: selectedContainer.name, format,
        component: target.component, workload: target.workload,
        since: new Date(since).toISOString(), until: new Date(until).toISOString(),
      }
      const result = await api.exportLogging(opts)
      setTruncated(result.truncated)
      const href = URL.createObjectURL(result.blob)
      const link = document.createElement('a'); link.href = href; link.download = result.filename; link.click()
      URL.revokeObjectURL(href)
    } catch (e) { setError((e as Error).message) } finally { setExporting(false) }
  }

  if (targets.isLoading) return <p className="text-sm text-text-muted">Discovering log targets…</p>
  if (targets.isError) return <EmptyState icon={<FileText className="h-7 w-7" />} title="Log discovery failed" description={(targets.error as Error).message} />
  if (!allTargets.length) return <EmptyState icon={<FileText className="h-7 w-7" />} title="No log targets" description="No Kyber-managed pods are currently discoverable." />

  const resource = target?.agent
    ? { label: `Agent ${target.agent}`, href: prefixed(`/agents/${target.agent}`) }
    : target?.machine ? { label: `Machine ${target.machine}`, href: prefixed(`/machines/${target.machine}`) } : null

  return <div className="space-y-5">
    <div><h2 className="font-display text-2xl font-semibold">Fleet Logs</h2><p className="mt-1 text-sm text-text-muted">Live pod output and durable archive for every Kyber workload.</p></div>
    <Card className="space-y-4">
      <div className="flex flex-wrap gap-3">
        <Select label="Component" value={target?.component ?? ''} onChange={chooseComponent}>{components.map((v) => <option key={v}>{v}</option>)}</Select>
        <Select label="Workload" value={target?.workload ?? ''} onChange={chooseWorkload}>{workloads.map((v) => <option key={v}>{v}</option>)}</Select>
        <Select label="Pod" value={target?.pod ?? ''} onChange={choosePod}>{workloadTargets.map((v) => <option key={v.pod}>{v.pod}</option>)}</Select>
        <Select label="Container" value={selectedContainer?.name ?? ''} onChange={setContainer}>{target?.containers.map((v) => <option key={v.name} value={v.name}>{v.name}{v.init ? ' (init)' : ''}</option>)}</Select>
      </div>
      <div className="flex flex-wrap items-center gap-x-5 gap-y-2 border-t border-border-subtle pt-3 text-xs text-text-muted">
        <span>Effective verbosity: <strong className="font-mono text-text-primary">{selectedContainer?.effectiveLevel}</strong></span>
        <span>Archive retention: <strong className="font-mono text-text-primary">{settings.data?.archiveRetentionDays ?? '…'} days</strong></span>
        <span>Managed by: <strong className="font-mono text-text-primary">{settings.data?.managedBy ?? '…'}</strong></span>
        {resource && <Link className="ml-auto text-accent hover:underline" to={resource.href}>{resource.label} →</Link>}
      </div>
    </Card>
    <Card className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <Button size="sm" variant={source === 'kubelet' ? 'primary' : 'secondary'} onClick={() => setSource('kubelet')} disabled={!target?.liveAvailable}>Live</Button>
        <Button size="sm" variant={source === 'archive' ? 'primary' : 'secondary'} onClick={() => setSource('archive')} disabled={!target?.archiveAvailable}>Archive</Button>
        {source === 'kubelet' && <><label className="ml-2 flex items-center gap-2 text-xs"><input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> Follow</label><Button size="sm" variant="ghost" onClick={() => setPaused((v) => !v)}>{paused ? 'Resume scroll' : 'Pause scroll'}</Button></>}
        <span className="ml-auto text-xs text-text-muted">{lines.length} lines</span>
      </div>
      <div className="flex flex-wrap items-end gap-2">
        <label className="text-xs text-text-muted">From<input aria-label="From" type="datetime-local" value={since} onChange={(e) => setSince(e.target.value)} className="ml-2 rounded border border-border-subtle bg-surface-base p-1.5 font-mono" /></label>
        <label className="text-xs text-text-muted">To<input aria-label="To" type="datetime-local" value={until} onChange={(e) => setUntil(e.target.value)} className="ml-2 rounded border border-border-subtle bg-surface-base p-1.5 font-mono" /></label>
        {source === 'archive' && <Button size="sm" variant="secondary" onClick={() => setLoadKey((v) => v + 1)}>Load window</Button>}
        <Button size="sm" variant="ghost" disabled={exporting} onClick={() => void download('ndjson')}><Download className="h-4 w-4" /> NDJSON</Button>
        <Button size="sm" variant="ghost" disabled={exporting} onClick={() => void download('text')}><Download className="h-4 w-4" /> Text</Button>
      </div>
      {(truncated || lines.length === MAX_LINES) && <p role="status" className="rounded bg-warning/10 px-3 py-2 text-xs text-warning">Output was truncated to protect the control plane and browser.</p>}
      {error && <p role="alert" className="rounded bg-danger/10 px-3 py-2 text-xs text-danger">{error}</p>}
      <div ref={boxRef} className="h-[28rem] overflow-auto rounded-lg border border-border-subtle bg-surface-sunken p-3 font-mono text-xs leading-5 text-text-secondary">
        {!lines.length && !error ? <p className="text-text-disabled">Awaiting telemetry…</p> : lines.map((line, i) => <div key={i} className="whitespace-pre-wrap break-all">{line}</div>)}
      </div>
    </Card>
  </div>
}
