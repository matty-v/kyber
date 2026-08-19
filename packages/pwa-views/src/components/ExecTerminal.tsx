import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { ClipboardAddon } from '@xterm/addon-clipboard'
import { SearchAddon } from '@xterm/addon-search'
import { UnicodeGraphemesAddon } from '@xterm/addon-unicode-graphemes'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { WebglAddon } from '@xterm/addon-webgl'
import { ClipboardCopy, ClipboardPaste, Search, X } from 'lucide-react'
import { createApiClient } from '../lib/api'
import { useCluster } from '../lib/cluster-context'
import { terminalExtraKeys } from './terminal-controls'
import '@xterm/xterm/css/xterm.css'

const encoder = new TextEncoder()

interface Props {
  kind: 'agent' | 'machine'
  name: string
  mode?: 'attach' | 'shell' | 'history' | 'device-auth'
  heightClassName?: string
}

type ConnectionState = 'connecting' | 'connected' | 'reconnecting' | 'closed'

export function ExecTerminal({ kind, name, mode, heightClassName }: Props) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const searchRef = useRef<SearchAddon | null>(null)
  const [connection, setConnection] = useState<ConnectionState>('connecting')
  const [searchOpen, setSearchOpen] = useState(false)
  const [query, setQuery] = useState('')
  const interactive = mode !== 'attach' && mode !== 'history'

  const send = useCallback((data: string) => {
    const ws = wsRef.current
    if (interactive && ws?.readyState === WebSocket.OPEN) {
      ws.send(encoder.encode(data))
      termRef.current?.focus()
    }
  }, [interactive])

  const copy = useCallback(async () => {
    const selection = termRef.current?.getSelection()
    if (!selection) return
    try { await navigator.clipboard.writeText(selection) } catch { termRef.current?.focus() }
  }, [])

  const paste = useCallback(async () => {
    try { send(await navigator.clipboard.readText()) } catch { termRef.current?.focus() }
  }, [send])

  const search = useCallback((direction: 'next' | 'previous') => {
    if (!query) return
    const options = { incremental: true }
    if (direction === 'next') searchRef.current?.findNext(query, options)
    else searchRef.current?.findPrevious(query, options)
  }, [query])

  useEffect(() => {
    const container = containerRef.current
    if (!container) return
    const term = new Terminal({
      theme: { background: '#000', foreground: '#e5e7eb', cursor: '#22d3ee', selectionBackground: '#155e75' },
      // The symbols-only Noto face comes first but contains no Latin glyphs,
      // so normal text still uses the native system monospace face.
      fontFamily: '\"Kyber Terminal Symbols\", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace',
      fontSize: window.matchMedia('(max-width: 640px)').matches ? 12 : 13,
      fontWeight: '500',
      fontWeightBold: '700',
      lineHeight: 1.2,
      cursorBlink: interactive,
      disableStdin: !interactive,
      scrollback: 10_000,
      minimumContrastRatio: 4.5,
      // Claude Code uses several symbols that xterm's synthetic glyph path
      // flattens at some device-pixel ratios. Let the font stack draw them.
      customGlyphs: false,
      rescaleOverlappingGlyphs: true,
      rightClickSelectsWord: true,
      allowProposedApi: true,
    })
    const fitAddon = new FitAddon()
    const searchAddon = new SearchAddon()
    term.loadAddon(fitAddon)
    term.loadAddon(searchAddon)
    term.loadAddon(new UnicodeGraphemesAddon())
    term.loadAddon(new ClipboardAddon())
    term.loadAddon(new WebLinksAddon((_event, uri) => {
      if (/^https?:\/\//i.test(uri)) window.open(uri, '_blank', 'noopener,noreferrer')
    }))
    let ws: WebSocket | null = null
    let reconnectTimer: ReturnType<typeof setTimeout> | undefined
    let reconnectAttempts = 0
    let disposed = false
    let sessionEnded = false
    let lastSize = ''
    let fitFrame = 0
    let observer: ResizeObserver | undefined

    const fit = () => {
      cancelAnimationFrame(fitFrame)
      fitFrame = requestAnimationFrame(() => {
        try { fitAddon.fit() } catch { return }
        const size = `${term.cols}x${term.rows}`
        if (size !== lastSize && ws?.readyState === WebSocket.OPEN) {
          lastSize = size
          ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }))
        }
      })
    }
    const connect = () => {
      if (disposed) return
      setConnection(reconnectAttempts ? 'reconnecting' : 'connecting')
      ws = api.execWebSocket(kind, name, mode)
      wsRef.current = ws
      ws.binaryType = 'arraybuffer'
      ws.onopen = () => { reconnectAttempts = 0; setConnection('connected'); fit() }
      ws.onmessage = (event: MessageEvent) => {
        if (event.data instanceof ArrayBuffer) term.write(new Uint8Array(event.data))
        else if (typeof event.data === 'string') {
          try {
            const message = JSON.parse(event.data) as { type?: string; error?: string }
            if (message.type === 'exit') {
              sessionEnded = true
              term.write(`\r\n\x1b[33m[session ended]\x1b[0m${message.error ? `\r\n\x1b[31m${message.error}\x1b[0m` : ''}\r\n`)
              setConnection('closed')
            }
          } catch { term.write(event.data) }
        }
      }
      ws.onerror = () => setConnection('reconnecting')
      ws.onclose = (event) => {
        if (disposed || sessionEnded || event.code === 1000 || mode === 'history') { setConnection('closed'); return }
        setConnection('reconnecting')
        reconnectAttempts += 1
        reconnectTimer = setTimeout(connect, Math.min(1000 * 2 ** (reconnectAttempts - 1), 15_000))
      }
    }

    term.attachCustomKeyEventHandler((event) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'f' && event.type === 'keydown') { setSearchOpen(true); return false }
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'c' && term.hasSelection()) {
        if (event.type === 'keydown') void navigator.clipboard.writeText(term.getSelection())
        return false
      }
      return true
    })
    const input = term.onData(send)
    const focusTerminal = () => term.focus()
    container.addEventListener('pointerdown', focusTerminal)
    const initialize = async () => {
      // xterm caches rasterized glyphs when it opens. Wait for the explicit
      // symbol fallback so Claude Code's UI glyphs are not cached from a
      // metric-incompatible browser fallback.
      await document.fonts?.load('400 13px "Kyber Terminal Symbols"', '●❯✻▎▛█▜▝▘')
      if (disposed) return
      term.open(container)
      try {
        const webgl = new WebglAddon()
        webgl.onContextLoss(() => webgl.dispose())
        term.loadAddon(webgl)
      } catch { /* canvas renderer remains active */ }
      termRef.current = term
      searchRef.current = searchAddon
      observer = new ResizeObserver(fit)
      observer.observe(container)
      fit()
      connect()
    }
    void initialize()

    return () => {
      disposed = true
      if (reconnectTimer) clearTimeout(reconnectTimer)
      cancelAnimationFrame(fitFrame)
      observer?.disconnect()
      container.removeEventListener('pointerdown', focusTerminal)
      input.dispose()
      ws?.close(1000)
      term.dispose()
      termRef.current = null
      wsRef.current = null
      searchRef.current = null
    }
  }, [api, interactive, kind, mode, name, send])

  return (
    <div className="overflow-hidden rounded-lg border border-border-subtle bg-surface-sunken">
      <div className="flex min-h-10 items-center gap-2 border-b border-border-subtle px-2 text-xs">
        <span className={`h-2 w-2 rounded-full ${connection === 'connected' ? 'bg-success' : connection === 'closed' ? 'bg-text-muted' : 'animate-pulse bg-warning'}`} />
        <span className="mr-auto capitalize text-text-muted">{connection}</span>
        <button type="button" title="Search terminal" aria-label="Search terminal" onClick={() => setSearchOpen((open) => !open)} className="rounded p-2 hover:bg-surface-hover"><Search size={15} /></button>
        <button type="button" title="Copy selection" aria-label="Copy selection" onClick={() => void copy()} className="rounded p-2 hover:bg-surface-hover"><ClipboardCopy size={15} /></button>
        {interactive && <button type="button" title="Paste" aria-label="Paste" onClick={() => void paste()} className="rounded p-2 hover:bg-surface-hover"><ClipboardPaste size={15} /></button>}
      </div>
      {searchOpen && <form className="flex gap-2 border-b border-border-subtle p-2" onSubmit={(event) => { event.preventDefault(); search('next') }}>
        <input autoFocus value={query} onChange={(event) => { setQuery(event.target.value); searchRef.current?.findNext(event.target.value, { incremental: true }) }} placeholder="Find in terminal" aria-label="Find in terminal" className="min-w-0 flex-1 rounded border border-border-subtle bg-surface px-2 py-1 text-sm" />
        <button type="button" aria-label="Previous match" onClick={() => search('previous')} className="rounded px-2 hover:bg-surface-hover">↑</button>
        <button type="submit" aria-label="Next match" className="rounded px-2 hover:bg-surface-hover">↓</button>
        <button type="button" aria-label="Close search" onClick={() => { setSearchOpen(false); searchRef.current?.clearDecorations(); termRef.current?.focus() }} className="rounded p-1 hover:bg-surface-hover"><X size={15} /></button>
      </form>}
      <div ref={containerRef} className={`${heightClassName ?? 'h-[min(60vh,42rem)] min-h-80'} w-full overflow-hidden`} />
      {interactive && <div className="flex gap-1 overflow-x-auto border-t border-border-subtle p-2 sm:hidden" aria-label="Terminal keys">
        {terminalExtraKeys.map((key) => <button key={key.label} type="button" title={key.title} onClick={() => send(key.data)} className="min-h-11 shrink-0 rounded border border-border-subtle bg-surface px-3 font-mono text-xs active:bg-surface-hover">{key.label}</button>)}
      </div>}
    </div>
  )
}
