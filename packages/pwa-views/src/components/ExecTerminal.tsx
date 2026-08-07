import { useEffect, useMemo, useRef, useCallback } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { createApiClient } from '../lib/api'
import { useCluster } from '../lib/cluster-context'
import '@xterm/xterm/css/xterm.css'

// Shared across every keystroke — a new TextEncoder per event is wasted work.
const encoder = new TextEncoder()

interface Props {
  kind: 'agent' | 'machine'
  name: string
  mode?: 'attach' | 'shell' | 'history' | 'device-auth'
  // Tailwind height class for the terminal container. Default 'h-80'.
  heightClassName?: string
}

export function ExecTerminal({ kind, name, mode, heightClassName }: Props) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const fitRef = useRef<FitAddon | null>(null)

  // Best-effort write-to-clipboard; Safari mobile blocks writeText outside a
  // user gesture so swallow. Used only by the auto-copy-on-selection handler
  // inside the effect below.
  const writeClipboard = useCallback(async (text: string) => {
    if (!text) return
    try {
      await navigator.clipboard.writeText(text)
    } catch {
      // ignore
    }
  }, [])

  useEffect(() => {
    if (!containerRef.current) return

    const term = new Terminal({
      theme: {
        background: '#000000',
        foreground: '#e5e7eb',
        cursor: '#60a5fa',
      },
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 13,
      cursorBlink: true,
      scrollback: 5000,
    })

    const fitAddon = new FitAddon()
    term.loadAddon(fitAddon)
    term.open(containerRef.current)
    fitAddon.fit()

    termRef.current = term
    fitRef.current = fitAddon

    // Best-effort auto-copy on selection. The explicit Copy button is the
    // reliable path on browsers that block clipboard writes outside a user
    // gesture (Safari mobile in particular).
    const disposeSel = term.onSelectionChange(() => {
      if (term.hasSelection()) {
        void writeClipboard(term.getSelection())
      }
    })

    const ws = api.execWebSocket(kind, name, mode)
    wsRef.current = ws
    ws.binaryType = 'arraybuffer'

    ws.onopen = () => {
      term.write('\r\x1b[32mConnected\x1b[0m\r\n')
      const dims =
        term.rows && term.cols
          ? { type: 'resize', cols: term.cols, rows: term.rows }
          : null
      if (dims) ws.send(JSON.stringify(dims))
    }

    ws.onmessage = (e: MessageEvent) => {
      if (e.data instanceof ArrayBuffer) {
        term.write(new Uint8Array(e.data))
      } else if (typeof e.data === 'string') {
        try {
          const msg = JSON.parse(e.data) as { type?: string; error?: string }
          if (msg.type === 'exit') {
            term.write('\r\n\x1b[33m[session ended]\x1b[0m\r\n')
            if (msg.error) term.write(`\x1b[31m${msg.error}\x1b[0m\r\n`)
          }
        } catch {
          term.write(e.data)
        }
      }
    }

    ws.onclose = () => {
      term.write('\r\n\x1b[33m[connection closed]\x1b[0m\r\n')
    }

    ws.onerror = () => {
      term.write('\r\n\x1b[31m[connection error]\x1b[0m\r\n')
    }

    const disposeInput = term.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(encoder.encode(data))
      }
    })

    const resizeObserver = new ResizeObserver(() => {
      fitAddon.fit()
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(
          JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }),
        )
      }
    })
    if (containerRef.current) {
      resizeObserver.observe(containerRef.current)
    }

    return () => {
      disposeInput.dispose()
      disposeSel.dispose()
      resizeObserver.disconnect()
      ws.close()
      term.dispose()
      termRef.current = null
      wsRef.current = null
      fitRef.current = null
    }
  }, [kind, name, mode, writeClipboard, api])

  return (
    <div
      ref={containerRef}
      className={`${heightClassName ?? 'h-80'} w-full overflow-hidden rounded-lg border border-border-subtle bg-surface-sunken`}
    />
  )
}
