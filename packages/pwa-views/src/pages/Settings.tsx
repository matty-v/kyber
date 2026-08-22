import { useState, useEffect } from 'react'
import { Eye, EyeOff, Copy, Check, RefreshCw } from 'lucide-react'
import { Card } from '../components/Card'
import { Button } from '../components/Button'
import { ConfirmDialog } from '../components/ConfirmDialog'
import { Input } from '@/components/ui/input'
import { establishEmbeddedBrowserSession } from '../lib/api'
import { DiagnosticsCard } from '../components/DiagnosticsCard'
import { UpdatesCard } from '../components/UpdatesCard'
import { useCluster } from '../lib/cluster-context'
import { useEffectiveModelList } from '../lib/models'
import type { AvailableModel } from '../lib/types'
import {
  useFleetDefaults,
  useRotateApiKey,
  useUpdateFleetDefaults,
} from '../hooks/useAPI'

export function Settings() {
  const cluster = useCluster()

  return (
    <div>
      <h1 className="text-xl font-bold text-text-primary mb-6">Settings</h1>

      <DiagnosticsCard />

      <UpdatesCard />

      {cluster.id === 'local' && <APIConnectionCard />}
      <RotateApiKeyCard />

      <FleetDefaultsCard />
    </div>
  )
}

type SessionStatus = 'checking' | 'configured' | 'not-configured'

export function APIConnectionCard() {
  const [apiKey, setApiKeyState] = useState('')
  const [saved, setSaved] = useState(false)
  const [revealed, setRevealed] = useState(false)
  const [copied, setCopied] = useState(false)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState('')
  const [sessionStatus, setSessionStatus] = useState<SessionStatus>('checking')

  useEffect(() => {
    let cancelled = false
    void fetch('/api/v1/config').then((response) => {
      if (!cancelled) {
        setSessionStatus(response.ok ? 'configured' : 'not-configured')
      }
    }).catch(() => {
      if (!cancelled) setSessionStatus('not-configured')
    })
    return () => {
      cancelled = true
    }
  }, [])

  async function save() {
    if (!apiKey) return
    setSaving(true)
    setSaveError('')
    try {
      await establishEmbeddedBrowserSession(apiKey)
      setApiKeyState('')
      setRevealed(false)
      setSaved(true)
      setSessionStatus('configured')
      setTimeout(() => setSaved(false), 2000)
    } catch {
      setSaveError('The API key was rejected or the control plane could not be reached.')
    } finally {
      setSaving(false)
    }
  }

  async function copyKey() {
    if (!apiKey) return
    try {
      await navigator.clipboard.writeText(apiKey)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      // Some browsers block writeText outside a user gesture — this is a click handler,
      // so the only realistic failure is a no-clipboard environment. Silently no-op.
    }
  }

  const hasKey = apiKey.length > 0

  return (
      <Card className="max-w-3xl mt-4">
        <h2 className="text-sm font-medium text-text-muted mb-4">API Connection</h2>
        <p className="mb-4 text-xs text-text-muted">
          Paste the key to establish an HttpOnly browser session. Kyber does
          not retain the raw key in browser-readable storage.
        </p>
        <p className="mb-4 text-xs text-text-muted" role="status">
          API key:{' '}
          <span className={sessionStatus === 'configured' ? 'text-success' : ''}>
            {sessionStatus === 'checking'
              ? 'Checking…'
              : sessionStatus === 'configured'
                ? 'Set (browser session active)'
                : 'Not set'}
          </span>
        </p>
        <div className="space-y-4">
          <div>
            <label className="block text-sm text-text-muted mb-1">API Key</label>
            <div className="relative">
              <Input
                type={revealed ? 'text' : 'password'}
                value={apiKey}
                onChange={(e) => {
                  setApiKeyState(e.target.value)
                  setSaved(false)
                }}
                placeholder="your-api-key"
                className="pr-20"
                aria-label="API key"
              />
              <div className="absolute inset-y-0 right-2 flex items-center gap-1">
                <button
                  type="button"
                  onClick={() => setRevealed((r) => !r)}
                  className="p-1 text-text-muted hover:text-text-primary focus:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring rounded"
                  aria-label={revealed ? 'Hide API key' : 'Show API key'}
                  aria-pressed={revealed}
                >
                  {revealed ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
                </button>
                <button
                  type="button"
                  onClick={copyKey}
                  disabled={!hasKey}
                  className="p-1 text-text-muted hover:text-text-primary focus:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring rounded disabled:opacity-40 disabled:cursor-not-allowed"
                  aria-label="Copy API key to clipboard"
                >
                  {copied ? <Check className="h-4 w-4 text-success" /> : <Copy className="h-4 w-4" />}
                </button>
              </div>
            </div>
          </div>

          <div className="flex items-center gap-3">
            <Button variant="primary" size="md" onClick={() => void save()} loading={saving} disabled={saving || !hasKey}>
              Save
            </Button>
            {saved && (
              <span className="text-sm text-success">Session established</span>
            )}
          </div>
          {saveError && <p className="text-sm text-danger">{saveError}</p>}
        </div>
      </Card>
  )
}

// FleetDefaultsCard — kyber#376. Reads + writes the kyber-fleet-defaults
// ConfigMap via /api/v1/fleet-defaults. defaultModel is consumed by the
// reconciler today (CLAUDE_MODEL injection when spec.model is empty);
// defaultRuntimeVersion is plumbed end-to-end here but consumed only
// from PR-C onwards.
function FleetDefaultsCard() {
  const query = useFleetDefaults()
  const mutation = useUpdateFleetDefaults()
  const claude = useEffectiveModelList('claude-code')
  const codex = useEffectiveModelList('codex')
  const [claudeModel, setClaudeModel] = useState('')
  const [claudeVersion, setClaudeVersion] = useState('')
  const [codexModel, setCodexModel] = useState('')
  const [codexVersion, setCodexVersion] = useState('')
  const [dirty, setDirty] = useState(false)
  // Set when the server rejected a model id as unknown to every catalog it
  // can see (400 VALIDATION_ERROR). Offers the force escape — the case for
  // it is a model newer than the last detection poll.
  const [modelRejection, setModelRejection] = useState<string | null>(null)

  // Sync editor state when the server-side values arrive. Skip when the
  // user has started editing — overwriting their in-progress text would
  // be surprising. The dirty flag clears after a save (via the
  // mutation's onSuccess invalidation re-firing the query).
  useEffect(() => {
    if (!query.data || dirty) return
    setClaudeModel(query.data.defaultModel)
    setClaudeVersion(query.data.defaultRuntimeVersion)
    setCodexModel(query.data.codexDefaultModel ?? '')
    setCodexVersion(query.data.codexDefaultRuntimeVersion ?? '')
  }, [query.data, dirty])

  async function save(force = false) {
    try {
      await mutation.mutateAsync({
        defaultModel: claudeModel,
        defaultRuntimeVersion: claudeVersion,
        codexDefaultModel: codexModel,
        codexDefaultRuntimeVersion: codexVersion,
        ...(force ? { force: true } : {}),
      })
      setDirty(false)
      setModelRejection(null)
    } catch (e) {
      // A model-validation 400 gets an inline escape hatch (the model may
      // simply be newer than the last detection poll); everything else is
      // surfaced via the mutation's errorPrefix toast.
      const err = e as { status?: number; code?: string; message?: string }
      if (err?.status === 400 && err?.code === 'VALIDATION_ERROR') {
        setModelRejection(err.message ?? 'The model id is not in any catalog this cluster can see.')
      } else {
        setModelRejection(null)
      }
    }
  }

  const unavailable = query.isError
  const loading = query.isLoading

  return (
    <Card className="max-w-3xl mt-4">
      <h2 className="text-sm font-medium text-text-muted mb-1">Agent harnesses</h2>
      <p className="text-xs text-text-muted mb-3">
        Fleet defaults applied when an agent omits its model or harness
        version. Each runtime has an independent fallback; existing pods
        adopt changes on their next recreation.
      </p>
      {unavailable && (
        <p className="text-xs text-warning mb-3">
          Fleet defaults are not available on this control plane (the
          kyber-fleet-defaults ConfigMap is not configured). Set
          controlPlane.fleetDefaults.configMapName in chart values.
        </p>
      )}
      <div className="grid gap-3 md:grid-cols-2">
        <HarnessDefaults
          id="claude-code"
          name="Claude Code"
          description="Anthropic models with the Claude Code harness."
          model={claudeModel}
          version={claudeVersion}
          models={claude.models}
          versions={claude.claudeCodeVersions}
          modelPlaceholder="Runtime default"
          versionPlaceholder="latest"
          disabled={loading || unavailable}
          onModelChange={(value) => { setClaudeModel(value); setDirty(true) }}
          onVersionChange={(value) => { setClaudeVersion(value); setDirty(true) }}
        />
        <HarnessDefaults
          id="codex"
          name="Codex"
          description="OpenAI models available to ChatGPT-authenticated Codex agents."
          model={codexModel}
          version={codexVersion}
          models={codex.models}
          versions={codex.codexVersions}
          modelPlaceholder="Runtime default"
          versionPlaceholder="latest"
          disabled={loading || unavailable}
          onModelChange={(value) => { setCodexModel(value); setDirty(true) }}
          onVersionChange={(value) => { setCodexVersion(value); setDirty(true) }}
        />
      </div>
      <p className="mt-3 text-[11px] text-text-disabled">
        Default lets the harness choose its model. Latest installs the current
        upstream harness whenever an agent pod is created. Concrete values pin
        that runtime until you change them.
      </p>
      {modelRejection && (
        <div className="mt-3 rounded border border-warning/40 bg-warning/5 p-2">
          <p className="text-xs text-warning">{modelRejection}</p>
          <p className="mt-1 text-[11px] text-text-muted">
            If this model is newer than the last detection poll, you can save it anyway.
          </p>
        </div>
      )}
      <div className="mt-3">
        <div className="flex items-center gap-3">
          <Button
            variant="primary"
            size="md"
            onClick={() => void save()}
            loading={mutation.isPending}
            disabled={mutation.isPending || loading || unavailable || !dirty}
          >
            Save
          </Button>
          {modelRejection && dirty && (
            <Button
              variant="secondary"
              size="md"
              onClick={() => void save(true)}
              loading={mutation.isPending}
              disabled={mutation.isPending || loading || unavailable}
            >
              Save anyway
            </Button>
          )}
          {!dirty && mutation.isSuccess && (
            <span className="text-sm text-success">Saved</span>
          )}
        </div>
      </div>
    </Card>
  )
}

interface HarnessDefaultsProps {
  id: string
  name: string
  description: string
  model: string
  version: string
  models: AvailableModel[]
  versions: string[]
  modelPlaceholder: string
  versionPlaceholder: string
  disabled: boolean
  onModelChange: (value: string) => void
  onVersionChange: (value: string) => void
}

function HarnessDefaults(props: HarnessDefaultsProps) {
  const modelListID = `${props.id}-models`
  const versionListID = `${props.id}-versions`
  return (
    <section className="rounded-lg border border-border-default bg-surface-overlay p-4">
      <h3 className="text-sm font-semibold text-text-primary">{props.name}</h3>
      <p className="mt-1 min-h-8 text-xs text-text-muted">{props.description}</p>
      <div className="mt-3 space-y-3">
        <div>
          <label className="block text-xs text-text-muted mb-1" htmlFor={`${props.id}-default-model`}>Default model</label>
          <Input id={`${props.id}-default-model`} list={modelListID} value={props.model} onChange={(e) => props.onModelChange(e.target.value)} placeholder={props.modelPlaceholder} disabled={props.disabled} />
          <datalist id={modelListID}>{props.models.map((model) => <option key={model.id} value={model.id}>{model.displayName}</option>)}</datalist>
          <button
            type="button"
            className={`mt-1.5 rounded border px-2 py-1 text-[11px] ${props.model === '' ? 'border-accent text-accent' : 'border-border-default text-text-muted hover:text-text-primary'}`}
            onClick={() => props.onModelChange('')}
            disabled={props.disabled}
          >
            Default
          </button>
        </div>
        <div>
          <label className="block text-xs text-text-muted mb-1" htmlFor={`${props.id}-default-version`}>Default harness version</label>
          <Input id={`${props.id}-default-version`} list={versionListID} value={props.version} onChange={(e) => props.onVersionChange(e.target.value)} placeholder={props.versionPlaceholder} disabled={props.disabled} />
          <datalist id={versionListID}>{props.versions.map((version) => <option key={version} value={version} />)}</datalist>
          <button
            type="button"
            className={`mt-1.5 rounded border px-2 py-1 text-[11px] ${props.version === 'latest' ? 'border-accent text-accent' : 'border-border-default text-text-muted hover:text-text-primary'}`}
            onClick={() => props.onVersionChange('latest')}
            disabled={props.disabled}
          >
            Latest
          </button>
        </div>
      </div>
    </section>
  )
}

function RotateApiKeyCard() {
  const rotate = useRotateApiKey()
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [newKey, setNewKey] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)

  async function doRotate() {
    setConfirmOpen(false)
    try {
      const resp = await rotate.mutateAsync()
      setNewKey(resp.apiKey)
    } catch {
      // Error toast is fired by the mutation's meta.errorPrefix path.
    }
  }

  async function copyNewKey() {
    if (!newKey) return
    try {
      await navigator.clipboard.writeText(newKey)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      // No clipboard — silently no-op.
    }
  }

  return (
    <>
      <Card className="max-w-3xl mt-4">
        <h2 className="text-sm font-medium text-text-muted mb-1">Rotate API key</h2>
        <p className="text-xs text-text-muted mb-3">
          Generates a new key, persists it to the kyber-api-credentials Secret,
          and updates the live authenticator. The old key is rejected
          immediately. Other browser sessions will need the new key pasted in.
        </p>
        <Button
          variant="danger"
          size="md"
          onClick={() => setConfirmOpen(true)}
          loading={rotate.isPending}
          disabled={rotate.isPending}
        >
          <RefreshCw className="h-4 w-4" />
          Rotate API key
        </Button>
      </Card>

      <ConfirmDialog
        open={confirmOpen}
        title="Rotate API key?"
        message="This generates a new control-plane API key and revokes the current one. All other browser sessions will be signed out. Continue?"
        confirmLabel="Rotate"
        cancelLabel="Cancel"
        dangerous
        loading={rotate.isPending}
        onConfirm={() => void doRotate()}
        onCancel={() => setConfirmOpen(false)}
      />

      {newKey && (
        // One-time readback modal. Hand-rolled — same shape as ConfirmDialog
        // so the visual language matches, but with a single dismiss action
        // rather than confirm/cancel.
        <div
          className="fixed inset-0 z-50 flex items-center justify-center p-4"
          role="dialog"
          aria-modal="true"
          aria-labelledby="rotated-key-title"
        >
          <div
            className="absolute inset-0 bg-surface-sunken/60 backdrop-blur-sm"
            onClick={() => setNewKey(null)}
          />
          <div className="relative z-10 w-full max-w-md rounded-xl border border-border-default bg-surface-overlay p-6 shadow-xl">
            <h2 id="rotated-key-title" className="font-display text-base font-semibold text-text-primary">
              New API key
            </h2>
            <p className="mt-2 text-sm text-text-muted">
              Copy this now. It will not be shown again. Paste it into other
              browsers / devices that need access.
            </p>
            <div className="mt-3 flex items-stretch gap-2">
              <code className="flex-1 break-all rounded-lg border border-border-default bg-surface-base px-3 py-2 font-mono text-xs text-text-primary">
                {newKey}
              </code>
              <button
                type="button"
                onClick={() => void copyNewKey()}
                className="rounded-lg border border-border-default bg-surface-base px-3 text-text-muted hover:text-text-primary focus:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring"
                aria-label="Copy new API key"
              >
                {copied ? <Check className="h-4 w-4 text-success" /> : <Copy className="h-4 w-4" />}
              </button>
            </div>
            <p className="mt-3 text-xs text-text-muted">
              This browser continues with a refreshed HttpOnly session. The raw
              key is not stored in browser-readable storage.
            </p>
            <div className="mt-5 flex justify-end">
              <Button variant="primary" size="sm" onClick={() => setNewKey(null)}>
                Done
              </Button>
            </div>
          </div>
        </div>
      )}
    </>
  )
}
