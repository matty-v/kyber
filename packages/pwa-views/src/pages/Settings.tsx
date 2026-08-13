import { useState, useEffect } from 'react'
import { Eye, EyeOff, Copy, Check, RefreshCw } from 'lucide-react'
import { Card } from '../components/Card'
import { Button } from '../components/Button'
import { ConfirmDialog } from '../components/ConfirmDialog'
import { Input } from '@/components/ui/input'
import {
  getEmbeddedApiKey as getApiKey,
  setEmbeddedApiKey as setApiKey,
} from '../lib/api'
import { DiagnosticsCard } from '../components/DiagnosticsCard'
import { UpdatesCard } from '../components/UpdatesCard'
import { useDensity, type Density } from '../contexts/DensityContext'
import { useEffectiveModelList } from '../lib/models'
import type { AvailableModel } from '../lib/types'
import {
  useFleetDefaults,
  useRotateApiKey,
  useUpdateFleetDefaults,
  useAnthropicKeyStatus,
  useSetAnthropicKey,
  useClearAnthropicKey,
  useAvailable,
} from '../hooks/useAPI'

const MASK_SUFFIX_CHARS = 4

function maskedPreview(key: string): string {
  if (!key) return ''
  if (key.length <= MASK_SUFFIX_CHARS) return '•'.repeat(key.length)
  return '•'.repeat(10) + key.slice(-MASK_SUFFIX_CHARS)
}

export function Settings() {
  const [apiKey, setApiKeyState] = useState('')
  const [saved, setSaved] = useState(false)
  const [revealed, setRevealed] = useState(false)
  const [copied, setCopied] = useState(false)

  useEffect(() => {
    setApiKeyState(getApiKey())
  }, [])

  function save() {
    setApiKey(apiKey)
    setSaved(true)
    setTimeout(() => setSaved(false), 2000)
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
    <div>
      <h1 className="text-xl font-bold text-text-primary mb-6">Settings</h1>

      <Card className="max-w-lg">
        <h2 className="text-sm font-medium text-text-muted mb-4">API Connection</h2>
        <div className="space-y-4">
          <div>
            <label className="block text-sm text-text-muted mb-1">API Key</label>
            <div className="relative">
              <Input
                // type="text" unconditionally — using type="password" would mask
                // every character in the masked-preview string too, which defeats
                // the deliberate last-4-visible hint. The `revealed` flag governs
                // which string is bound to `value`, not the input type. `readOnly`
                // while masked prevents accidental edits.
                type="text"
                value={revealed ? apiKey : (hasKey ? maskedPreview(apiKey) : '')}
                onChange={(e) => {
                  // Only accept edits while revealed — typing into a masked field
                  // would replace the mask string with the typed characters and
                  // clobber the stored key silently.
                  if (!revealed) return
                  setApiKeyState(e.target.value)
                  setSaved(false)
                }}
                readOnly={!revealed}
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
            <Button variant="primary" size="md" onClick={save}>
              Save
            </Button>
            {saved && (
              <span className="text-sm text-success">Saved</span>
            )}
          </div>
        </div>
      </Card>

      <UpdatesCard />
      <DensityCard />

      <FleetDefaultsCard />

      <RotateApiKeyCard onRotated={(k) => setApiKeyState(k)} />

      <ModelDiscoveryCard />

      <DiagnosticsCard />
    </div>
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

  async function save() {
    try {
      await mutation.mutateAsync({
        defaultModel: claudeModel,
        defaultRuntimeVersion: claudeVersion,
        codexDefaultModel: codexModel,
        codexDefaultRuntimeVersion: codexVersion,
      })
      setDirty(false)
    } catch {
      // Surfaced via the mutation's errorPrefix toast.
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
          modelPlaceholder="claude-sonnet-4-5"
          versionPlaceholder="2.1.119"
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
          modelPlaceholder="gpt-5.6-sol"
          versionPlaceholder="0.146.0"
          disabled={loading || unavailable}
          onModelChange={(value) => { setCodexModel(value); setDirty(true) }}
          onVersionChange={(value) => { setCodexVersion(value); setDirty(true) }}
        />
      </div>
      <p className="mt-3 text-[11px] text-text-disabled">
        Leave a model blank to require an explicit per-agent model. Leave a
        harness version blank to use the version baked into its runtime image.
      </p>
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
        </div>
        <div>
          <label className="block text-xs text-text-muted mb-1" htmlFor={`${props.id}-default-version`}>Default harness version</label>
          <Input id={`${props.id}-default-version`} list={versionListID} value={props.version} onChange={(e) => props.onVersionChange(e.target.value)} placeholder={props.versionPlaceholder} disabled={props.disabled} />
          <datalist id={versionListID}>{props.versions.map((version) => <option key={version} value={version} />)}</datalist>
        </div>
      </div>
    </section>
  )
}

// AnthropicKeyCard manages the Anthropic API key used by the control-plane
// detection poller (kyber#375 PR-A). Write-only: the value is never
// returned by the server, even to authenticated callers. The card only
// reveals whether a key is configured + provides Save/Clear controls.
// Exported for tests: the disabled state below is only reachable through a
// specific server response, and asserting it through the whole Settings page
// would mean mocking every unrelated card's hooks.
export function ModelDiscoveryCard() {
  const available = useAvailable()
  const status = useAnthropicKeyStatus()
  const setKey = useSetAnthropicKey()
  const clearKey = useClearAnthropicKey()
  const [draft, setDraft] = useState('')
  const [revealed, setRevealed] = useState(false)
  const [confirmClearOpen, setConfirmClearOpen] = useState(false)

  const configured = status.data?.configured ?? false

  // supported:false means the control plane has no anthropic-key Secret to
  // write into — model discovery is off on this install (runtimeDetect.enabled)
  // — so there is nowhere for a key to go and PUT would 503. Without this the
  // panel rendered a key field and a Save button that always failed, and the
  // operator found out only AFTER typing a live credential into it.
  //
  // Keyed on the response body, not on an HTTP status: a 503 can equally come
  // from a rolling control plane or a tunnel with no origin, and telling an
  // operator to change their values because of a transient blip would be its
  // own wrong answer. An older control plane omits the field, which reads as
  // undefined and leaves the field on — today's behaviour.
  const unavailable = status.data?.supported === false

  async function save() {
    if (!draft) return
    await setKey.mutateAsync(draft)
    setDraft('')
    setRevealed(false)
  }

  async function doClear() {
    setConfirmClearOpen(false)
    try {
      await clearKey.mutateAsync()
    } catch {
      // Error toast is fired by the mutation's meta.errorPrefix path.
    }
  }

  return (
    <>
      <Card className="max-w-3xl mt-4">
        <h2 className="text-sm font-medium text-text-muted mb-1">Model discovery</h2>
        <p className="text-xs text-text-muted mb-3">How Kyber keeps each harness's model and version catalog current.</p>
        <div className="grid gap-3 md:grid-cols-2">
          <section className="rounded-lg border border-border-default bg-surface-overlay p-4">
            <h3 className="text-sm font-semibold text-text-primary">Anthropic</h3>
            {unavailable ? (
              <>
                <p className="mt-1 text-xs text-text-muted">
                  Model discovery is turned off on this install, so there is no
                  Secret to store a platform key in.
                </p>
                <p className="mt-3 text-[11px] text-text-disabled">
                  Set <code className="text-text-muted">runtimeDetect.enabled: true</code> in your
                  values and upgrade to enable it. Claude Code versions are discovered
                  from npm independently and are unaffected.
                </p>
              </>
            ) : (
              <>
            <p className="mt-1 text-xs text-text-muted">
              The Models API requires a platform key. It is stored write-only
              in a control-plane Secret. Status: {configured ? <span className="text-success">configured</span> : <span>not configured</span>}.
            </p>
            <p className="mt-1 text-[11px] text-text-disabled">Claude Code versions are discovered from npm independently.</p>
            <div className="mt-3 space-y-3">
          <div>
            <label className="block text-sm text-text-muted mb-1">
              {configured ? 'Replace key' : 'Enter key'}
            </label>
            <div className="relative">
              <Input
                type={revealed ? 'text' : 'password'}
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                placeholder={configured ? '••••••••••• (a key is set)' : 'sk-ant-...'}
                className="pr-10"
                aria-label="Anthropic API key"
                autoComplete="off"
                spellCheck={false}
              />
              <div className="absolute inset-y-0 right-2 flex items-center">
                <button
                  type="button"
                  onClick={() => setRevealed((r) => !r)}
                  className="p-1 text-text-muted hover:text-text-primary focus:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring rounded"
                  aria-label={revealed ? 'Hide key' : 'Show key while typing'}
                  aria-pressed={revealed}
                >
                  {revealed ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
                </button>
              </div>
            </div>
          </div>
          <div className="flex items-center gap-2">
            <Button
              variant="primary"
              size="md"
              onClick={() => void save()}
              loading={setKey.isPending}
              disabled={setKey.isPending || draft.length === 0}
            >
              {configured ? 'Replace' : 'Save'}
            </Button>
            {configured && (
              <Button
                variant="danger"
                size="md"
                onClick={() => setConfirmClearOpen(true)}
                loading={clearKey.isPending}
                disabled={clearKey.isPending}
              >
                Clear
              </Button>
            )}
            {setKey.isSuccess && !setKey.isPending && (
              <span className="text-sm text-success">Saved</span>
            )}
          </div>
            </div>
              </>
            )}
          </section>
          <section className="rounded-lg border border-border-default bg-surface-overlay p-4">
            <h3 className="text-sm font-semibold text-text-primary">OpenAI</h3>
            <p className="mt-1 text-xs text-text-muted">
              No platform API key is required. Online Codex agents report the
              picker-visible models available to their ChatGPT subscription.
            </p>
            <dl className="mt-4 grid grid-cols-2 gap-3 text-xs">
              <div><dt className="text-text-disabled">Models</dt><dd className="mt-1 text-text-primary">{available.data?.codexModels?.length ?? 0} detected</dd></div>
              <div><dt className="text-text-disabled">Harness versions</dt><dd className="mt-1 text-text-primary">{available.data?.codexVersions?.length ?? 0} from npm</dd></div>
            </dl>
            <p className="mt-3 text-[11px] text-text-disabled">The latest authenticated report is shared with model pickers; credentials never leave the agent pod.</p>
          </section>
        </div>
      </Card>
      <ConfirmDialog
        open={confirmClearOpen}
        title="Clear Anthropic API key?"
        message="Detection of new Claude models will stop until a new key is entered. The CC versions list (npm) is unaffected."
        confirmLabel="Clear"
        cancelLabel="Cancel"
        dangerous
        loading={clearKey.isPending}
        onConfirm={() => void doClear()}
        onCancel={() => setConfirmClearOpen(false)}
      />
    </>
  )
}

function DensityCard() {
  const { density, setDensity } = useDensity()

  function pick(next: Density) {
    return () => setDensity(next)
  }

  const optionClass = (selected: boolean) =>
    `flex-1 rounded-lg border px-3 py-2 text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring ${
      selected
        ? 'bg-accent-muted border-accent text-text-primary'
        : 'border-border-default bg-surface-overlay text-text-muted hover:border-border-strong'
    }`

  return (
    <Card className="max-w-lg mt-4">
      <h2 className="text-sm font-medium text-text-muted mb-1">Density</h2>
      <p className="text-xs text-text-muted mb-3">
        Compact tightens row heights and padding. Useful on large displays
        where you want more rows on screen.
      </p>
      <div
        role="radiogroup"
        aria-label="Density"
        className="flex gap-2"
      >
        <button
          type="button"
          role="radio"
          aria-checked={density === 'comfortable'}
          onClick={pick('comfortable')}
          className={optionClass(density === 'comfortable')}
        >
          Comfortable
        </button>
        <button
          type="button"
          role="radio"
          aria-checked={density === 'compact'}
          onClick={pick('compact')}
          className={optionClass(density === 'compact')}
        >
          Compact
        </button>
      </div>
      <p className="mt-2 text-[11px] text-text-disabled">
        Persists to localStorage. Mobile viewports always render comfortable
        regardless of preference.
      </p>
    </Card>
  )
}

interface RotateApiKeyCardProps {
  /** Called after a successful rotation so the parent's revealed-key state
   *  reflects the new key. */
  onRotated: (newKey: string) => void
}

function RotateApiKeyCard({ onRotated }: RotateApiKeyCardProps) {
  const rotate = useRotateApiKey()
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [newKey, setNewKey] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)

  async function doRotate() {
    setConfirmOpen(false)
    try {
      const resp = await rotate.mutateAsync()
      setNewKey(resp.apiKey)
      onRotated(resp.apiKey)
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
      <Card className="max-w-lg mt-4">
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
              Stored in this browser's localStorage automatically.
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
