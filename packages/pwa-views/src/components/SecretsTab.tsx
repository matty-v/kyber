// Secrets tab for AgentDetail — list/add/delete/readback operator-uploaded
// per-agent user secrets. See docs/design/2026-04-18-user-secrets-design.md (#75).

import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { ChevronDown, Eye, FileText, HelpCircle, KeyRound, Plus, Trash2, Upload } from 'lucide-react'
import {
  useAgentSecrets,
  useDeleteAgentSecret,
  useImportAgentSecretsKV,
  usePutAgentSecretFile,
  usePutAgentSecretKV,
} from '../hooks/useAPI'
import { createApiClient } from '../lib/api'
import { useCluster } from '../lib/cluster-context'
import type { AgentSecret, AgentSecretKind } from '../lib/types'
import {
  MAX_USER_SECRET_ENTRY_BYTES,
  MAX_USER_SECRETS_AGGREGATE_BYTES,
  parseUserSecretImport,
  validateUserSecretKey,
} from '../lib/userSecretImport'
import { Button } from './Button'
import { Card } from './Card'
import { ConfirmDialog } from './ConfirmDialog'
import { EmptyState } from './EmptyState'
import { SecretsHelpContent } from './SecretsHelpContent'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`
  return `${(n / (1024 * 1024)).toFixed(2)} MiB`
}

function formatTimestamp(iso: string | undefined): string {
  if (!iso) return '—'
  try {
    return new Date(iso).toLocaleString(undefined, {
      dateStyle: 'short',
      timeStyle: 'short',
    })
  } catch {
    return iso
  }
}

function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message
  return 'Unknown error'
}

function readTextFile(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result ?? ''))
    reader.onerror = () => reject(reader.error ?? new Error('Unknown file read error'))
    reader.readAsText(file)
  })
}

interface Props {
  agentName: string
}

export function SecretsTab({ agentName }: Props) {
  const { data: secrets, isLoading, error, refetch } = useAgentSecrets(agentName)

  const [showAdd, setShowAdd] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<AgentSecret | null>(null)
  const [readback, setReadback] = useState<AgentSecret | null>(null)

  const deleteSecret = useDeleteAgentSecret()

  function confirmDelete() {
    if (!pendingDelete) return
    deleteSecret.mutate(
      { name: agentName, key: pendingDelete.key },
      {
        onSuccess: () => setPendingDelete(null),
      },
    )
  }

  return (
    <div className="space-y-3">
      {isLoading && (
        <Card className="text-sm text-text-muted">Loading secrets…</Card>
      )}

      {error && (
        <Card className="border-danger/40 bg-danger-muted text-sm text-danger">
          Failed to load secrets: {errorMessage(error)}
          <div className="mt-2">
            <Button variant="ghost" size="sm" onClick={() => void refetch()}>
              Retry
            </Button>
          </div>
        </Card>
      )}

      {!isLoading && !error && secrets && secrets.length === 0 && (
        <div className="space-y-3">
          <EmptyState
            icon={<KeyRound className="h-6 w-6" strokeWidth={1.5} />}
            title="No secrets set"
            description="Upload a key/value pair or a file. Secrets are mounted into the agent's environment at runtime."
            action={
              <Button variant="primary" size="sm" onClick={() => setShowAdd(true)}>
                <Plus className="h-3.5 w-3.5" /> Add secret
              </Button>
            }
          />
          <Collapsible className="mx-auto max-w-md">
            <CollapsibleTrigger asChild>
              <button
                type="button"
                aria-controls="secrets-how-it-works"
                className="group flex w-full items-center justify-center gap-1.5 rounded-md py-1.5 text-xs text-text-muted hover:text-text-secondary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring"
              >
                How secrets work
                <ChevronDown
                  className="h-3 w-3 transition-transform group-data-[state=open]:rotate-180"
                  aria-hidden="true"
                />
              </button>
            </CollapsibleTrigger>
            <CollapsibleContent id="secrets-how-it-works" className="px-4 pt-3">
              <SecretsHelpContent />
            </CollapsibleContent>
          </Collapsible>
        </div>
      )}

      {!isLoading && secrets && secrets.length > 0 && (
        <div className="space-y-3">
          <div className="flex items-center justify-between">
            <Button variant="primary" size="sm" onClick={() => setShowAdd(true)}>
              <Plus className="h-3.5 w-3.5" /> Add secret
            </Button>
            <Popover>
              <PopoverTrigger asChild>
                <button
                  type="button"
                  aria-label="How secrets work"
                  className="inline-flex h-8 w-8 items-center justify-center rounded-md text-text-muted hover:text-text-secondary hover:bg-surface-overlay focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring"
                >
                  <HelpCircle className="h-4 w-4" strokeWidth={1.5} />
                </button>
              </PopoverTrigger>
              <PopoverContent>
                <SecretsHelpContent />
              </PopoverContent>
            </Popover>
          </div>
          <Card className="p-0 overflow-hidden">
            <div className="overflow-x-auto">
            <table className="w-full text-sm min-w-[640px]">
              <thead>
                <tr className="border-b border-border-subtle bg-surface-raised/50">
                  <th className="px-4 py-2 text-left font-medium text-text-muted">Key</th>
                  <th className="px-4 py-2 text-left font-medium text-text-muted">Kind</th>
                  <th className="px-4 py-2 text-left font-medium text-text-muted">Size</th>
                  <th className="px-4 py-2 text-left font-medium text-text-muted">sha256</th>
                  <th className="px-4 py-2 text-left font-medium text-text-muted">Updated</th>
                  <th className="px-4 py-2"></th>
                </tr>
              </thead>
              <tbody>
                {secrets.map((s) => (
                  <tr key={s.key} className="border-b border-border-subtle/50 last:border-0">
                    <td className="px-4 py-2 font-mono text-text-primary">{s.key}</td>
                    <td className="px-4 py-2 text-text-secondary">
                      <span className="inline-flex items-center gap-1">
                        {s.kind === 'kv' ? (
                          <KeyRound className="h-3.5 w-3.5 text-text-muted" />
                        ) : (
                          <FileText className="h-3.5 w-3.5 text-text-muted" />
                        )}
                        {s.kind}
                      </span>
                    </td>
                    <td className="px-4 py-2 text-text-secondary">{formatBytes(s.size)}</td>
                    <td className="px-4 py-2 font-mono text-xs text-text-muted">
                      {s.sha256Prefix || '—'}
                    </td>
                    <td className="px-4 py-2 text-text-muted text-xs">
                      {formatTimestamp(s.updatedAt)}
                    </td>
                    <td className="px-4 py-2 text-right">
                      <div className="inline-flex gap-1">
                        <Tooltip>
                          <TooltipTrigger asChild>
                            <Button
                              variant="ghost"
                              size="sm"
                              onClick={() => setReadback(s)}
                              aria-label="Reveal / download value"
                            >
                              <Eye className="h-3.5 w-3.5" />
                            </Button>
                          </TooltipTrigger>
                          <TooltipContent>Reveal / download value</TooltipContent>
                        </Tooltip>
                        <Tooltip>
                          <TooltipTrigger asChild>
                            <Button
                              variant="ghost"
                              size="sm"
                              onClick={() => setPendingDelete(s)}
                              aria-label="Delete secret"
                            >
                              <Trash2 className="h-3.5 w-3.5" />
                            </Button>
                          </TooltipTrigger>
                          <TooltipContent>Delete secret</TooltipContent>
                        </Tooltip>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            </div>
          </Card>
        </div>
      )}

      {showAdd && (
        <AddSecretDialog
          agentName={agentName}
          existing={secrets ?? []}
          onClose={() => setShowAdd(false)}
        />
      )}

      {readback && (
        <ReadbackDialog
          agentName={agentName}
          secret={readback}
          onClose={() => setReadback(null)}
        />
      )}

      {pendingDelete && (
        <ConfirmDialog
          open={true}
          title="Delete secret?"
          message={`Permanently remove "${pendingDelete.key}" from agent "${agentName}". The pod will roll to drop the value.`}
          confirmLabel="Delete"
          dangerous
          loading={deleteSecret.isPending}
          onConfirm={confirmDelete}
          onCancel={() => setPendingDelete(null)}
        />
      )}
    </div>
  )
}

// ---- Add dialog ----

interface AddProps {
  agentName: string
  existing: AgentSecret[]
  onClose: () => void
}

type AddMode = AgentSecretKind | 'env-file'

function AddSecretDialog({ agentName, existing, onClose }: AddProps) {
  const titleId = useId()
  const [mode, setMode] = useState<AddMode>('kv')
  const [key, setKey] = useState('')
  const [value, setValue] = useState('')
  const [file, setFile] = useState<File | null>(null)
  const [submitErr, setSubmitErr] = useState<string | null>(null)
  const keyInputRef = useRef<HTMLInputElement>(null)

  const putKV = usePutAgentSecretKV()
  const putFile = usePutAgentSecretFile()
  const importKV = useImportAgentSecretsKV()
  const busy = putKV.isPending || putFile.isPending || importKV.isPending

  useEffect(() => {
    keyInputRef.current?.focus()
  }, [])

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape' && !busy) onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [busy, onClose])

  const keyError = useMemo(() => (key ? validateUserSecretKey(key) : null), [key])

  const existingKey = useMemo(
    () => existing.find((s) => s.key === key),
    [existing, key],
  )

  function submit() {
    setSubmitErr(null)
    if (mode === 'env-file') {
      if (!file) {
        setSubmitErr('Pick a key=value file to upload')
        return
      }
      if (file.size > MAX_USER_SECRETS_AGGREGATE_BYTES) {
        setSubmitErr(`File exceeds ${MAX_USER_SECRETS_AGGREGATE_BYTES} bytes`)
        return
      }
      void readTextFile(file).then((contents) => {
        try {
          const entries = parseUserSecretImport(contents)
          const existingByKey = new Map(existing.map((entry) => [entry.key, entry]))
          const encoder = new TextEncoder()
          let aggregate = existing.reduce((total, entry) => total + entry.size, 0)
          for (const entry of entries) {
            aggregate -= existingByKey.get(entry.key)?.size ?? 0
            aggregate += encoder.encode(entry.value).length
          }
          if (aggregate > MAX_USER_SECRETS_AGGREGATE_BYTES) {
            throw new Error(`Import would exceed the ${MAX_USER_SECRETS_AGGREGATE_BYTES} byte aggregate limit`)
          }
          importKV.mutate(
            { name: agentName, entries },
            {
              onSuccess: onClose,
              onError: (e) => setSubmitErr(errorMessage(e)),
            },
          )
        } catch (e) {
          setSubmitErr(errorMessage(e))
        }
      }).catch((e) => setSubmitErr(`Failed to read file: ${errorMessage(e)}`))
      return
    }

    const err = validateUserSecretKey(key)
    if (err) {
      setSubmitErr(err)
      return
    }
    if (mode === 'kv') {
      if (!value) {
        setSubmitErr('Value is required')
        return
      }
      if (new TextEncoder().encode(value).length > MAX_USER_SECRET_ENTRY_BYTES) {
        setSubmitErr(`Value exceeds ${MAX_USER_SECRET_ENTRY_BYTES} bytes`)
        return
      }
      putKV.mutate(
        { name: agentName, key, value },
        {
          onSuccess: onClose,
          onError: (e) => setSubmitErr(errorMessage(e)),
        },
      )
    } else {
      if (!file) {
        setSubmitErr('Pick a file to upload')
        return
      }
      if (file.size > MAX_USER_SECRET_ENTRY_BYTES) {
        setSubmitErr(`File exceeds ${MAX_USER_SECRET_ENTRY_BYTES} bytes`)
        return
      }
      putFile.mutate(
        { name: agentName, key, file },
        {
          onSuccess: onClose,
          onError: (e) => setSubmitErr(errorMessage(e)),
        },
      )
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div
        className="absolute inset-0 bg-surface-sunken/60 backdrop-blur-sm"
        onClick={busy ? undefined : onClose}
      />
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className="relative z-10 w-full max-w-md rounded-xl border border-border-subtle bg-surface-raised p-6 shadow-xl"
      >
        <h2 id={titleId} className="text-base font-semibold text-text-primary mb-4">
          Add secret
        </h2>

        <div className="space-y-4">
          {/* Kind toggle */}
          <div>
            <label className="block text-xs font-medium text-text-muted mb-1">Kind</label>
            <div className="inline-flex rounded-lg border border-border-subtle bg-surface-base p-0.5">
              <button
                type="button"
                onClick={() => setMode('kv')}
                className={`px-3 py-1 text-xs font-medium rounded-md transition-colors ${
                  mode === 'kv'
                    ? 'bg-accent text-white'
                    : 'text-text-muted hover:text-text-primary'
                }`}
              >
                kv (env var)
              </button>
              <button
                type="button"
                onClick={() => setMode('file')}
                className={`px-3 py-1 text-xs font-medium rounded-md transition-colors ${
                  mode === 'file'
                    ? 'bg-accent text-white'
                    : 'text-text-muted hover:text-text-primary'
                }`}
              >
                file (/user-secrets)
              </button>
              <button
                type="button"
                onClick={() => setMode('env-file')}
                className={`px-3 py-1 text-xs font-medium rounded-md transition-colors ${
                  mode === 'env-file'
                    ? 'bg-accent text-white'
                    : 'text-text-muted hover:text-text-primary'
                }`}
              >
                key=value file
              </button>
            </div>
          </div>

          {/* Key */}
          {mode !== 'env-file' && <div>
            <label className="block text-xs font-medium text-text-muted mb-1">
              Key
            </label>
            <input
              ref={keyInputRef}
              type="text"
              value={key}
              onChange={(e) => setKey(e.target.value.toUpperCase())}
              placeholder="MY_TOKEN"
              autoCapitalize="characters"
              spellCheck={false}
              className="w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm font-mono text-text-primary placeholder-text-disabled focus:border-accent focus:outline-none"
            />
            {keyError && (
              <p className="mt-1 text-xs text-danger">{keyError}</p>
            )}
            {!keyError && existingKey && (
              <p className="mt-1 text-xs text-warn">
                Replaces existing {existingKey.kind} entry with same key.
              </p>
            )}
          </div>}

          {/* Value input */}
          {mode === 'kv' ? (
            <div>
              <label className="block text-xs font-medium text-text-muted mb-1">
                Value
              </label>
              <textarea
                value={value}
                onChange={(e) => setValue(e.target.value)}
                rows={4}
                spellCheck={false}
                className="w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm font-mono text-text-primary placeholder-text-disabled focus:border-accent focus:outline-none"
                placeholder="Secret value (never echoed after save)"
              />
              <p className="mt-1 text-xs text-text-muted">
                {new TextEncoder().encode(value).length} / {MAX_USER_SECRET_ENTRY_BYTES} bytes
              </p>
            </div>
          ) : (
            <div>
              <label className="block text-xs font-medium text-text-muted mb-1">
                File
              </label>
              <label className="flex items-center gap-2 cursor-pointer rounded-lg border border-dashed border-border-default bg-surface-overlay px-3 py-3 hover:border-border-strong">
                <Upload className="h-4 w-4 text-text-muted" />
                <span className="text-sm text-text-secondary truncate">
                  {file ? `${file.name} (${formatBytes(file.size)})` : mode === 'env-file' ? 'Choose .env file…' : 'Choose file…'}
                </span>
                <input
                  type="file"
                  className="hidden"
                  onChange={(e) => setFile(e.target.files?.[0] ?? null)}
                />
              </label>
              {file && file.size > (mode === 'env-file' ? MAX_USER_SECRETS_AGGREGATE_BYTES : MAX_USER_SECRET_ENTRY_BYTES) && (
                <p className="mt-1 text-xs text-danger">
                  File exceeds {formatBytes(mode === 'env-file' ? MAX_USER_SECRETS_AGGREGATE_BYTES : MAX_USER_SECRET_ENTRY_BYTES)} limit
                </p>
              )}
              {mode === 'env-file' && (
                <p className="mt-1 text-xs text-text-muted">
                  One KEY=VALUE per line. Blank lines and # comments are ignored.
                  Existing keys are replaced; values are not shell-expanded.
                </p>
              )}
            </div>
          )}

          {submitErr && (
            <div className="rounded-md border border-danger/40 bg-danger-muted px-3 py-2 text-xs text-danger">
              {submitErr}
            </div>
          )}
        </div>

        <div className="mt-5 flex gap-3 justify-end">
          <Button variant="ghost" size="sm" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            variant="primary"
            size="sm"
            onClick={submit}
            loading={busy}
            disabled={mode === 'env-file' ? !file : !key || Boolean(keyError)}
          >
            {mode === 'env-file' ? 'Import' : 'Save'}
          </Button>
        </div>
      </div>
    </div>
  )
}

// ---- Readback dialog ----

interface ReadbackProps {
  agentName: string
  secret: AgentSecret
  onClose: () => void
}

function ReadbackDialog({ agentName, secret, onClose }: ReadbackProps) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const titleId = useId()
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState<string | null>(null)
  const [kvValue, setKvValue] = useState<string>('')
  const [blobUrl, setBlobUrl] = useState<string | null>(null)
  const [filename, setFilename] = useState<string>('')
  const [copied, setCopied] = useState(false)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    setErr(null)
    api.getAgentSecretValue(agentName, secret.key)
      .then((res) => {
        if (cancelled) return
        if (res.kind === 'kv') {
          setKvValue(res.value)
        } else {
          setBlobUrl(URL.createObjectURL(res.value))
          setFilename(res.filename)
        }
      })
      .catch((e) => {
        if (!cancelled) setErr(errorMessage(e))
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [agentName, secret.key, api])

  // Revoke object URL on close so we don't leak.
  useEffect(() => {
    return () => {
      if (blobUrl) URL.revokeObjectURL(blobUrl)
    }
  }, [blobUrl])

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  async function copyToClipboard() {
    try {
      await navigator.clipboard.writeText(kvValue)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      // Clipboard API blocked — user can still select manually.
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div
        className="absolute inset-0 bg-surface-sunken/60 backdrop-blur-sm"
        onClick={onClose}
      />
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className="relative z-10 w-full max-w-lg rounded-xl border border-border-subtle bg-surface-raised p-6 shadow-xl"
      >
        <h2 id={titleId} className="text-base font-semibold text-text-primary mb-1 font-mono">
          {secret.key}
        </h2>
        <p className="text-xs text-text-muted mb-4">
          {secret.kind} • {formatBytes(secret.size)} • sha256 {secret.sha256Prefix || '—'}
        </p>

        {loading && <p className="text-sm text-text-muted">Loading…</p>}

        {err && (
          <div className="rounded-md border border-danger/40 bg-danger-muted px-3 py-2 text-sm text-danger">
            {err}
          </div>
        )}

        {!loading && !err && secret.kind === 'kv' && (
          <div className="space-y-2">
            <pre className="max-h-64 overflow-auto rounded-lg border border-border-subtle bg-surface-base p-3 text-xs font-mono text-text-primary whitespace-pre-wrap break-all">
              {kvValue}
            </pre>
            <Button variant="secondary" size="sm" onClick={() => void copyToClipboard()}>
              {copied ? 'Copied' : 'Copy to clipboard'}
            </Button>
          </div>
        )}

        {!loading && !err && secret.kind === 'file' && blobUrl && (
          <div className="space-y-2">
            <p className="text-sm text-text-secondary">
              Download the file to inspect — the value is not displayed inline.
            </p>
            <a
              href={blobUrl}
              download={filename}
              className="inline-flex items-center gap-1.5 rounded-lg bg-accent hover:bg-accent text-white px-3 py-1.5 text-xs font-medium"
            >
              <FileText className="h-3.5 w-3.5" /> Download {filename}
            </a>
          </div>
        )}

        <div className="mt-5 flex justify-end">
          <Button variant="ghost" size="sm" onClick={onClose}>
            Close
          </Button>
        </div>
      </div>
    </div>
  )
}
