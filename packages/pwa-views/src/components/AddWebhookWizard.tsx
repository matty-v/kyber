// AddWebhookWizard (#208) — multi-step modal Dialog for declaring a new inbound
// prompt binding. Adapts the page-level CreateAgent wizard pattern (numbered
// step strip + per-step content + back/next/submit) into a Dialog because a
// binding is a much smaller add than an agent.
//
// On successful create the wizard swaps to a "Setup complete" panel that
// surfaces the webhook URL + secret EXACTLY ONCE — Kyber stores only a hash.

import { useEffect, useId, useMemo, useState } from 'react'
import { toast } from 'sonner'
import { Copy, FlaskConical, Plus, Trash2, X } from 'lucide-react'
import {
  useCreateInboundBinding,
  useInboundDebug,
  useUpdateInboundBinding,
} from '../hooks/useAPI'
import type {
  AgentInboundBinding,
  AgentInboundField,
  AgentInboundFilter,
  InboundCreateResponse,
  InboundDebugResponse,
} from '../lib/types'
import { Button } from './Button'
import { DebugResultsPanel } from './DebugResultsPanel'

// Mirrors the CRD validator on AgentInboundBinding.Name.
const NAME_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/

const STEP_LABELS = [
  'Template',
  'Name',
  'Auth',
  'Matching',
  'Fields',
  'Action',
  'Limits',
  'Review',
] as const

type StepIndex = 0 | 1 | 2 | 3 | 4 | 5 | 6 | 7

type Template = 'github' | 'generic'

interface WizardState {
  template: Template | null
  name: string
  signatureHeader: string
  signaturePrefix: string
  eventHeader: string
  eventPath: string
  matchEvents: string[]
  filters: AgentInboundFilter[]
  fields: AgentInboundField[]
  action: string
  maxPerMinute: number
}

function emptyState(): WizardState {
  return {
    template: null,
    name: '',
    signatureHeader: '',
    signaturePrefix: '',
    eventHeader: '',
    eventPath: '',
    matchEvents: [],
    filters: [],
    fields: [],
    action: '',
    maxPerMinute: 10,
  }
}

// stateFromBinding seeds the wizard state from an existing binding for
// edit mode (#222). Template is set to 'generic' so it doesn't try to
// re-apply template defaults — we already have concrete values from the
// existing binding.
function stateFromBinding(b: AgentInboundBinding): WizardState {
  return {
    template: 'generic',
    name: b.name,
    signatureHeader: b.signatureHeader ?? '',
    signaturePrefix: b.signaturePrefix ?? '',
    eventHeader: b.eventHeader ?? '',
    eventPath: b.eventPath ?? '',
    matchEvents: b.matchEvents ? [...b.matchEvents] : [],
    filters: b.filters ? b.filters.map((f) => ({ ...f })) : [],
    fields: b.fields ? b.fields.map((f) => ({ ...f })) : [],
    action: b.action ?? '',
    maxPerMinute: b.limits?.maxPerMinute ?? 10,
  }
}

function applyTemplate(state: WizardState, template: Template): WizardState {
  if (template === 'github') {
    return {
      ...state,
      template,
      signatureHeader: state.signatureHeader || 'X-Hub-Signature-256',
      signaturePrefix: state.signaturePrefix || 'sha256=',
      eventHeader: state.eventHeader || 'X-GitHub-Event',
      eventPath: state.eventPath || '$.action',
    }
  }
  // Generic — leave fields blank for the operator to fill in.
  return { ...state, template }
}

// buildBindingDraft converts the wizard's in-progress state into the
// AgentInboundBinding wire shape used by /api/v1/inbound-debug. The wizard
// hasn't created the Kubernetes Secret yet, so existingSecret is left empty
// — the debug endpoint accepts an empty secret reference because no real
// HMAC verification happens during a debug evaluation.
function buildBindingDraft(s: WizardState): AgentInboundBinding {
  return {
    name: s.name || 'draft',
    existingSecret: '',
    signatureHeader: s.signatureHeader,
    signaturePrefix: s.signaturePrefix || undefined,
    eventHeader: s.eventHeader || undefined,
    eventPath: s.eventPath || undefined,
    matchEvents: s.matchEvents.length > 0 ? s.matchEvents : undefined,
    filters: s.filters.length > 0 ? s.filters : undefined,
    fields: s.fields.length > 0 ? s.fields : undefined,
    action: s.action,
    limits: s.maxPerMinute !== 10 ? { maxPerMinute: s.maxPerMinute } : undefined,
  }
}

async function copyToClipboard(text: string, label: string) {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(`${label} copied`)
  } catch {
    toast.error('Could not copy', {
      description: 'Clipboard access blocked — copy manually.',
    })
  }
}

interface Props {
  agentName: string
  onClose: () => void
  // When provided, the wizard opens in edit mode (#222): the Template
  // and Name steps are skipped, fields are pre-filled from the binding,
  // and submit fires PATCH instead of POST. Omit for the create flow.
  editing?: AgentInboundBinding
}

// EDIT_FIRST_STEP is the step the wizard jumps to in edit mode — the
// Template and Name steps are read-only by definition (#222), so the
// operator starts where they can actually make changes.
const EDIT_FIRST_STEP: StepIndex = 2

export function AddWebhookWizard({ agentName, onClose, editing }: Props) {
  const isEdit = editing !== undefined
  const titleId = useId()
  const [step, setStep] = useState<StepIndex>(isEdit ? EDIT_FIRST_STEP : 0)
  const [state, setState] = useState<WizardState>(
    isEdit ? stateFromBinding(editing) : emptyState(),
  )
  const [error, setError] = useState<string | null>(null)
  const [success, setSuccess] = useState<InboundCreateResponse | null>(null)

  const createBinding = useCreateInboundBinding()
  const updateBinding = useUpdateInboundBinding()
  const submitting = isEdit ? updateBinding.isPending : createBinding.isPending

  // Per-step validity gates the Next/Create buttons.
  const stepValid = useMemo<boolean>(() => {
    switch (step) {
      case 0:
        return state.template !== null
      case 1:
        return NAME_RE.test(state.name) && state.name.length <= 63
      case 2:
        return state.signatureHeader.trim().length > 0
      case 3:
      case 4:
        return true // both optional sections
      case 5:
        return state.action.trim().length > 0
      case 6:
        return state.maxPerMinute >= 1 && state.maxPerMinute <= 600
      case 7:
        // Review step is valid iff every prior step is valid. Mirror the
        // earlier checks rather than recursing through `stepValid` to avoid
        // a self-reference at this layer.
        return (
          state.template !== null &&
          NAME_RE.test(state.name) &&
          state.name.length <= 63 &&
          state.signatureHeader.trim().length > 0 &&
          state.action.trim().length > 0 &&
          state.maxPerMinute >= 1 &&
          state.maxPerMinute <= 600
        )
      default:
        return false
    }
  }, [step, state])

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      // While the success panel is up the user has nothing to undo — Escape
      // still closes. While a submit is in flight, swallow Escape so the user
      // doesn't lose their secret if the request is mid-flight.
      if (e.key === 'Escape' && !submitting) {
        onClose()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose, submitting])

  function update<K extends keyof WizardState>(key: K, value: WizardState[K]) {
    setState((prev) => ({ ...prev, [key]: value }))
    setError(null)
  }

  function next() {
    if (!stepValid) return
    if (step < 7) setStep((step + 1) as StepIndex)
  }

  function back() {
    // In edit mode, never go below EDIT_FIRST_STEP — Template and Name
    // are immutable so there's no editing to do back there.
    const floor: StepIndex = isEdit ? EDIT_FIRST_STEP : 0
    if (step > floor) setStep((step - 1) as StepIndex)
  }

  async function submit() {
    setError(null)
    try {
      if (isEdit) {
        await updateBinding.mutateAsync({
          name: agentName,
          bindingName: editing!.name,
          body: {
            signatureHeader: state.signatureHeader,
            signaturePrefix: state.signaturePrefix,
            eventHeader: state.eventHeader,
            eventPath: state.eventPath,
            matchEvents: state.matchEvents,
            filters: state.filters,
            fields: state.fields,
            action: state.action,
            limits: { maxPerMinute: state.maxPerMinute },
          },
        })
        // No setup-complete panel for edits — the existingSecret is
        // unchanged so there's nothing to surface. Toast fires from the
        // mutation's meta; close immediately.
        onClose()
        return
      }
      const limits =
        state.maxPerMinute !== 10 ? { maxPerMinute: state.maxPerMinute } : undefined
      const data = await createBinding.mutateAsync({
        name: agentName,
        body: {
          name: state.name,
          signatureHeader: state.signatureHeader,
          signaturePrefix: state.signaturePrefix || undefined,
          eventHeader: state.eventHeader || undefined,
          eventPath: state.eventPath || undefined,
          matchEvents: state.matchEvents.length > 0 ? state.matchEvents : undefined,
          filters: state.filters.length > 0 ? state.filters : undefined,
          fields: state.fields.length > 0 ? state.fields : undefined,
          action: state.action,
          limits,
        },
      })
      setSuccess(data)
    } catch (err) {
      setError(
        err instanceof Error
          ? err.message
          : isEdit
            ? 'Failed to update webhook'
            : 'Failed to create webhook',
      )
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div
        className="absolute inset-0 bg-surface-sunken/60 backdrop-blur-sm"
        onClick={submitting ? undefined : onClose}
      />
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className="relative z-10 flex max-h-[90vh] w-full max-w-2xl flex-col overflow-hidden rounded-xl border border-border-subtle bg-surface-raised shadow-xl"
      >
        <div className="flex items-center justify-between border-b border-border-subtle px-6 py-4">
          <h2 id={titleId} className="text-base font-semibold text-text-primary">
            {success
              ? 'Setup complete'
              : isEdit
                ? `Edit webhook — ${editing!.name}`
                : 'Add webhook'}
          </h2>
          <button
            type="button"
            onClick={onClose}
            disabled={submitting}
            aria-label="Close wizard"
            className="rounded-md p-1 text-text-muted hover:text-text-primary hover:bg-surface-overlay focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring disabled:opacity-40"
          >
            <X className="h-4 w-4" />
          </button>
        </div>

        <div className="flex-1 overflow-y-auto px-6 py-5">
          {success ? (
            <SetupComplete data={success} onClose={onClose} />
          ) : (
            <>
              <StepStrip current={step} onJump={(i) => setStep(i)} isEdit={isEdit} />
              <div className="mt-5 space-y-4">
                {step === 0 && (
                  <TemplateStep
                    selected={state.template}
                    onSelect={(t) => setState((prev) => applyTemplate(prev, t))}
                  />
                )}
                {step === 1 && (
                  <NameStep value={state.name} onChange={(v) => update('name', v)} />
                )}
                {step === 2 && (
                  <AuthStep
                    state={state}
                    update={update}
                    fromTemplate={state.template === 'github'}
                  />
                )}
                {step === 3 && (
                  <MatchingStep
                    matchEvents={state.matchEvents}
                    onMatchEventsChange={(v) => update('matchEvents', v)}
                    filters={state.filters}
                    onFiltersChange={(v) => update('filters', v)}
                  />
                )}
                {step === 4 && (
                  <FieldsStep
                    fields={state.fields}
                    onChange={(v) => update('fields', v)}
                  />
                )}
                {step === 5 && (
                  <ActionStep
                    state={state}
                    agentName={agentName}
                    onChange={(v) => update('action', v)}
                    bindingDraft={buildBindingDraft(state)}
                  />
                )}
                {step === 6 && (
                  <LimitsStep
                    value={state.maxPerMinute}
                    onChange={(v) => update('maxPerMinute', v)}
                  />
                )}
                {step === 7 && (
                  <ReviewStep state={state} agentName={agentName} isEdit={isEdit} />
                )}
              </div>

              {error && (
                <p className="mt-4 rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-xs text-danger">
                  {error}
                </p>
              )}
            </>
          )}
        </div>

        {!success && (
          <div className="flex items-center justify-between gap-2 border-t border-border-subtle px-6 py-4">
            <Button
              variant="ghost"
              size="sm"
              onClick={back}
              disabled={step === (isEdit ? EDIT_FIRST_STEP : 0) || submitting}
            >
              Back
            </Button>
            <div className="flex items-center gap-2">
              <Button
                variant="ghost"
                size="sm"
                onClick={onClose}
                disabled={submitting}
              >
                Cancel
              </Button>
              {step < 7 && (
                <Button variant="primary" size="sm" onClick={next} disabled={!stepValid}>
                  Next
                </Button>
              )}
              {step === 7 && (
                <Button
                  variant="primary"
                  size="sm"
                  onClick={() => void submit()}
                  loading={submitting}
                  disabled={!stepValid || submitting}
                >
                  {isEdit ? 'Save' : 'Create'}
                </Button>
              )}
            </div>
          </div>
        )}
      </div>
    </div>
  )
}

// ---- Step strip ----

function StepStrip({
  current,
  onJump,
  isEdit = false,
}: {
  current: StepIndex
  onJump: (i: StepIndex) => void
  // In edit mode the Template + Name steps are skipped from the strip —
  // operators can't change them via PATCH (#222).
  isEdit?: boolean
}) {
  return (
    <nav aria-label="Wizard steps" className="-mx-1 flex flex-wrap gap-1.5">
      {STEP_LABELS.map((label, i) => {
        if (isEdit && i < EDIT_FIRST_STEP) return null
        const active = i === current
        // Past steps are clickable; future steps require Next so the
        // per-step validity stays the gate.
        const disabled = i > current
        const cls = active
          ? 'bg-accent text-surface-base border-accent'
          : disabled
            ? 'bg-surface-overlay text-text-disabled border-border-default cursor-not-allowed'
            : 'bg-surface-overlay text-text-primary border-border-default hover:border-accent'
        return (
          <button
            key={label}
            type="button"
            disabled={disabled}
            aria-current={active ? 'step' : undefined}
            onClick={() => onJump(i as StepIndex)}
            className={`inline-flex shrink-0 items-center gap-1 rounded-full border px-2.5 py-0.5 text-[11px] font-medium transition-colors ${cls}`}
          >
            <span aria-hidden="true">{i + 1}</span>
            <span>{label}</span>
          </button>
        )
      })}
    </nav>
  )
}

// ---- Step 1: Template ----

function TemplateStep({
  selected,
  onSelect,
}: {
  selected: Template | null
  onSelect: (t: Template) => void
}) {
  return (
    <div>
      <h3 className="text-sm font-medium text-text-primary">
        How will external systems sign requests?
      </h3>
      <p className="mt-1 text-xs text-text-muted">
        Pick a starter template — it pre-fills common defaults. You can edit anything in the next
        steps.
      </p>
      <div className="mt-4 grid gap-3 sm:grid-cols-2">
        <TemplateCard
          title="GitHub webhook"
          subtitle="X-Hub-Signature-256 + X-GitHub-Event"
          description="Pre-fills the GitHub HMAC and event headers."
          active={selected === 'github'}
          onClick={() => onSelect('github')}
        />
        <TemplateCard
          title="Generic"
          subtitle="Bring-your-own header names"
          description="Empty defaults — fill in your provider's HMAC scheme yourself."
          active={selected === 'generic'}
          onClick={() => onSelect('generic')}
        />
      </div>
      <p className="mt-3 text-[11px] text-text-muted">
        Templates pre-fill common defaults; everything is editable in later steps.
      </p>
    </div>
  )
}

function TemplateCard({
  title,
  subtitle,
  description,
  active,
  onClick,
}: {
  title: string
  subtitle: string
  description: string
  active: boolean
  onClick: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={`rounded-xl border p-4 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring ${
        active
          ? 'border-accent bg-accent/10'
          : 'border-border-subtle bg-surface-base hover:border-border-default'
      }`}
    >
      <div className="text-sm font-medium text-text-primary">{title}</div>
      <div className="mt-1 font-mono text-[11px] text-text-muted">{subtitle}</div>
      <div className="mt-2 text-xs text-text-secondary">{description}</div>
    </button>
  )
}

// ---- Step 2: Name ----

function NameStep({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const valid = value === '' || (NAME_RE.test(value) && value.length <= 63)
  return (
    <div>
      <h3 className="text-sm font-medium text-text-primary">Name</h3>
      <p className="mt-1 text-xs text-text-muted">
        Used in the URL: <span className="font-mono">/webhooks/inbound/&lt;agent&gt;/&lt;name&gt;</span>.
        Lowercase letters, digits, hyphens. 1–63 chars.
      </p>
      <input
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="github-prs"
        autoFocus
        className="mt-2 w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm font-mono text-text-primary placeholder-text-disabled focus:border-accent focus:outline-none"
      />
      {!valid && (
        <p className="mt-1 text-xs text-danger">
          Must match ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ (1–63 chars).
        </p>
      )}
    </div>
  )
}

// ---- Step 3: Auth ----

function AuthStep({
  state,
  update,
  fromTemplate,
}: {
  state: WizardState
  update: <K extends keyof WizardState>(k: K, v: WizardState[K]) => void
  fromTemplate: boolean
}) {
  return (
    <div className="space-y-3">
      <h3 className="text-sm font-medium text-text-primary">Authentication</h3>
      {fromTemplate && (
        <p className="text-[11px] text-text-muted">
          Pre-filled from the GitHub template — change anything that doesn&rsquo;t fit.
        </p>
      )}
      <LabeledInput
        label="Signature header"
        required
        value={state.signatureHeader}
        onChange={(v) => update('signatureHeader', v)}
        placeholder="X-Hub-Signature-256"
        mono
      />
      <LabeledInput
        label="Signature prefix"
        value={state.signaturePrefix}
        onChange={(v) => update('signaturePrefix', v)}
        placeholder="sha256="
        mono
        help="Stripped from the header before hex-decoding. Leave blank for bare hex."
      />
      <LabeledInput
        label="Event header"
        value={state.eventHeader}
        onChange={(v) => update('eventHeader', v)}
        placeholder="X-GitHub-Event"
        mono
      />
      <LabeledInput
        label="Event path (JSONPath)"
        value={state.eventPath}
        onChange={(v) => update('eventPath', v)}
        placeholder="$.action"
        mono
        help="Optional. Appended to the event header value with a dot, e.g. pull_request.opened."
      />
    </div>
  )
}

// ---- Step 4: Matching ----

function MatchingStep({
  matchEvents,
  onMatchEventsChange,
  filters,
  onFiltersChange,
}: {
  matchEvents: string[]
  onMatchEventsChange: (v: string[]) => void
  filters: AgentInboundFilter[]
  onFiltersChange: (v: AgentInboundFilter[]) => void
}) {
  const [draft, setDraft] = useState('')

  function commitDraft() {
    const v = draft.trim()
    if (!v) return
    if (!matchEvents.includes(v)) onMatchEventsChange([...matchEvents, v])
    setDraft('')
  }

  function removeEvent(idx: number) {
    onMatchEventsChange(matchEvents.filter((_, i) => i !== idx))
  }

  function addFilter() {
    onFiltersChange([...filters, { jsonPath: '', equals: '' }])
  }

  function updateFilter(i: number, patch: Partial<AgentInboundFilter>) {
    onFiltersChange(filters.map((f, idx) => (idx === i ? { ...f, ...patch } : f)))
  }

  function removeFilter(i: number) {
    onFiltersChange(filters.filter((_, idx) => idx !== i))
  }

  return (
    <div className="space-y-4">
      <div>
        <h3 className="text-sm font-medium text-text-primary">Matching</h3>
        <p className="mt-1 text-xs text-text-muted">
          Restrict which events trigger this binding. Empty = matches every event.
        </p>
      </div>
      <div>
        <label className="mb-1 block text-xs font-medium text-text-muted">Match events</label>
        <div className="flex flex-wrap items-center gap-1.5 rounded-lg border border-border-default bg-surface-overlay px-2 py-1.5">
          {matchEvents.map((ev, i) => (
            <span
              key={`${ev}-${i}`}
              className="inline-flex items-center gap-1 rounded-full bg-accent/15 px-2 py-0.5 text-xs text-text-primary"
            >
              <span className="font-mono">{ev}</span>
              <button
                type="button"
                aria-label={`Remove ${ev}`}
                className="text-text-muted hover:text-text-primary"
                onClick={() => removeEvent(i)}
              >
                <X className="h-3 w-3" />
              </button>
            </span>
          ))}
          <input
            type="text"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' || e.key === ',') {
                e.preventDefault()
                commitDraft()
              } else if (e.key === 'Backspace' && !draft && matchEvents.length > 0) {
                onMatchEventsChange(matchEvents.slice(0, -1))
              }
            }}
            onBlur={commitDraft}
            placeholder={matchEvents.length === 0 ? 'pull_request.opened, issues.opened, …' : ''}
            className="min-w-[8rem] flex-1 bg-transparent text-xs font-mono text-text-primary placeholder-text-disabled outline-none"
          />
        </div>
        <p className="mt-1 text-[11px] text-text-muted">
          Press Enter or comma to add. Backspace removes the last chip.
        </p>
      </div>

      <div>
        <div className="mb-1 flex items-center justify-between">
          <label className="block text-xs font-medium text-text-muted">Filters</label>
          <Button variant="ghost" size="sm" onClick={addFilter}>
            <Plus className="h-3.5 w-3.5" /> Add filter
          </Button>
        </div>
        {filters.length === 0 && (
          <p className="text-[11px] text-text-muted">
            No filters — every matched event passes through. Add a filter to require a specific
            JSONPath value.
          </p>
        )}
        <div className="space-y-2">
          {filters.map((f, i) => (
            <div
              key={i}
              className="rounded-lg border border-border-subtle bg-surface-base p-3"
            >
              <div className="grid gap-2 sm:grid-cols-2">
                <LabeledInput
                  label="JSONPath"
                  value={f.jsonPath}
                  onChange={(v) => updateFilter(i, { jsonPath: v })}
                  placeholder="$.repository.full_name"
                  mono
                  required
                />
                <LabeledInput
                  label="Equals"
                  value={f.equals ?? ''}
                  onChange={(v) =>
                    updateFilter(i, { equals: v || undefined })
                  }
                  placeholder="my-org/my-repo"
                  mono
                />
              </div>
              <div className="mt-2 flex items-center justify-between">
                <p className="text-[11px] text-text-muted">
                  Set Equals OR provide an `in` list via the API directly.
                </p>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => removeFilter(i)}
                  aria-label="Remove filter"
                >
                  <Trash2 className="h-3.5 w-3.5 text-danger" />
                </Button>
              </div>
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}

// ---- Step 5: Fields ----

function FieldsStep({
  fields,
  onChange,
}: {
  fields: AgentInboundField[]
  onChange: (v: AgentInboundField[]) => void
}) {
  function add() {
    onChange([...fields, { label: '', jsonPath: '' }])
  }

  function update(i: number, patch: Partial<AgentInboundField>) {
    onChange(fields.map((f, idx) => (idx === i ? { ...f, ...patch } : f)))
  }

  function remove(i: number) {
    onChange(fields.filter((_, idx) => idx !== i))
  }

  return (
    <div className="space-y-3">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h3 className="text-sm font-medium text-text-primary">Fields</h3>
          <p className="mt-1 text-xs text-text-muted">
            Operator-friendly labels surface in the rendered prompt. Each row extracts a value from
            the request body.
          </p>
        </div>
        <Button variant="ghost" size="sm" onClick={add}>
          <Plus className="h-3.5 w-3.5" /> Add field
        </Button>
      </div>
      {fields.length === 0 && (
        <p className="text-[11px] text-text-muted">
          No fields — the rendered envelope&rsquo;s data block will be empty. Add fields to
          surface request data to the agent.
        </p>
      )}
      <div className="space-y-2">
        {fields.map((f, i) => (
          <div key={i} className="rounded-lg border border-border-subtle bg-surface-base p-3">
            <div className="grid gap-2 sm:grid-cols-2">
              <LabeledInput
                label="Label"
                value={f.label}
                onChange={(v) => update(i, { label: v })}
                placeholder="PR title"
                required
              />
              <LabeledInput
                label="JSONPath"
                value={f.jsonPath}
                onChange={(v) => update(i, { jsonPath: v })}
                placeholder="$.pull_request.title"
                mono
                required
              />
              <LabeledInput
                label="Truncate (chars)"
                value={f.truncate ? String(f.truncate) : ''}
                onChange={(v) => {
                  const n = Number(v)
                  update(i, {
                    truncate: Number.isFinite(n) && n > 0 ? n : undefined,
                  })
                }}
                placeholder="200"
              />
              <LabeledInput
                label="Prefix"
                value={f.prefix ?? ''}
                onChange={(v) => update(i, { prefix: v || undefined })}
                placeholder="url: "
              />
            </div>
            <div className="mt-2 flex justify-end">
              <Button
                variant="ghost"
                size="sm"
                onClick={() => remove(i)}
                aria-label="Remove field"
              >
                <Trash2 className="h-3.5 w-3.5 text-danger" />
              </Button>
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

// ---- Step 6: Action ----

function ActionStep({
  state,
  agentName,
  onChange,
  bindingDraft,
}: {
  state: WizardState
  agentName: string
  onChange: (v: string) => void
  bindingDraft: AgentInboundBinding
}) {
  const preview = useMemo(() => {
    const lines: string[] = []
    lines.push(`[inbound:<request-id>] binding=${state.name || '<name>'} agent=${agentName}`)
    lines.push('data:')
    if (state.fields.length === 0) {
      lines.push('  (no fields)')
    } else {
      for (const f of state.fields) {
        lines.push(`  ${f.label || '<label>'}: <value>`)
      }
    }
    lines.push('action:')
    if (state.action.trim()) {
      for (const ln of state.action.split('\n')) {
        lines.push('  ' + ln)
      }
    } else {
      lines.push('  <action text>')
    }
    return lines.join('\n')
  }, [state.name, state.fields, state.action, agentName])

  const [showTest, setShowTest] = useState(false)

  return (
    <div className="space-y-3">
      <div>
        <h3 className="text-sm font-medium text-text-primary">Action</h3>
        <p className="mt-1 text-xs text-text-muted">
          The static instruction text rendered into the agent&rsquo;s prompt. No templating in V1
          — this preserves the data/instruction trust boundary.
        </p>
      </div>
      <textarea
        value={state.action}
        onChange={(e) => onChange(e.target.value)}
        rows={5}
        placeholder="Triage this PR. Skim the diff, reply with a short impact summary, and label it priority:high|low."
        className="w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm text-text-primary placeholder-text-disabled focus:border-accent focus:outline-none"
      />
      <div>
        <label className="mb-1 block text-xs font-medium text-text-muted">Envelope preview</label>
        <pre className="overflow-x-auto rounded-lg border border-border-subtle bg-surface-base p-3 text-xs font-mono text-text-secondary">
          {preview}
        </pre>
      </div>
      <div>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => setShowTest((v) => !v)}
          aria-expanded={showTest}
        >
          <FlaskConical className="h-3.5 w-3.5" />
          {showTest ? 'Hide test panel' : 'Test with payload'}
        </Button>
        {showTest && (
          <div className="mt-3 rounded-lg border border-border-subtle bg-surface-base p-3">
            <TestPayloadPanel agentName={agentName} bindingDraft={bindingDraft} />
          </div>
        )}
      </div>
    </div>
  )
}

// TestPayloadPanel is the in-wizard variant of InboundDebugger. It's
// intentionally simpler: no binding picker (the binding is the wizard's
// in-progress draft) and no manual-edit toggle. Result rendering reuses
// DebugResultsPanel so it looks identical to the standalone tool.
function TestPayloadPanel({
  agentName,
  bindingDraft,
}: {
  agentName: string
  bindingDraft: AgentInboundBinding
}) {
  const [payloadText, setPayloadText] = useState<string>('{\n  \n}')
  const [payloadError, setPayloadError] = useState<string | null>(null)
  const [headerRows, setHeaderRows] = useState<{ id: number; key: string; value: string }[]>([])
  const [result, setResult] = useState<InboundDebugResponse | null>(null)

  const debug = useInboundDebug()

  function validatePayload(text: string): { ok: true; value: unknown } | { ok: false; error: string } {
    try {
      return { ok: true, value: JSON.parse(text) }
    } catch (e) {
      return { ok: false, error: e instanceof Error ? e.message : 'Invalid JSON' }
    }
  }

  const payloadValid = useMemo(() => validatePayload(payloadText).ok, [payloadText])

  function addHeader() {
    setHeaderRows((rows) => [...rows, { id: Date.now() + rows.length, key: '', value: '' }])
  }

  function updateHeader(id: number, patch: Partial<{ key: string; value: string }>) {
    setHeaderRows((rows) => rows.map((r) => (r.id === id ? { ...r, ...patch } : r)))
  }

  function removeHeader(id: number) {
    setHeaderRows((rows) => rows.filter((r) => r.id !== id))
  }

  async function run() {
    const v = validatePayload(payloadText)
    if (!v.ok) {
      setPayloadError(v.error)
      return
    }
    setPayloadError(null)

    const headers: Record<string, string> = {}
    for (const r of headerRows) {
      const k = r.key.trim()
      if (!k) continue
      headers[k] = r.value
    }
    setResult(null)
    try {
      const data = await debug.mutateAsync({
        binding: bindingDraft,
        payload: v.value,
        headers: Object.keys(headers).length > 0 ? headers : undefined,
        agent: agentName,
      })
      setResult(data)
    } catch {
      // Toast surfaced by mutation cache.
    }
  }

  return (
    <div className="space-y-3">
      <div>
        <label className="mb-1 block text-xs font-medium text-text-muted" htmlFor="wizard-test-payload">
          Payload (JSON)
        </label>
        <textarea
          id="wizard-test-payload"
          value={payloadText}
          onChange={(e) => {
            setPayloadText(e.target.value)
            if (payloadError) setPayloadError(null)
          }}
          onBlur={() => {
            const v = validatePayload(payloadText)
            setPayloadError(v.ok ? null : v.error)
          }}
          rows={6}
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
          disabled={!payloadValid || debug.isPending}
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

// ---- Step 7: Limits ----

function LimitsStep({
  value,
  onChange,
}: {
  value: number
  onChange: (n: number) => void
}) {
  return (
    <div className="space-y-2">
      <h3 className="text-sm font-medium text-text-primary">Rate limits</h3>
      <p className="text-xs text-text-muted">
        Per-binding token-bucket rate limit. Default 10 — over this rate, requests get 429.
      </p>
      <LabeledInput
        label="Max per minute"
        value={String(value)}
        onChange={(v) => {
          const n = Number(v)
          onChange(Number.isFinite(n) ? Math.max(1, Math.min(600, Math.round(n))) : 1)
        }}
        placeholder="10"
        type="number"
      />
    </div>
  )
}

// ---- Step 8: Review ----

function ReviewStep({
  state,
  agentName,
  isEdit = false,
}: {
  state: WizardState
  agentName: string
  isEdit?: boolean
}) {
  return (
    <div className="space-y-3">
      <h3 className="text-sm font-medium text-text-primary">Review</h3>
      <p className="text-xs text-text-muted">
        {isEdit ? (
          <>
            Review your changes. Hit Save to apply them to <span className="font-mono">{state.name}</span> on agent <span className="font-mono">{agentName}</span>.
          </>
        ) : (
          <>
            Review the binding spec. Hit Create to declare it on agent <span className="font-mono">{agentName}</span>.
          </>
        )}
      </p>
      <dl className="grid gap-2 rounded-lg border border-border-subtle bg-surface-base p-3 text-xs sm:grid-cols-2">
        <ReviewRow label="Template">{state.template ?? '—'}</ReviewRow>
        <ReviewRow label="Name" mono>
          {state.name}
        </ReviewRow>
        <ReviewRow label="Signature header" mono>
          {state.signatureHeader}
        </ReviewRow>
        <ReviewRow label="Signature prefix" mono>
          {state.signaturePrefix || '—'}
        </ReviewRow>
        <ReviewRow label="Event header" mono>
          {state.eventHeader || '—'}
        </ReviewRow>
        <ReviewRow label="Event path" mono>
          {state.eventPath || '—'}
        </ReviewRow>
        <ReviewRow label="Match events">
          {state.matchEvents.length === 0 ? '— (any)' : state.matchEvents.join(', ')}
        </ReviewRow>
        <ReviewRow label="Filters">
          {state.filters.length === 0 ? '—' : `${state.filters.length} configured`}
        </ReviewRow>
        <ReviewRow label="Fields">
          {state.fields.length === 0 ? '—' : `${state.fields.length} configured`}
        </ReviewRow>
        <ReviewRow label="Max per minute">{state.maxPerMinute}</ReviewRow>
      </dl>
      <div>
        <label className="mb-1 block text-xs font-medium text-text-muted">Action text</label>
        <pre className="overflow-x-auto whitespace-pre-wrap rounded-lg border border-border-subtle bg-surface-base p-3 text-xs text-text-secondary">
          {state.action}
        </pre>
      </div>
    </div>
  )
}

function ReviewRow({
  label,
  children,
  mono,
}: {
  label: string
  children: React.ReactNode
  mono?: boolean
}) {
  return (
    <div className="flex justify-between gap-2">
      <dt className="shrink-0 text-text-muted">{label}</dt>
      <dd className={`text-right text-text-primary ${mono ? 'font-mono' : ''}`}>{children}</dd>
    </div>
  )
}

// ---- Setup-complete panel ----

function SetupComplete({
  data,
  onClose,
}: {
  data: InboundCreateResponse
  onClose: () => void
}) {
  const { binding, secret, url } = data
  return (
    <div className="space-y-4">
      <p className="text-xs text-warn">
        Copy the secret now — Kyber stores only a hash and cannot reveal it again.
      </p>
      {url && (
        <div>
          <label className="mb-1 block text-xs font-medium text-text-muted">Webhook URL</label>
          <div className="flex items-center gap-2 rounded-lg border border-border-default bg-surface-base p-3">
            <code className="flex-1 break-all font-mono text-xs text-text-primary">{url}</code>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => void copyToClipboard(url, 'URL')}
            >
              <Copy className="h-3.5 w-3.5" /> Copy
            </Button>
          </div>
        </div>
      )}
      {!url && (
        <p className="rounded-md border border-warn/40 bg-warn/10 px-3 py-2 text-xs text-warn">
          The control plane has no PublicURL configured — webhook URL not displayed. Configure
          publicUrl on the chart, then read the URL via{' '}
          <span className="font-mono">GET /api/v1/agents/&lt;name&gt;/inbound-bindings</span>.
        </p>
      )}
      <div>
        <label className="mb-1 block text-xs font-medium text-text-muted">Secret</label>
        <div className="rounded-lg border border-border-default bg-surface-base p-3">
          <pre className="overflow-x-auto whitespace-pre-wrap break-all font-mono text-xs text-text-primary">
            {secret}
          </pre>
        </div>
        <div className="mt-2">
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void copyToClipboard(secret, 'Secret')}
          >
            <Copy className="h-3.5 w-3.5" /> Copy secret
          </Button>
        </div>
      </div>
      <p className="text-[11px] text-text-muted">
        Binding <span className="font-mono">{binding.name}</span> is now declared on the agent.
        Once the controller reconciles, external systems posting to the URL above with the secret
        in <span className="font-mono">{binding.signatureHeader}</span> will trigger inbound
        prompts.
      </p>
      <div className="flex justify-end pt-1">
        <Button variant="primary" size="sm" onClick={onClose}>
          Done
        </Button>
      </div>
    </div>
  )
}

// ---- Shared helpers ----

interface LabeledInputProps {
  label: string
  value: string
  onChange: (v: string) => void
  placeholder?: string
  required?: boolean
  mono?: boolean
  help?: string
  type?: string
}

function LabeledInput({
  label,
  value,
  onChange,
  placeholder,
  required,
  mono,
  help,
  type = 'text',
}: LabeledInputProps) {
  return (
    <label className="block text-xs">
      <span className="mb-1 block font-medium text-text-muted">
        {label}
        {required && <span className="ml-0.5 text-danger">*</span>}
      </span>
      <input
        type={type}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        className={`w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm text-text-primary placeholder-text-disabled focus:border-accent focus:outline-none ${
          mono ? 'font-mono' : ''
        }`}
      />
      {help && <span className="mt-1 block text-[11px] text-text-muted">{help}</span>}
    </label>
  )
}
