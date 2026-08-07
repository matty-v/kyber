// InboundDebugger (#208 Phase 3) — operator-driven "test a payload" tool
// embedded in the WebhooksTab. Picks an existing binding (or accepts a manual
// JSON spec for advanced cases), accepts a JSON payload + optional headers,
// and shows the receiver's Decision-shaped diagnostic via /api/v1/inbound-debug.
//
// The result panel is rendered by DebugResultsPanel so the AddWebhookWizard's
// embedded variant looks identical.

import { useEffect, useMemo, useState } from 'react'
import { Plus, Trash2 } from 'lucide-react'
import { useInboundDebug } from '../hooks/useAPI'
import type {
  AgentInboundBinding,
  InboundBindingWithStats,
  InboundDebugResponse,
} from '../lib/types'
import { Button } from './Button'
import { DebugResultsPanel } from './DebugResultsPanel'

interface Props {
  agentName: string
  bindings: InboundBindingWithStats[]
}

type HeaderRow = { id: number; key: string; value: string }

// stripStats narrows a list-side InboundBindingWithStats back to the spec
// shape the server expects on the debug request body.
function toBinding(b: InboundBindingWithStats): AgentInboundBinding {
  // Avoid unused-var lint by destructuring into _ prefixed locals.
  const { url: _url, stats: _stats, ...spec } = b
  // Placate the no-unused-vars lint without runtime cost.
  void _url
  void _stats
  return spec
}

export function InboundDebugger({ agentName, bindings }: Props) {
  const [selectedName, setSelectedName] = useState<string>(() => bindings[0]?.name ?? '')
  // Manual mode lets advanced users edit the binding spec inline as JSON.
  const [manual, setManual] = useState<boolean>(false)
  const [manualSpec, setManualSpec] = useState<string>('')
  const [payloadText, setPayloadText] = useState<string>('{\n  \n}')
  const [payloadError, setPayloadError] = useState<string | null>(null)
  const [headerRows, setHeaderRows] = useState<HeaderRow[]>([])
  const [result, setResult] = useState<InboundDebugResponse | null>(null)

  const debug = useInboundDebug()

  // Keep selectedName valid when the bindings list changes underneath us.
  useEffect(() => {
    if (bindings.length === 0) {
      setSelectedName('')
      return
    }
    if (!bindings.some((b) => b.name === selectedName)) {
      setSelectedName(bindings[0].name)
    }
  }, [bindings, selectedName])

  const selectedBinding = useMemo<InboundBindingWithStats | null>(() => {
    return bindings.find((b) => b.name === selectedName) ?? null
  }, [bindings, selectedName])

  // Validate JSON payload on blur; the parsed value is what's sent.
  function validatePayload(text: string): { ok: true; value: unknown } | { ok: false; error: string } {
    try {
      return { ok: true, value: JSON.parse(text) }
    } catch (e) {
      return { ok: false, error: e instanceof Error ? e.message : 'Invalid JSON' }
    }
  }

  function onPayloadBlur() {
    const v = validatePayload(payloadText)
    setPayloadError(v.ok ? null : v.error)
  }

  // The Run button needs the payload to be valid JSON. We re-check on every
  // render rather than relying on setState so the disabled state is always
  // correct without an extra controlled-state hop.
  const payloadValid = useMemo(() => validatePayload(payloadText).ok, [payloadText])

  // Manual-spec validity (only checked when manual mode is on).
  const manualBinding = useMemo<AgentInboundBinding | null>(() => {
    if (!manual) return null
    try {
      return JSON.parse(manualSpec) as AgentInboundBinding
    } catch {
      return null
    }
  }, [manual, manualSpec])

  // The binding spec we'll submit: manual override wins when toggled on.
  const effectiveBinding: AgentInboundBinding | null = manual
    ? manualBinding
    : selectedBinding
      ? toBinding(selectedBinding)
      : null

  const canRun = payloadValid && effectiveBinding !== null && !debug.isPending

  function addHeader() {
    setHeaderRows((rows) => [...rows, { id: Date.now() + rows.length, key: '', value: '' }])
  }

  function updateHeader(id: number, patch: Partial<HeaderRow>) {
    setHeaderRows((rows) => rows.map((r) => (r.id === id ? { ...r, ...patch } : r)))
  }

  function removeHeader(id: number) {
    setHeaderRows((rows) => rows.filter((r) => r.id !== id))
  }

  async function run() {
    const payloadParsed = validatePayload(payloadText)
    if (!payloadParsed.ok) {
      setPayloadError(payloadParsed.error)
      return
    }
    if (!effectiveBinding) return

    // Flatten header rows into a Record, dropping empty keys silently.
    const headers: Record<string, string> = {}
    for (const r of headerRows) {
      const k = r.key.trim()
      if (!k) continue
      headers[k] = r.value
    }

    setResult(null)
    try {
      const data = await debug.mutateAsync({
        binding: effectiveBinding,
        payload: payloadParsed.value,
        headers: Object.keys(headers).length > 0 ? headers : undefined,
        agent: agentName,
      })
      setResult(data)
    } catch {
      // Toast surfaced via mutation cache; nothing to do inline.
    }
  }

  function enterManualMode() {
    if (selectedBinding && !manualSpec) {
      setManualSpec(JSON.stringify(toBinding(selectedBinding), null, 2))
    }
    setManual(true)
  }

  return (
    <div className="space-y-4">
      <div>
        <h3 className="text-sm font-medium text-text-primary">Test a payload</h3>
        <p className="mt-1 text-xs text-text-muted">
          Run the receiver&rsquo;s evaluation against an arbitrary payload — without dispatching
          a real prompt. Useful for diagnosing why a real webhook was dropped.
        </p>
      </div>

      <div className="space-y-2">
        <label className="block text-xs font-medium text-text-muted" htmlFor="debug-binding">
          Binding
        </label>
        {!manual ? (
          <div className="flex items-center gap-2">
            <select
              id="debug-binding"
              value={selectedName}
              onChange={(e) => setSelectedName(e.target.value)}
              disabled={bindings.length === 0}
              className="flex-1 rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm text-text-primary focus:border-accent focus:outline-none disabled:opacity-50"
            >
              {bindings.length === 0 && <option value="">No bindings configured</option>}
              {bindings.map((b) => (
                <option key={b.name} value={b.name}>
                  {b.name}
                </option>
              ))}
            </select>
            <Button
              variant="ghost"
              size="sm"
              onClick={enterManualMode}
              disabled={bindings.length === 0}
            >
              Edit as JSON
            </Button>
          </div>
        ) : (
          <div className="space-y-1">
            <textarea
              value={manualSpec}
              onChange={(e) => setManualSpec(e.target.value)}
              rows={6}
              spellCheck={false}
              aria-label="Binding spec JSON"
              className="w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 font-mono text-xs text-text-primary focus:border-accent focus:outline-none"
            />
            <div className="flex items-center justify-between">
              {manualBinding === null && manualSpec.trim() !== '' && (
                <p className="text-[11px] text-danger">Binding spec must be valid JSON.</p>
              )}
              <Button variant="ghost" size="sm" onClick={() => setManual(false)} className="ml-auto">
                Use dropdown
              </Button>
            </div>
          </div>
        )}
      </div>

      <div>
        <label className="mb-1 block text-xs font-medium text-text-muted" htmlFor="debug-payload">
          Payload (JSON)
        </label>
        <textarea
          id="debug-payload"
          value={payloadText}
          onChange={(e) => {
            setPayloadText(e.target.value)
            // Clear stale error eagerly on edit.
            if (payloadError) setPayloadError(null)
          }}
          onBlur={onPayloadBlur}
          rows={8}
          spellCheck={false}
          placeholder='{ "action": "opened", "pull_request": { "title": "Demo PR" } }'
          className="w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 font-mono text-xs text-text-primary placeholder-text-disabled focus:border-accent focus:outline-none"
        />
        {payloadError && (
          <p className="mt-1 text-[11px] text-danger">Invalid JSON: {payloadError}</p>
        )}
      </div>

      <div>
        <div className="mb-1 flex items-center justify-between">
          <label className="block text-xs font-medium text-text-muted">Headers (optional)</label>
          <Button variant="ghost" size="sm" onClick={addHeader}>
            <Plus className="h-3.5 w-3.5" /> Add header
          </Button>
        </div>
        {headerRows.length === 0 && (
          <p className="text-[11px] text-text-muted">
            No headers — useful when binding declares an event header (e.g. X-GitHub-Event).
          </p>
        )}
        <div className="space-y-1.5">
          {headerRows.map((r) => (
            <div key={r.id} className="flex items-center gap-2">
              <input
                type="text"
                value={r.key}
                onChange={(e) => updateHeader(r.id, { key: e.target.value })}
                placeholder="Header name"
                aria-label="Header name"
                className="flex-1 rounded-lg border border-border-default bg-surface-overlay px-2.5 py-1.5 font-mono text-xs text-text-primary placeholder-text-disabled focus:border-accent focus:outline-none"
              />
              <input
                type="text"
                value={r.value}
                onChange={(e) => updateHeader(r.id, { value: e.target.value })}
                placeholder="value"
                aria-label="Header value"
                className="flex-1 rounded-lg border border-border-default bg-surface-overlay px-2.5 py-1.5 font-mono text-xs text-text-primary placeholder-text-disabled focus:border-accent focus:outline-none"
              />
              <Button
                variant="ghost"
                size="sm"
                onClick={() => removeHeader(r.id)}
                aria-label="Remove header"
              >
                <Trash2 className="h-3.5 w-3.5 text-danger" />
              </Button>
            </div>
          ))}
        </div>
      </div>

      <div className="flex items-center justify-end">
        <Button
          variant="primary"
          size="sm"
          onClick={() => void run()}
          loading={debug.isPending}
          disabled={!canRun}
        >
          Run
        </Button>
      </div>

      {result && (
        <div className="rounded-lg border border-border-subtle bg-surface-raised p-3">
          <DebugResultsPanel result={result} />
        </div>
      )}
    </div>
  )
}
