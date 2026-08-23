import { useEffect, useMemo, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { usePrefixedPath } from '../lib/route-prefix'
import { ArrowLeft } from 'lucide-react'
import {
  useAgents,
  useComputeConfig,
  useCreateAgent,
  useMachines,
  usePutDiscordComms,
} from '../hooks/useAPI'
import { useUpgradeProgress } from '../hooks/useUpgradeProgress'
import { Button } from '../components/Button'
import { Card } from '../components/Card'
import { availableFromMachine, parseCpu, parseMemoryGi } from '../lib/machineTypes'
import { toKebabCase } from '../lib/names'
import { parseAuthorizationInput } from '../lib/oauth'
import { firstInvalidId, parseIdList } from '../components/CommsTab'
import type { PutDiscordCommsRequest } from '../lib/types'
import { WizardState, WizardSetter, initialWizardState } from '../components/wizard/types'
import { BasicsSection } from '../components/wizard/BasicsSection'
import { ResourcesSection } from '../components/wizard/ResourcesSection'
import { IdentitySection } from '../components/wizard/IdentitySection'
import { AuthSection } from '../components/wizard/AuthSection'
import { ReviewSection } from '../components/wizard/ReviewSection'
import { WizardSteps } from '../components/wizard/WizardSteps'
import { WIZARD_STEPS, earliestInvalidStep } from '../components/wizard/validation'
import {
  DEFAULT_IDENTITY_TEMPLATE,
  IDENTITY_REPO_SLUG_RE,
} from '../components/wizard/identity-utils'
import { ConfirmDialog } from '../components/ConfirmDialog'
import { useWizardKeyboardShortcuts } from '../components/wizard/keyboardShortcuts'

const MIN_STEP = 1
const MAX_STEP = 5

function clampToValidStep(requested: number, state: WizardState): number {
  if (!Number.isFinite(requested) || requested < MIN_STEP) return MIN_STEP
  if (requested > MAX_STEP) return MAX_STEP
  return Math.min(requested, earliestInvalidStep(state))
}

export function CreateAgent() {
  const navigate = useNavigate()
  const prefixed = usePrefixedPath()
  const createAgent = useCreateAgent()
  // Creating an agent while the control plane is mid-restart either fails or
  // produces a pod that is immediately rolled again by the upgrade.
  const { inFlight: upgradeInFlight } = useUpgradeProgress()
  // Discord is wired AFTER the agent exists, through the same endpoint the
  // Comms tab uses — so there is one implementation of "wire a channel"
  // rather than a second one hidden in the create path.
  const putDiscordComms = usePutDiscordComms()
  const { data: machines } = useMachines()
  const { data: agents } = useAgents()
  const { data: config } = useComputeConfig()
  const [state, setState] = useState<WizardState>(() => initialWizardState([]))
  const [fieldError, setFieldError] = useState<string | null>(null)
  const [dirty, setDirty] = useState(false)
  const [dialogOpen, setDialogOpen] = useState(false)

  const [searchParams, setSearchParams] = useSearchParams()
  const requestedStep = Number(searchParams.get('step') ?? String(MIN_STEP))
  const activeStep = clampToValidStep(requestedStep, state)

  // Deep-link guard — bounces ?step=N to the earliest invalid step when the URL
  // requests a step the user can't reach yet. Runs whenever the URL step changes
  // (mount included).
  useEffect(() => {
    const earliest = earliestInvalidStep(state)
    if (requestedStep > earliest) {
      setSearchParams({ step: String(earliest) }, { replace: true })
    }
    // `state` is intentionally NOT in the dep array. The guard only re-fires
    // on URL changes (mount + explicit navigation). Adding `state` would bounce
    // the URL while the user is filling in fields. Render-time clamping via
    // clampToValidStep handles the case where state advances without a URL change.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [requestedStep])

  const set: WizardSetter = function<K extends keyof WizardState>(key: K, value: WizardState[K]) {
    setState((prev) => ({ ...prev, [key]: value }))
    setFieldError(null)
    setDirty(true)
  }

  function jumpTo(stepId: number) {
    setSearchParams({ step: String(stepId) })
  }

  function back() {
    if (activeStep > MIN_STEP) setSearchParams({ step: String(activeStep - 1) })
  }

  function next() {
    if (activeStep < MAX_STEP && WIZARD_STEPS[activeStep - 1].isValid(state).ok) {
      setSearchParams({ step: String(activeStep + 1) })
    }
  }

  function handleCancel() {
    if (dirty) {
      setDialogOpen(true)
    } else {
      navigate(prefixed('/agents'))
    }
  }

  function handleConfirmDiscard() {
    navigate(prefixed('/agents'))
  }

  const selectedMachine = machines?.find((m) => m.id === state.machine) ?? null
  const machineAvailable = useMemo(() => {
    if (!selectedMachine) return null
    return availableFromMachine(selectedMachine, agents ?? [])
  }, [selectedMachine, agents])
  const newCpu = parseCpu(state.cpu)
  const newMem = parseMemoryGi(state.memory)
  const newDisk = parseMemoryGi(state.disk)
  // Disk is included in fitCheck since #129 PR-C — but only when the machine
  // actually surfaced an ephemeral-storage budget (machineAvailable.diskGi > 0).
  // Pre-PR-C clusters and machines with no node yet report diskGi=0, in which
  // case we skip the disk fit-check rather than block every agent creation
  // (operators on those clusters can't pick a smaller disk to make it fit).
  const fitCheckPasses = !state.machine || !selectedMachine || !machineAvailable
    ? true
    : newCpu <= machineAvailable.cpu &&
      newMem <= machineAvailable.memoryGi &&
      (machineAvailable.diskGi <= 0 || newDisk <= machineAvailable.diskGi)

  const currentStepValidation = WIZARD_STEPS[activeStep - 1].isValid(state)
  const currentStepValid = currentStepValidation.ok
  const currentStepReason = currentStepValidation.ok ? null : currentStepValidation.reason

  useWizardKeyboardShortcuts({
    enabled: !dialogOpen,
    onEsc: handleCancel,
    onEnter: () => {
      if (activeStep < MAX_STEP) {
        next()
      } else if (fitCheckPasses && !createAgent.isPending && !upgradeInFlight) {
        // On step 5 (Review), trigger form submit. fitCheckPasses + isPending
        // guard against double-submit and unfit-resource submission, and
        // upgradeInFlight keeps Enter from bypassing the disabled button.
        void submit({ preventDefault: () => {} } as unknown as React.FormEvent)
      }
    },
    isCurrentStepValid: currentStepValid,
  })

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (activeStep !== MAX_STEP) {
      // Defensive: form submit should only fire from the last step's button.
      return
    }
    try {
      let oauthCodeFinal: string | undefined
      let pkceVerifierFinal: string | undefined

      if (state.runtime !== 'codex' && state.authType === 'oauth') {
        const parsed = parseAuthorizationInput(state.oauthCode)
        if (!parsed) {
          setFieldError('Paste the authorization code Anthropic showed you')
          return
        }
        if (parsed.state && parsed.state !== state.pkceState) {
          setFieldError('State mismatch — authorize again')
          return
        }
        oauthCodeFinal = parsed.code
        pkceVerifierFinal = state.pkceVerifier
      }

      let identityRepo: { repo?: string; template?: string } | undefined
      if (state.identityRepoMode === 'template') {
        identityRepo = { template: DEFAULT_IDENTITY_TEMPLATE }
      } else if (state.identityRepoMode === 'existing') {
        if (!IDENTITY_REPO_SLUG_RE.test(state.identityRepoExisting)) {
          setFieldError('Identity repo must be in owner/repo format (e.g. matty-v/my-agent)')
          return
        }
        identityRepo = { repo: state.identityRepoExisting }
      }

      // Validate Discord before creating anything: a bad ID here should be an
      // inline fix, not an agent that exists with a half-configured channel.
      let discordBody: PutDiscordCommsRequest | undefined
	  const telegramAllowedUserIds = parseIdList(state.telegramAllowedUserIds)
	  if (state.telegramEnabled && telegramAllowedUserIds.length === 0) {
		setFieldError('Add at least one Telegram user ID — otherwise nobody could talk to the agent.')
		return
	  }
	  const badTelegramID = firstInvalidId(telegramAllowedUserIds)
	  if (badTelegramID) {
		setFieldError(`"${badTelegramID}" isn't a Telegram user ID. Ask @userinfobot for your numeric ID.`)
		return
	  }
      if (state.discordEnabled && (state.authType === 'oauth' || state.runtime === 'codex')) {
        const allowedUserIds = parseIdList(state.discordAllowedUserIds)
        const guildIds = parseIdList(state.discordGuildIds)
        const channelIds = parseIdList(state.discordChannelIds)
        if (!state.discordBotToken) {
          setFieldError('Enter the Discord bot token, or uncheck Discord.')
          return
        }
        if (allowedUserIds.length === 0) {
          setFieldError(
            'Add at least one Discord user ID — otherwise nobody could talk to the agent.',
          )
          return
        }
        const bad = firstInvalidId([...guildIds, ...channelIds, ...allowedUserIds])
        if (bad) {
          setFieldError(`"${bad}" isn't a Discord ID. In Discord, right-click → Copy ID.`)
          return
        }
        discordBody = {
          botToken: state.discordBotToken,
          guildIds,
          channelIds,
          allowedUserIds,
          mentionOnly: state.discordMentionOnly,
        }
      }

      await createAgent.mutateAsync({
        // Defense-in-depth strip: BasicsSection's onBlur sanitizes when the
        // user leaves the field, but if they advance via Enter (which doesn't
        // blur the input before unmount) state.name could still carry leading
        // or trailing hyphens. See #189.
        name: toKebabCase(state.name),
        machine: state.machine,
        runtime: state.runtime,
        resources: { cpu: state.cpu, memory: state.memory, disk: state.disk },
        identity: { soulDescription: state.soulDescription || undefined },
        identityRepo,
        secrets: {
          authType: state.authType,
          telegramEnabled: state.telegramEnabled,
          oauthCode: oauthCodeFinal,
          pkceVerifier: pkceVerifierFinal,
          pkceState: state.pkceState || undefined,
          anthropicApiKey: state.anthropicApiKey || undefined,
          openaiApiKey: state.runtime === 'codex' ? state.openaiApiKey || undefined : undefined,
          telegramBotToken: state.telegramBotToken || undefined,
		  telegramAllowedUserIds: state.telegramEnabled ? telegramAllowedUserIds : undefined,
        },
      })

      // The agent exists now. A Discord failure here must not read as "create
      // failed" — the agent is real and the operator can finish on its Comms
      // tab, so say exactly that instead of dumping them back at the form.
      if (discordBody) {
        try {
          await putDiscordComms.mutateAsync({ name: toKebabCase(state.name), body: discordBody })
        } catch (err) {
          setFieldError(
            `Agent created, but Discord setup failed: ${
              err instanceof Error ? err.message : 'unknown error'
            }. Finish it on the agent's Comms tab.`,
          )
          return
        }
      }

      navigate(prefixed(state.runtime === 'codex' && state.authType === 'oauth'
        ? `/agents/${toKebabCase(state.name)}`
        : '/agents'))
    } catch (err) {
      setFieldError(err instanceof Error ? err.message : 'Failed to create agent')
    }
  }

  return (
    <div>
      <div className="flex items-center gap-3 mb-6">
        <Button variant="ghost" size="sm" onClick={() => navigate(prefixed('/agents'))}>
          <ArrowLeft className="h-4 w-4" />
        </Button>
        <h1 className="text-xl font-bold text-text-primary">New Agent</h1>
      </div>

      <Card className="max-w-lg">
        <WizardSteps state={state} activeStep={activeStep} onStepClick={jumpTo} />

        <form onSubmit={(e) => void submit(e)} className="space-y-5">
          {/* `key={activeStep}` re-mounts this wrapper on every step change so
              the animate-fade-in keyframe re-fires. prefers-reduced-motion
              collapses the duration in index.css. */}
          <div key={activeStep} className="animate-fade-in">
            {activeStep === 1 && (
              <BasicsSection
                state={state}
                set={set}
                machines={machines ?? []}
                agents={agents ?? []}
              />
            )}
            {activeStep === 2 && (
              <ResourcesSection
                state={state}
                set={set}
                selectedMachine={selectedMachine}
                machineAvailable={machineAvailable}
              />
            )}
            {activeStep === 3 && <IdentitySection state={state} set={set} />}
            {activeStep === 4 && <AuthSection state={state} set={set} />}
            {activeStep === 5 && <ReviewSection state={state} onEdit={(stepId) => jumpTo(stepId)} />}
          </div>

          {fieldError && (
            <p className="text-sm text-danger bg-danger/10 rounded-lg px-3 py-2">{fieldError}</p>
          )}

          {/* When Next is disabled because the current step is incomplete,
              surface the validator's reason inline above the buttons. The
              reason text is owned by the per-step validator in
              wizard/validation.ts; it answers the implicit "why isn't Next
              clickable?" without requiring a click on the button to trigger
              validation. (#213 polish item.) */}
          {activeStep < MAX_STEP && currentStepReason && (
            <p
              role="status"
              data-testid="wizard-step-reason"
              className="text-xs text-text-muted"
            >
              {currentStepReason}
            </p>
          )}

          <div className="flex gap-3 pt-2">
            <Button
              type="button"
              variant="ghost"
              size="md"
              onClick={handleCancel}
            >
              Cancel
            </Button>
            {activeStep > MIN_STEP && (
              <Button type="button" variant="ghost" size="md" onClick={back}>
                Back
              </Button>
            )}
            {activeStep < MAX_STEP && (
              <Button
                type="button"
                variant="primary"
                size="md"
                onClick={next}
                disabled={!currentStepValid}
              >
                Next
              </Button>
            )}
            {activeStep === MAX_STEP && (
              <Button
                type="submit"
                variant="primary"
                size="md"
                loading={createAgent.isPending}
                disabled={!fitCheckPasses || createAgent.isPending || upgradeInFlight}
                title={
                  upgradeInFlight
                    ? 'An upgrade is in progress — the control plane is restarting. Wait for it to finish.'
                    : undefined
                }
              >
                Create Agent
              </Button>
            )}
          </div>
        </form>
      </Card>
      <ConfirmDialog
        open={dialogOpen}
        title="Discard changes?"
        message="You have unsaved changes to this agent. Discarding will return you to the agent list."
        confirmLabel="Discard"
        cancelLabel="Cancel"
        dangerous
        onConfirm={handleConfirmDiscard}
        onCancel={() => setDialogOpen(false)}
      />
    </div>
  )
}
