import { useEffect, useMemo, useRef, useState } from 'react'
import { createApiClient } from '../lib/api'
import { useCluster } from '../lib/cluster-context'
import { Button } from './Button'
import type { LogStreamOptions } from '../lib/types'

interface Props {
  kind: 'agent' | 'machine'
  name: string
  follow?: boolean
  tail?: number
}

interface LogLine {
  id: number
  text: string
}

type LogSource = 'live' | 'archive'

interface AppliedWindow {
  since: string
  until: string
}

const MAX_LINES = 2000

// toRFC3339 converts a <input type="datetime-local"> value (local wall-clock,
// no zone) to an absolute RFC3339 UTC instant the archive endpoint accepts.
// Returns null for an empty/unparseable value.
function toRFC3339(local: string): string | null {
  if (!local) return null
  const d = new Date(local)
  if (Number.isNaN(d.getTime())) return null
  return d.toISOString()
}

export function LogViewer({ kind, name, follow = true, tail = 200 }: Props) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const [lines, setLines] = useState<LogLine[]>([])
  const [error, setError] = useState<string | null>(null)
  const [paused, setPaused] = useState(false)
  const [source, setSource] = useState<LogSource>('live')
  const [from, setFrom] = useState('')
  const [to, setTo] = useState('')
  const [windowError, setWindowError] = useState<string | null>(null)
  // appliedWindow is the window actually being read — set on "Load", so editing
  // the pickers doesn't refetch until the operator commits.
  const [appliedWindow, setAppliedWindow] = useState<AppliedWindow | null>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const autoScrollRef = useRef(true)
  const streamRef = useRef<ReadableStream<string> | null>(null)
  const readerRef = useRef<ReadableStreamDefaultReader<string> | null>(null)
  const nextIdRef = useRef(0)

  // Archive reads are bounded (no follow); only the live source streams.
  const isArchive = source === 'archive'

  useEffect(() => {
    // Archive mode with no committed window: nothing to read yet — show the
    // picker prompt rather than opening a stream.
    if (isArchive && !appliedWindow) {
      setLines([])
      setError(null)
      return
    }

    setLines([])
    setError(null)
    setPaused(false)
    autoScrollRef.current = true
    nextIdRef.current = 0

    const opts: LogStreamOptions = isArchive
      ? { source: 'archive', since: appliedWindow!.since, until: appliedWindow!.until }
      : { follow, tail }

    const stream = api.logStream(kind, name, opts)
    streamRef.current = stream
    const reader = stream.getReader()
    readerRef.current = reader

    let buffer = ''

    async function consume() {
      try {
        while (true) {
          const { done, value } = await reader.read()
          if (done) break
          buffer += value
          const split = buffer.split('\n')
          buffer = split.pop() ?? ''
          if (split.length > 0) {
            const start = nextIdRef.current
            const added: LogLine[] = split.map((text, i) => ({
              id: start + i,
              text,
            }))
            nextIdRef.current = start + split.length
            setLines((prev) => [...prev, ...added].slice(-MAX_LINES))
          }
        }
        // Flush any trailing partial line (archive responses don't always end
        // in a newline).
        if (buffer.length > 0) {
          const id = nextIdRef.current
          nextIdRef.current = id + 1
          setLines((prev) => [...prev, { id, text: buffer }].slice(-MAX_LINES))
        }
      } catch (err) {
        if (err instanceof Error && err.name !== 'AbortError') {
          setError(err.message)
        }
      }
    }

    void consume()

    return () => {
      reader.cancel().catch(() => undefined)
    }
  }, [kind, name, follow, tail, api, isArchive, appliedWindow])

  // Auto-scroll to bottom when new lines arrive and not paused.
  useEffect(() => {
    if (autoScrollRef.current && !paused && containerRef.current) {
      containerRef.current.scrollTop = containerRef.current.scrollHeight
    }
  }, [lines, paused])

  function handleScroll() {
    if (!containerRef.current) return
    const { scrollTop, scrollHeight, clientHeight } = containerRef.current
    const atBottom = scrollHeight - scrollTop - clientHeight < 40
    autoScrollRef.current = atBottom
  }

  function togglePause() {
    setPaused((p) => {
      const next = !p
      autoScrollRef.current = !next
      return next
    })
  }

  function loadArchiveWindow() {
    const since = toRFC3339(from)
    const until = toRFC3339(to)
    if (!since || !until) {
      setWindowError('Pick both a "from" and "to" time.')
      return
    }
    if (new Date(until).getTime() < new Date(since).getTime()) {
      setWindowError('"To" must not be earlier than "from".')
      return
    }
    setWindowError(null)
    setAppliedWindow({ since, until })
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center justify-between gap-2">
        {/* Source toggle: live pod stdout vs durable archive window (#431). */}
        <div className="inline-flex overflow-hidden rounded-md border border-border-subtle text-xs">
          <button
            type="button"
            onClick={() => setSource('live')}
            className={`px-2 py-1 ${source === 'live' ? 'bg-accent/20 text-text-primary' : 'text-text-muted'}`}
          >
            Live
          </button>
          <button
            type="button"
            onClick={() => setSource('archive')}
            className={`px-2 py-1 ${source === 'archive' ? 'bg-accent/20 text-text-primary' : 'text-text-muted'}`}
          >
            Archive
          </button>
        </div>
        <span className="text-xs text-text-muted">{lines.length} lines</span>
        {!isArchive && (
          <Button variant="ghost" size="sm" onClick={togglePause}>
            {paused ? 'Resume scroll' : 'Pause scroll'}
          </Button>
        )}
      </div>

      {isArchive && (
        <div className="flex flex-wrap items-end gap-2 rounded-md border border-border-subtle bg-surface-sunken p-2">
          <label className="flex flex-col gap-1 text-xs text-text-muted">
            From
            <input
              type="datetime-local"
              value={from}
              onChange={(e) => setFrom(e.target.value)}
              className="rounded border border-border-subtle bg-surface-base px-2 py-1 font-mono text-xs text-text-secondary"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs text-text-muted">
            To
            <input
              type="datetime-local"
              value={to}
              onChange={(e) => setTo(e.target.value)}
              className="rounded border border-border-subtle bg-surface-base px-2 py-1 font-mono text-xs text-text-secondary"
            />
          </label>
          <Button variant="secondary" size="sm" onClick={loadArchiveWindow}>
            Load
          </Button>
          {windowError && <span className="text-xs text-danger">{windowError}</span>}
        </div>
      )}

      <div
        ref={containerRef}
        onScroll={handleScroll}
        className="h-80 overflow-y-auto rounded-lg border border-border-subtle bg-surface-sunken p-3 font-mono text-xs text-text-secondary leading-5"
      >
        {error && <p className="text-danger">Error: {error}</p>}
        {lines.length === 0 && !error && isArchive && !appliedWindow && (
          <div className="flex h-full items-center justify-center text-text-disabled">
            <span className="font-mono text-xs tracking-[0.05em]">
              Pick a time range and press Load to read archived logs.
            </span>
          </div>
        )}
        {lines.length === 0 && !error && !(isArchive && !appliedWindow) && (
          <div className="flex h-full items-center justify-center">
            <div className="flex items-center gap-2 text-text-disabled">
              <span
                className="h-1.5 w-1.5 rounded-full bg-accent/70"
                style={{ animation: 'kyber-pulse-ring 1.8s ease-in-out infinite' }}
              />
              <span className="font-mono text-xs tracking-[0.05em]">
                {isArchive ? 'No lines in this window.' : 'Awaiting telemetry…'}
              </span>
            </div>
          </div>
        )}
        {lines.map((line) => (
          <div key={line.id} className="whitespace-pre-wrap break-all">
            {line.text}
          </div>
        ))}
      </div>
    </div>
  )
}
