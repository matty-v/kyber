import { useEffect, useState } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import { usePrefixedPath } from '../lib/route-prefix'
import { AlertTriangle, ArrowLeft, Play, Square, RotateCcw, KeyRound, Cpu, Trash2, MoreHorizontal, Minimize2, ScrollText } from 'lucide-react'
import {
  useAgent,
  useStartAgent,
  useStopAgent,
  useRestartAgent,
  useRestartAgentSession,
  useCompactAgentSession,
  useForceNeedsAuthAgent,
  useSetAgentModel,
  useAgentModels,
  useSetAgentRuntimeVersion,
  useSetAgentResources,
  useDeleteAgent,
  useTokenUsage,
  useReauthorizeAgent,
  useComputeConfig,
  usePatchAgent,
  useSetSessionResume,
} from '../hooks/useAPI'
import { useEffectiveModelList } from '../lib/models'
import { StatusBadge } from '../components/StatusBadge'
import { SchedulingFailureBanner } from '../components/SchedulingFailureBanner'
import { SchedulingFailureBadge } from '../components/SchedulingFailureBadge'
import { AgentActivityBadge } from '../components/AgentActivityBadge'
import { Button } from '../components/Button'
import { Card } from '../components/Card'
import { ConfirmDialog } from '../components/ConfirmDialog'
import { agentActionConfirmMessage } from '../lib/agentMessages'
import { Skeleton } from '../components/Skeleton'
import { TokenUsageCard } from '../components/TokenUsage'
import { ActivityTab } from '../components/ActivityTab'
import { JobsTab } from '../components/JobsTab'
import { CommsTab } from '../components/CommsTab'
import { SkillsTab } from '../components/SkillsTab'
import { SecretsTab } from '../components/SecretsTab'
import { ShellTab } from '../components/ShellTab'
import { CodexDeviceAuthPanel } from '../components/CodexDeviceAuthPanel'
import { AgentTerminalPeek } from '../components/TerminalPeek'
import { WebhooksTab } from '../components/WebhooksTab'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import {
  isLifecycleKind,
  lifecycleActionEndpoint,
  lifecycleItemsInMore,
  sessionItemsInMore,
} from '../lib/design/agent-actions'
import { Tabs, TabsList, TabsTrigger, TabsContent } from '@/components/ui/tabs'
import { generatePkcePair } from '../lib/pkce'
import { parseAuthorizationInput } from '../lib/oauth'
import type { Agent, AgentPhase, AgentIdentityRepoStatus, AgentIdentityRepoPhase, AgentStatus, SetResourcesRequest } from '../lib/types'

const CLAUDE_CODE_CLIENT_ID = '9d1c250a-e61b-44d9-88ed-5944d1962f5e'
const OAUTH_REDIRECT_URI = 'https://platform.claude.com/oauth/code/callback'
const OAUTH_SCOPES = [
  'org:create_api_key',
  'user:profile',
  'user:inference',
  'user:sessions:claude_code',
  'user:mcp_servers',
  'user:file_upload',
].join(' ')

function buildAuthorizeUrl(challenge: string, state: string): string {
  const u = new URL('https://claude.ai/oauth/authorize')
  u.searchParams.set('code', 'true')
  u.searchParams.set('client_id', CLAUDE_CODE_CLIENT_ID)
  u.searchParams.set('response_type', 'code')
  u.searchParams.set('redirect_uri', OAUTH_REDIRECT_URI)
  u.searchParams.set('scope', OAUTH_SCOPES)
  u.searchParams.set('code_challenge', challenge)
  u.searchParams.set('code_challenge_method', 'S256')
  u.searchParams.set('state', state)
  return u.toString()
}

type ActionKind =
  | 'start'
  | 'stop'
  | 'restart' // pod-level roll (renamed "Restart pod" in the UI)
  | 'restart-session' // in-pod tmux kill + relaunch (#128)
  | 'compact-session' // in-session context compaction, pod and session both stay up
  | 'force-needs-auth' // operator-forced re-auth for a wedged agent (#395)
  | 'retry-startup' // NeedsAuth "try again with what you have" (kyber#26); POSTs /start
  | 'delete'
  | 'set-model'
  | 'set-runtime-version'
  | 'set-resources'
// Tab order per #125 refinement: Overview | Secrets | Jobs | Webhooks | Activity | Shell.
// Activity replaces the old top-level "logs" (Pod Boot Log) tab and absorbs
// the read-only tmux attach that used to live under Shell. Shell itself is
// now interactive-only (root-in-chroot default). Webhooks (#208) sits between
// Jobs and Activity — both are inbound-prompt surfaces (cron-driven vs
// webhook-driven respectively).
type Tab = 'overview' | 'comms' | 'skills' | 'secrets' | 'jobs' | 'webhooks' | 'activity' | 'shell'

function identityRepoPhaseBadgeClass(phase: AgentIdentityRepoPhase | undefined): string {
  switch (phase) {
    case 'Ready':
      return 'bg-success/20 text-success ring-1 ring-inset ring-success/30'
    case 'Pending':
      return 'bg-accent/20 text-accent ring-1 ring-inset ring-accent-ring'
    case 'Failed':
      return 'bg-danger/20 text-danger ring-1 ring-inset ring-danger/30'
    default:
      return 'bg-surface-overlay text-text-muted ring-1 ring-inset ring-border-default'
  }
}

function formatTimestamp(iso: string | undefined): string | null {
  if (!iso) return null
  try {
    return new Date(iso).toLocaleString(undefined, {
      dateStyle: 'short',
      timeStyle: 'short',
    })
  } catch {
    return iso
  }
}

// StatusCardBody renders the right-hand "Status" card body on the Agent
// detail Overview tab. Exported for AgentDetail.test.tsx so the empty-state
// branch and partial-field rendering can be tested in isolation (no
// component-tree boilerplate per case).
//
// kyber#355: before this fix the body was four optional rows with no
// empty-state, so every Running agent in v1.3.1 (where the controller
// wasn't writing pod-derived fields) rendered as a bare heading. The card
// now mirrors MachineDetail.tsx's pending-data wording style ("X will
// appear once …") for the no-data case so the empty card teaches "not
// yet available" instead of "feature broken."
export function StatusCardBody({ status }: { status: AgentStatus }) {
  const formattedStart = formatTimestamp(status.startTime)
  const hasAnyStatusDetail = Boolean(
    status.podName ||
      status.podIP ||
      status.nodeName ||
      formattedStart ||
      status.restartCount !== undefined ||
      status.message,
  )

  if (!hasAnyStatusDetail) {
    return (
      <p className="text-xs text-text-muted italic" data-testid="status-empty-state">
        Pod is starting — status details will appear once it&apos;s scheduled.
      </p>
    )
  }

  return (
    <dl className="space-y-2 text-sm">
      {status.podName && (
        <div className="flex justify-between gap-2">
          <dt className="text-text-muted shrink-0">Pod</dt>
          <dd className="text-text-primary font-mono text-xs truncate">{status.podName}</dd>
        </div>
      )}
      {status.podIP && (
        <div className="flex justify-between">
          <dt className="text-text-muted">Pod IP</dt>
          <dd className="text-text-primary font-mono text-xs">{status.podIP}</dd>
        </div>
      )}
      {status.nodeName && (
        <div className="flex justify-between gap-2">
          <dt className="text-text-muted shrink-0">Node</dt>
          <dd className="text-text-primary font-mono text-xs truncate">{status.nodeName}</dd>
        </div>
      )}
      {formattedStart && (
        <div className="flex justify-between">
          <dt className="text-text-muted">Started</dt>
          <dd className="text-text-primary text-xs">{formattedStart}</dd>
        </div>
      )}
      {status.restartCount !== undefined && (
        <div className="flex justify-between">
          <dt className="text-text-muted">Restarts</dt>
          <dd className={status.restartCount > 0 ? 'text-warn' : 'text-text-primary'}>
            {status.restartCount}
          </dd>
        </div>
      )}
      {status.message && (
        <div>
          <dt className="text-text-muted mb-1">Message</dt>
          <dd className="text-text-secondary text-xs">{status.message}</dd>
        </div>
      )}
    </dl>
  )
}

// MismatchBadges surfaces the two PR-E (kyber#379) safety-net
// conditions: RuntimeVersionMismatch and ModelUnsupported. Both render
// as warning-styled cards inline with the other agent-detail cards so
// operators see them without scrolling — these signals replace what
// used to be silent failures (R2-D2 incident class).
//
// Each badge clears within one reconcile cycle after the underlying
// signal resolves (controller logic at
// pkg/controllers/agent/reconciler.go:reconcileRuntimeStatusConditions).
// Returns null when neither condition is True, so a healthy agent
// surfaces nothing.
export function MismatchBadges({ agent }: { agent: Agent }) {
  const showMismatch = Boolean(agent.runtimeVersionMismatch)
  const showUnsupported = Boolean(agent.modelUnsupported)
  // kyber#674 — blocked-before-pod conditions. Unlike the two badges above,
  // these mean NO pod exists at all, so the agent shows a blank status and a
  // restart cannot help. Rendered first: they explain why everything else on
  // the page is empty.
  const showImageMissing = Boolean(agent.runtimeImageMissing)
  const showModelUnresolved = Boolean(agent.modelUnresolved)
  // Probe ran and failed for a reason NOT attributable to the model
  // (auth, network, unrecognized error): modelSupported is absent, but
  // the diagnostic is present. "Couldn't verify" must be visible here,
  // not only in the CRD — the canary regression was invisible precisely
  // on this surface. (A definite rejection renders the danger banner
  // above instead.)
  const showProbeInconclusive =
    !showUnsupported &&
    agent.runtimeVersion?.modelSupported !== false &&
    Boolean(agent.runtimeVersion?.modelProbeMessage)
  if (!showMismatch && !showUnsupported && !showImageMissing && !showModelUnresolved && !showProbeInconclusive) return null
  const installed = agent.runtimeVersion?.installedVersion
  const requested = agent.runtimeVersion?.requestedVersion
  return (
    <div className="space-y-2">
      {showImageMissing && (
        <Card className="border-danger/40 bg-danger/5">
          <div className="flex items-start gap-3">
            <AlertTriangle className="h-5 w-5 text-danger shrink-0 mt-0.5" aria-hidden="true" />
            <div className="space-y-1">
              <h2 className="text-sm font-semibold text-text-primary">
                This cluster can&apos;t run the <code className="font-mono">{agent.runtime}</code> runtime
              </h2>
              <p className="text-xs text-text-muted">
                {agent.blockedReason ??
                  'No container image is configured for it on this install, so no pod can be created and the agent will never start.'}{' '}
                This is an install-level fix, not an agent one — pin the runtime&apos;s image in the Helm
                values and it clears on its own. Restarting the agent will not help.
              </p>
            </div>
          </div>
        </Card>
      )}
      {showModelUnresolved && (
        <Card className="border-danger/40 bg-danger/5">
          <div className="flex items-start gap-3">
            <AlertTriangle className="h-5 w-5 text-danger shrink-0 mt-0.5" aria-hidden="true" />
            <div className="space-y-1">
              <h2 className="text-sm font-semibold text-text-primary">No model resolved</h2>
              <p className="text-xs text-text-muted">
                {agent.blockedReason ??
                  "This agent has no model set and the fleet default is empty, so the controller won't build a pod. Set a model on the agent, or set the fleet default in Settings."}
              </p>
            </div>
          </div>
        </Card>
      )}
      {showMismatch && (
        <Card className="border-warning/40 bg-warning/5">
          <div className="flex items-start gap-3">
            <AlertTriangle className="h-5 w-5 text-warning shrink-0 mt-0.5" aria-hidden="true" />
            <div className="space-y-1">
              <h2 className="text-sm font-semibold text-text-primary">Runtime version mismatch</h2>
              <p className="text-xs text-text-muted">
                {installed && requested ? (
                  <>
                    The agent is running harness version <code className="font-mono">{installed}</code>, but
                    requested <code className="font-mono">{requested}</code>. The boot-time install
                    likely failed and the pod fell back to the baked-in version. Fix the cause
                    (bad version string, registry outage) and restart the agent.
                  </>
                ) : (
                  <>The agent's installed harness version doesn't match what was requested. Restart the agent to re-attempt the install.</>
                )}
              </p>
            </div>
          </div>
        </Card>
      )}
      {showUnsupported && (
        <Card className="border-danger/40 bg-danger/5">
          <div className="flex items-start gap-3">
            <AlertTriangle className="h-5 w-5 text-danger shrink-0 mt-0.5" aria-hidden="true" />
            <div className="space-y-1">
              <h2 className="text-sm font-semibold text-text-primary">Model rejected by installed Claude Code</h2>
              <p className="text-xs text-text-muted">
                The pre-flight probe reported the configured model
                {agent.model ? <> (<code className="font-mono">{agent.model}</code>)</> : null}
                {' '}as rejected by the installed Claude Code{installed ? <> ({installed})</> : null}.
                {' '}Every turn will fail until this is fixed. Check the model id first (Change model on
                this page, or the fleet default in Settings — an agent with no model of its own inherits
                the fleet default); if the id is right, apply a newer Claude Code version
                (<code className="font-mono">spec.runtimeVersion</code> per-agent or the fleet
                {' '}<code className="font-mono">defaultRuntimeVersion</code>) and restart.
              </p>
              {agent.runtimeVersion?.modelProbeMessage ? (
                <p className="text-xs text-text-muted font-mono border-l-2 border-danger/40 pl-2">
                  {agent.runtimeVersion.modelProbeMessage}
                </p>
              ) : null}
            </div>
          </div>
        </Card>
      )}
      {showProbeInconclusive && (
        <Card className="border-warning/40 bg-warning/5">
          <div className="flex items-start gap-3">
            <AlertTriangle className="h-5 w-5 text-warning shrink-0 mt-0.5" aria-hidden="true" />
            <div className="space-y-1">
              <h2 className="text-sm font-semibold text-text-primary">Model check inconclusive</h2>
              <p className="text-xs text-text-muted">
                The boot-time model probe failed for a reason that does not look like a model
                rejection (network, auth, or an unrecognized error), so the platform cannot confirm
                the configured model works. If the agent answers normally, this is transient noise
                from boot; if turns are failing, the output below is the lead.
              </p>
              <p className="text-xs text-text-muted font-mono border-l-2 border-warning/40 pl-2">
                {agent.runtimeVersion?.modelProbeMessage}
              </p>
            </div>
          </div>
        </Card>
      )}
    </div>
  )
}

// SessionResumeCard is the kyber#118 per-agent toggle. Presentational and
// exported (like StatusCardBody / MismatchBadges) so tests can exercise it
// without the page's data providers.
export function SessionResumeCard({
  enabled,
  pending,
  onChange,
}: {
  enabled: boolean
  pending: boolean
  onChange: (enabled: boolean) => void
}) {
  return (
    <Card>
      <h2 className="text-sm font-medium text-text-primary mb-2">Session resume</h2>
      <label className="flex items-start gap-2 text-sm text-text-primary">
        <input
          type="checkbox"
          className="mt-0.5"
          checked={enabled}
          disabled={pending}
          onChange={(e) => onChange(e.target.checked)}
        />
        <span>
          Resume the previous session after an unexpected restart
          <span className="mt-0.5 block text-xs text-text-muted">
            Applies when the pod is recreated, preempted, or crashes. An
            intentional session restart still starts fresh. Saving marks this
            agent for restart; the setting lands the next time its pod starts.
          </span>
        </span>
      </label>
    </Card>
  )
}

function IdentityRepoCard({ data }: { data: AgentIdentityRepoStatus }) {
  return (
    <Card>
      <h2 className="text-sm font-medium text-text-muted mb-3">Identity repo</h2>
      <dl className="space-y-2 text-sm">
        {data.phase && (
          <div className="flex justify-between items-center">
            <dt className="text-text-muted">Phase</dt>
            <dd>
              <span
                className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ${identityRepoPhaseBadgeClass(data.phase)}`}
              >
                {data.phase}
              </span>
            </dd>
          </div>
        )}
        {data.repo && (
          <div className="flex justify-between gap-2">
            <dt className="text-text-muted shrink-0">Repo</dt>
            <dd className="truncate">
              <a
                href={`https://github.com/${data.repo}`}
                target="_blank"
                rel="noopener noreferrer"
                className="text-accent hover:text-accent font-mono text-xs underline-offset-2 hover:underline"
              >
                {data.repo}
              </a>
            </dd>
          </div>
        )}
        {data.tokenExpiresAt && (
          <div className="flex justify-between gap-2">
            <dt className="text-text-muted shrink-0">Token expires</dt>
            <dd className="text-text-primary text-xs">{formatTimestamp(data.tokenExpiresAt)}</dd>
          </div>
        )}
        {data.phase === 'Failed' && data.message && (
          <div>
            <dt className="text-text-muted mb-1">Message</dt>
            <dd className="text-danger text-xs">{data.message}</dd>
          </div>
        )}
      </dl>
    </Card>
  )
}

// LifecycleMenuItems renders the per-phase lifecycle actions inside the agent
// detail "More" dropdown. Extracted + exported (like StatusCardBody above) so
// the actions surfaced per phase can be tested in isolation without mounting
// the whole detail page — kyber#599: a crashed agent (Failed/MemoryExhausted)
// must surface the WORKING recovery, Start (desiredPhase=Running), and no
// longer the no-op Restart pod (which only ever fired from Running). The
// per-phase set itself is owned by lifecycleItemsInMore; this just maps each
// applicable kind to its labelled menu item.
export function LifecycleMenuItems({
  phase,
  onSelect,
}: {
  phase: AgentPhase
  onSelect: (kind: ActionKind) => void
}) {
  const items = lifecycleItemsInMore(phase)
  return (
    <>
      {items.includes('start') && (
        <DropdownMenuItem onSelect={() => onSelect('start')}>
          <Play className="h-3.5 w-3.5" />
          Start
        </DropdownMenuItem>
      )}
      {items.includes('stop') && (
        <DropdownMenuItem onSelect={() => onSelect('stop')}>
          <Square className="h-3.5 w-3.5" />
          Stop
        </DropdownMenuItem>
      )}
      {items.includes('restart') && (
        <DropdownMenuItem onSelect={() => onSelect('restart')}>
          <RotateCcw className="h-3.5 w-3.5" />
          Restart pod
        </DropdownMenuItem>
      )}
      {items.includes('force-needs-auth') && (
        <DropdownMenuItem onSelect={() => onSelect('force-needs-auth')}>
          <KeyRound className="h-3.5 w-3.5" />
          Require re-auth
        </DropdownMenuItem>
      )}
      {/* kyber#26: the only lifecycle control a NeedsAuth agent gets. Labelled
          for what the operator sees happen (the pod is rebuilt) rather than for
          the endpoint it calls (/start) — see agent-actions.ts for why it is
          not the 'restart' kind. Sits beside, and does not replace, the
          Re-authorize panel further down the page. */}
      {items.includes('retry-startup') && (
        <DropdownMenuItem onSelect={() => onSelect('retry-startup')}>
          <RotateCcw className="h-3.5 w-3.5" />
          Restart pod
        </DropdownMenuItem>
      )}
    </>
  )
}

export function AgentDetail() {
  const { name = '' } = useParams<{ name: string }>()
  const navigate = useNavigate()
  const prefixed = usePrefixedPath()
  const { data: agent, isLoading, error } = useAgent(name)
  const tokenUsage = useTokenUsage(name, agent?.phase === 'Running')
  const { data: computeConfig } = useComputeConfig()
  // The public /available catalog supplies harness versions. Model choices
  // come separately from this agent's authenticated provider catalog.
  const effective = useEffectiveModelList(agent?.runtime)
  const [pending, setPending] = useState<ActionKind | null>(null)
  const [activeTab, setActiveTab] = useState<Tab>('overview')
  const [newModel, setNewModel] = useState('')
  const [newRuntimeVersion, setNewRuntimeVersion] = useState('')
  const [newCPU, setNewCPU] = useState('')
  const [newMemory, setNewMemory] = useState('')
  const [startupPrompt, setStartupPrompt] = useState('')
  const agentModels = useAgentModels(name, pending === 'set-model')

  useEffect(() => {
    if (pending === 'set-model' && !newModel && agentModels.data?.models[0]) {
      const current = agentModels.data.models.find((model) => model.id === (agent?.currentModel || agent?.model))
      setNewModel(current?.id ?? agentModels.data.models[0].id)
    }
  }, [pending, newModel, agentModels.data, agent?.model, agent?.currentModel])

  // Re-authorize flow state
  const [reauthVerifier, setReauthVerifier] = useState('')
  const [reauthState, setReauthState] = useState('')
  const [reauthCode, setReauthCode] = useState('')
  const [reauthError, setReauthError] = useState<string | null>(null)
  const [reauthSuccess, setReauthSuccess] = useState(false)

  const startAgent = useStartAgent()
  const stopAgent = useStopAgent()
  const restartAgent = useRestartAgent()
  const restartAgentSession = useRestartAgentSession()
  const compactAgentSession = useCompactAgentSession()
  const forceNeedsAuthAgent = useForceNeedsAuthAgent()
  const setAgentModel = useSetAgentModel()
  const setAgentRuntimeVersion = useSetAgentRuntimeVersion()
  const setAgentResources = useSetAgentResources()
  const patchAgent = usePatchAgent()
  const setSessionResume = useSetSessionResume()
  const deleteAgent = useDeleteAgent()
  const reauthorizeAgent = useReauthorizeAgent()

  useEffect(() => {
    setStartupPrompt(agent?.startupPrompt ?? '')
  }, [agent?.startupPrompt])

  const isActing =
    startAgent.isPending ||
    stopAgent.isPending ||
    restartAgent.isPending ||
    restartAgentSession.isPending ||
    compactAgentSession.isPending ||
    forceNeedsAuthAgent.isPending ||
    setAgentModel.isPending ||
    setAgentRuntimeVersion.isPending ||
    setAgentResources.isPending ||
    deleteAgent.isPending

  async function executeAction() {
    if (!pending) return
    try {
      // Lifecycle kinds resolve to their API sub-action through
      // lifecycleActionEndpoint, which owns the kind→endpoint mapping beside
      // the per-phase rules (kyber#26). 'retry-startup' resolves to 'start'
      // there — reading it through the helper instead of repeating the mapping
      // here is what stops a menu item and its handler drifting into a dead
      // button. Non-lifecycle kinds (sessions, setters, delete) pass through.
      const action = isLifecycleKind(pending) ? lifecycleActionEndpoint(pending) : pending
      if (action === 'start') await startAgent.mutateAsync(name)
      if (action === 'stop') await stopAgent.mutateAsync(name)
      if (action === 'restart') await restartAgent.mutateAsync(name)
      if (action === 'restart-session') await restartAgentSession.mutateAsync(name)
      if (action === 'compact-session') await compactAgentSession.mutateAsync(name)
      if (action === 'force-needs-auth') await forceNeedsAuthAgent.mutateAsync(name)
      if (pending === 'set-model' && newModel) {
        await setAgentModel.mutateAsync({ name, model: newModel })
        setNewModel('')
      }
      if (pending === 'set-runtime-version') {
        // Empty input is a deliberate clear (revert to fleet default).
        await setAgentRuntimeVersion.mutateAsync({ name, runtimeVersion: newRuntimeVersion })
        setNewRuntimeVersion('')
      }
      if (pending === 'set-resources') {
        const body: SetResourcesRequest = {}
        if (newCPU) body.cpu = newCPU
        if (newMemory) body.memory = newMemory
        if (!body.cpu && !body.memory) return
        await setAgentResources.mutateAsync({ name, body })
        setNewCPU('')
        setNewMemory('')
      }
      if (pending === 'delete') {
        await deleteAgent.mutateAsync(name)
        navigate(prefixed('/agents'))
        return
      }
    } finally {
      setPending(null)
    }
  }

  if (isLoading) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-8 w-48 rounded" />
        <Skeleton className="h-40 rounded-xl" />
      </div>
    )
  }

  if (error || !agent) {
    return (
      <div className="rounded-lg border border-danger/40 bg-danger-muted p-4 text-sm text-danger">
        {error?.message ?? 'Agent not found'}
      </div>
    )
  }

  const tabs: { id: Tab; label: string }[] = [
    { id: 'overview', label: 'Overview' },
    { id: 'comms', label: 'Comms' },
    { id: 'skills', label: 'Skills' },
    { id: 'secrets', label: 'Secrets' },
    { id: 'jobs', label: 'Jobs' },
    { id: 'webhooks', label: 'Webhooks' },
    { id: 'activity', label: 'Activity' },
    { id: 'shell', label: 'Shell' },
  ]

  // Per #128 amendment: Restart session is the primary header action when
  // the agent is live. Other lifecycle actions (Start/Stop/Restart
  // pod) move into the More dropdown — filtered per the existing
  // lifecycleItemsInMore helper so inapplicable items (e.g. Start on a
  // Running agent) stay hidden.
  // Section occupancy. Each section renders its label only when it has at
  // least one item — a header with nothing under it reads as a bug, and on
  // non-Running phases the agent-actions section is legitimately empty.
  // Both per-phase sets are owned by lib/design/agent-actions so they can be
  // tested without mounting this page.
  const sessionActions = sessionItemsInMore(agent.phase)
  const hasAgentActions = sessionActions.length > 0
  const hasPodActions = lifecycleItemsInMore(agent.phase).length > 0

  return (
    <div>
      <div className="flex items-center gap-3 mb-4">
        <Button variant="ghost" size="sm" onClick={() => navigate(prefixed('/agents'))}>
          <ArrowLeft className="h-4 w-4" />
        </Button>
        <h1 className="min-w-0 truncate text-xl font-bold text-text-primary">{agent.id}</h1>
        <StatusBadge phase={agent.phase} />
        <SchedulingFailureBadge agent={agent} />
        <AgentActivityBadge agent={agent} />
        <div className="ml-auto flex items-center gap-2">
          <Button variant="secondary" size="sm" onClick={() => navigate(prefixed(`/logs?agent=${encodeURIComponent(name)}`))}>
            <ScrollText className="h-4 w-4" /> Logs
          </Button>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button
                variant="secondary"
                size="sm"
                disabled={isActing}
                aria-label="More actions"
              >
                <MoreHorizontal className="h-4 w-4" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              {/* Three sections, ordered by blast radius: things that touch
                  only the conversation, then things that touch the pod, then
                  things that change the agent's definition. Each label is
                  conditional on its section having items — a header with
                  nothing under it reads as a bug, and on non-Running phases
                  the agent-actions section is legitimately empty. */}
              {hasAgentActions && (
                <>
                  <DropdownMenuLabel>Agent actions</DropdownMenuLabel>
                  {sessionActions.includes('compact-session') && (
                    <DropdownMenuItem
                      onSelect={() => setPending('compact-session')}
                      aria-label="Compact session"
                    >
                      <Minimize2 className="h-3.5 w-3.5" />
                      Compact session
                    </DropdownMenuItem>
                  )}
                  {sessionActions.includes('restart-session') && (
                    <DropdownMenuItem
                      onSelect={() => setPending('restart-session')}
                      aria-label="Restart session"
                    >
                      <RotateCcw className="h-3.5 w-3.5" />
                      Restart session
                    </DropdownMenuItem>
                  )}
                </>
              )}
              {hasPodActions && (
                <>
                  {hasAgentActions && <DropdownMenuSeparator />}
                  <DropdownMenuLabel>Pod actions</DropdownMenuLabel>
                  <LifecycleMenuItems phase={agent.phase} onSelect={setPending} />
                </>
              )}
              {(hasAgentActions || hasPodActions) && <DropdownMenuSeparator />}
              <DropdownMenuLabel>Agent configuration</DropdownMenuLabel>
              <DropdownMenuItem
                onSelect={() => {
                  setNewModel(agent.currentModel || agent.model)
                  setPending('set-model')
                }}
              >
                <Cpu className="h-3.5 w-3.5" />
                Set Model
              </DropdownMenuItem>
              <DropdownMenuItem
                onSelect={() => {
                  setNewRuntimeVersion(agent.runtimeVersion?.requestedVersion ?? '')
                  setPending('set-runtime-version')
                }}
              >
                <Cpu className="h-3.5 w-3.5" />
                Set Harness Version
              </DropdownMenuItem>
              <DropdownMenuItem
                onSelect={() => {
                  setNewCPU(agent.resources.cpu)
                  setNewMemory(agent.resources.memory)
                  setPending('set-resources')
                }}
              >
                <Cpu className="h-3.5 w-3.5" />
                Set Resources
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem variant="danger" onSelect={() => setPending('delete')}>
                <Trash2 className="h-3.5 w-3.5" />
                Delete
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      {/* Actions */}

      {/* Tabs */}
      <Tabs value={activeTab} onValueChange={(v) => setActiveTab(v as Tab)}>
        <TabsList className="mb-4 w-full overflow-x-auto">
          {tabs.map((tab) => (
            <TabsTrigger key={tab.id} value={tab.id}>
              {tab.label}
            </TabsTrigger>
          ))}
        </TabsList>

        <TabsContent value="overview">
          <div className="space-y-4">
          <SchedulingFailureBanner agent={agent} />
          {agent.runtime === 'codex' && agent.authType === 'oauth' &&
            (agent.phase === 'Starting' || agent.phase === 'NeedsAuth') && (
            <CodexDeviceAuthPanel name={name} phase={agent.phase} />
          )}
          {agent.phase === 'NeedsAuth' && !(agent.runtime === 'codex' && agent.authType === 'oauth') && (
            <Card className="border-warn/40 bg-warn-muted">
              <h2 className="text-sm font-semibold text-warn mb-1">Re-authorization required</h2>
              <p className="text-xs text-warn/80 mb-3">
                This agent&apos;s OAuth credentials have expired. Re-authorize to resume.
              </p>
              {!reauthSuccess ? (
                <div className="space-y-3">
                  <div>
                    <Button
                      type="button"
                      variant="secondary"
                      size="sm"
                      onClick={async () => {
                        const { verifier, challenge } = await generatePkcePair()
                        const state = crypto.randomUUID()
                        setReauthVerifier(verifier)
                        setReauthState(state)
                        setReauthCode('')
                        setReauthError(null)
                        window.open(buildAuthorizeUrl(challenge, state), '_blank', 'noopener')
                      }}
                    >
                      {reauthVerifier ? 'Re-authorize again' : 'Re-authorize'}
                    </Button>
                    <p className="mt-1 text-xs text-text-muted">
                      Opens Anthropic in a new tab. Sign in and authorize. Anthropic will show a
                      page with your authorization code — copy it and paste below.
                    </p>
                  </div>
                  {reauthVerifier && (
                    <div className="space-y-2">
                      <label className="block text-xs font-medium text-text-muted">
                        Authorization code
                      </label>
                      <input
                        type="text"
                        value={reauthCode}
                        onChange={(e) => {
                          setReauthCode(e.target.value)
                          setReauthError(null)
                        }}
                        placeholder="Paste the code Anthropic shows you"
                        className="w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm text-text-primary placeholder-text-disabled focus:border-accent focus:outline-none"
                      />
                      <Button
                        type="button"
                        variant="primary"
                        size="sm"
                        loading={reauthorizeAgent.isPending}
                        disabled={!reauthCode.trim()}
                        onClick={async () => {
                          setReauthError(null)
                          const parsed = parseAuthorizationInput(reauthCode)
                          if (!parsed) {
                            setReauthError('Paste the authorization code Anthropic showed you')
                            return
                          }
                          if (parsed.state && parsed.state !== reauthState) {
                            setReauthError('State mismatch — authorize again')
                            return
                          }
                          try {
                            await reauthorizeAgent.mutateAsync({
                              name,
                              body: {
                                oauthCode: parsed.code,
                                pkceVerifier: reauthVerifier,
                                state: reauthState,
                              },
                            })
                            setReauthSuccess(true)
                          } catch (err) {
                            setReauthError(err instanceof Error ? err.message : 'Re-authorization failed')
                          }
                        }}
                      >
                        Submit
                      </Button>
                      {reauthError && (
                        <p className="text-xs text-danger bg-danger/10 rounded-lg px-3 py-2">
                          {reauthError}
                        </p>
                      )}
                    </div>
                  )}
                </div>
              ) : (
                <p className="text-xs text-success bg-success/10 rounded-lg px-3 py-2">
                  Re-authorized successfully. The agent will transition out of NeedsAuth shortly.
                </p>
              )}
            </Card>
          )}
          <AgentTerminalPeek agentName={name} hasPod={Boolean(agent.status.podName)} />
          <Card>
            <h2 className="text-sm font-medium text-text-primary mb-2">Startup prompt</h2>
            <p className="text-xs text-text-muted mb-3">
              Sent as the first user turn on every new session. Saving marks this agent for restart; it does not interrupt the live session.
            </p>
            <textarea
              value={startupPrompt}
              onChange={(e) => setStartupPrompt(e.target.value)}
              maxLength={32768}
              rows={6}
              className="w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm text-text-primary"
              placeholder="No startup prompt configured"
            />
            <div className="mt-2 flex items-center justify-between gap-3">
              <span className="text-xs text-text-muted">{startupPrompt.length.toLocaleString()} / 32,768</span>
              <Button
                size="sm"
                loading={patchAgent.isPending}
                disabled={patchAgent.isPending || startupPrompt === (agent.startupPrompt ?? '')}
                onClick={() => patchAgent.mutate({ name, startupPrompt })}
              >
                Save prompt
              </Button>
            </div>
          </Card>
          <SessionResumeCard
            enabled={agent.sessionResume ?? false}
            pending={setSessionResume.isPending}
            onChange={(enabled) => setSessionResume.mutate({ name, sessionResume: enabled })}
          />
          <TokenUsageCard data={tokenUsage.data} isLoading={tokenUsage.isLoading} />
          {agent.identityRepo && <IdentityRepoCard data={agent.identityRepo} />}
          <MismatchBadges agent={agent} />
          <div className="grid gap-4 sm:grid-cols-2">
          <Card>
            <h2 className="text-sm font-medium text-text-muted mb-3">Spec</h2>
            <dl className="space-y-2 text-sm">
              <div className="flex justify-between">
                <dt className="text-text-muted">Model</dt>
                <dd className="text-right text-text-primary font-mono text-xs">
                  {agent.currentModel || agent.model || 'Harness default'}
                  {!agent.model && agent.currentModel && <span className="block font-sans text-[10px] text-text-muted">harness default</span>}
                </dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-text-muted">Machine</dt>
                <dd className="text-text-primary">{agent.machine}</dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-text-muted">Runtime</dt>
                <dd className="text-text-primary">
                  {agent.runtime}
                  {agent.runtimeVersion?.installedVersion && (
                    <span className="text-text-muted font-mono text-xs ml-2">
                      {agent.runtimeVersion.installedVersion}
                    </span>
                  )}
                </dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-text-muted">CPU</dt>
                <dd className="text-text-primary font-mono text-xs">{agent.resources.cpu}</dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-text-muted">Memory</dt>
                <dd className="text-text-primary font-mono text-xs">{agent.resources.memory}</dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-text-muted">Disk</dt>
                <dd className="text-text-primary font-mono text-xs">{agent.resources.disk}</dd>
              </div>
            </dl>
          </Card>
          <Card>
            <h2 className="text-sm font-medium text-text-muted mb-3">Status</h2>
            <StatusCardBody status={agent.status} />
          </Card>
          </div>
        </div>
        </TabsContent>

        <TabsContent value="comms">
          {/* Restart pod is routed through the existing confirm flow so the
              "apply this change" path is the same one the More menu uses —
              including its warning that the live session ends. */}
          <CommsTab agentName={name} onRestartPod={() => setPending('restart')} />
        </TabsContent>

        <TabsContent value="skills">
          {/* Read-only: skills are managed by talking to the agent. */}
          <SkillsTab agentName={name} />
        </TabsContent>

        <TabsContent value="secrets">
          <SecretsTab agentName={name} />
        </TabsContent>

        <TabsContent value="jobs">
          <JobsTab agentName={name} />
        </TabsContent>

        <TabsContent value="webhooks">
          <WebhooksTab agentName={name} />
        </TabsContent>

        <TabsContent value="activity">
          <ActivityTab agentName={name} />
        </TabsContent>

        <TabsContent value="shell">
          <ShellTab agentName={name} />
        </TabsContent>
      </Tabs>

      {/* Set model dialog — custom modal because we need an input field inside */}
      {pending === 'set-model' && (
        <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
          <div className="absolute inset-0 bg-surface-sunken/60 backdrop-blur-sm" onClick={() => setPending(null)} />
          <div className="relative z-10 w-full max-w-sm rounded-xl border border-border-subtle bg-surface-raised p-6 shadow-xl">
            <h2 className="text-base font-semibold text-text-primary mb-4">Change model</h2>
            {(() => {
              const models = agentModels.data?.models ?? []
              return (
                <>
                  {agentModels.isLoading && <p className="mb-3 text-sm text-text-muted">Loading models from the authenticated agent…</p>}
                  {agentModels.isError && (
                    <p className="mb-3 text-sm text-warning">
                      No authenticated model catalog is available yet. Finish authentication and wait a few seconds, then reopen this dialog.
                    </p>
                  )}
                  <select
                    value={newModel}
                    onChange={(e) => setNewModel(e.target.value)}
                    className="w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm text-text-primary focus:border-accent focus:outline-none"
                    disabled={models.length === 0}
                  >
                    {!newModel && <option value="">Select a model</option>}
                    {newModel && !models.some((m) => m.id === newModel) && <option value={newModel}>{newModel} · current model (not in catalog)</option>}
                    {models.map((m) => {
					  const k = Math.round(m.contextWindow / 1000)
					  const window = k >= 1000 ? `${(k / 1000).toFixed(0)}M ctx` : `${k}K ctx`
					  const label = m.contextWindowKnown ? window : 'context unknown'
                      return (
                        <option key={m.id} value={m.id}>
                          {(m.displayName || m.id)} · {label}
                        </option>
                      )
                    })}
                  </select>
                </>
              )
            })()}
            <div className="mt-4 flex gap-3 justify-end">
              <Button variant="ghost" size="sm" onClick={() => setPending(null)} disabled={isActing}>
                Cancel
              </Button>
              <Button
                variant="primary"
                size="sm"
                onClick={() => void executeAction()}
                loading={isActing}
                disabled={!newModel}
              >
                Apply
              </Button>
            </div>
          </div>
        </div>
      )}

      {/* Set harness version dialog — kyber#378 PR-D. Claude Code agents can
          pick from detected npm versions; other runtimes use manual entry.
          Empty input clears spec.runtimeVersion. */}
      {pending === 'set-runtime-version' && (
        <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
          <div className="absolute inset-0 bg-surface-sunken/60 backdrop-blur-sm" onClick={() => setPending(null)} />
          <div className="relative z-10 w-full max-w-sm rounded-xl border border-border-subtle bg-surface-raised p-6 shadow-xl">
            <h2 className="text-base font-semibold text-text-primary mb-4">Change Harness Version</h2>
            {(() => {
              const isClaudeCode = agent.runtime === 'claude-code'
              const versions = isClaudeCode ? effective.claudeCodeVersions : effective.codexVersions
              const inList = versions.includes(newRuntimeVersion)
              return (
                <>
                  <select
                    value={newRuntimeVersion}
                    onChange={(e) => setNewRuntimeVersion(e.target.value)}
                    className="w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm text-text-primary focus:border-accent focus:outline-none"
                  >
                    <option value="">{isClaudeCode ? '(use fleet default)' : '(use baked-in version)'}</option>
                    {!inList && newRuntimeVersion && (
                      <option value={newRuntimeVersion}>{newRuntimeVersion} (manual)</option>
                    )}
                    {versions.map((v, i) => (
                      <option key={v} value={v}>
                        {v}{i === 0 ? ' (latest)' : ''}
                      </option>
                    ))}
                  </select>
                  <input
                    type="text"
                    placeholder={isClaudeCode ? 'Manual override: e.g. 2.1.200' : 'Manual harness version'}
                    value={!inList ? newRuntimeVersion : ''}
                    onChange={(e) => setNewRuntimeVersion(e.target.value.trim())}
                    className="mt-2 w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm text-text-primary focus:border-accent focus:outline-none"
                    autoComplete="off"
                    spellCheck={false}
                    aria-label="Manual harness version override"
                  />
                  <p className="mt-2 text-[11px] text-text-disabled">
                    Charset: <code>{`[0-9A-Za-z.\\-]`}</code>, max 64 chars. Empty
                    clears spec.runtimeVersion and falls back to the {isClaudeCode ? 'fleet default' : 'baked-in version'}.
                    Apply rolls the agent pod when Running.
                  </p>
                </>
              )
            })()}
            <div className="mt-4 flex gap-3 justify-end">
              <Button variant="ghost" size="sm" onClick={() => setPending(null)} disabled={isActing}>
                Cancel
              </Button>
              <Button
                variant="primary"
                size="sm"
                onClick={() => void executeAction()}
                loading={isActing}
              >
                Apply
              </Button>
            </div>
          </div>
        </div>
      )}

      {/* Set resources dialog — mirrors set-model dialog */}
      {pending === 'set-resources' && (
        <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
          <div className="absolute inset-0 bg-surface-sunken/60 backdrop-blur-sm" onClick={() => setPending(null)} />
          <div className="relative z-10 w-full max-w-sm rounded-xl border border-border-subtle bg-surface-raised p-6 shadow-xl">
            <h2 className="text-base font-semibold text-text-primary mb-4">Set Resources</h2>
            <div className="space-y-3">
              <label className="block text-sm text-text-secondary">
                CPU
                <input
                  type="text"
                  value={newCPU}
                  onChange={(e) => setNewCPU(e.target.value)}
                  placeholder="e.g. 500m, 1, 2"
                  className="mt-1 w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm text-text-primary placeholder-text-disabled focus:border-accent focus:outline-none"
                />
              </label>
              <label className="block text-sm text-text-secondary">
                Memory
                <input
                  type="text"
                  value={newMemory}
                  onChange={(e) => setNewMemory(e.target.value)}
                  placeholder="e.g. 2Gi, 4Gi"
                  className="mt-1 w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm text-text-primary placeholder-text-disabled focus:border-accent focus:outline-none"
                />
              </label>
              <p className="text-xs text-text-muted">
                Applying this will restart the agent.
              </p>
            </div>
            <div className="mt-4 flex gap-3 justify-end">
              <Button variant="ghost" size="sm" onClick={() => setPending(null)} disabled={isActing}>
                Cancel
              </Button>
              <Button
                variant="primary"
                size="sm"
                onClick={() => void executeAction()}
                loading={isActing}
                disabled={
                  (!newCPU && !newMemory) ||
                  (newCPU === agent.resources.cpu && newMemory === agent.resources.memory)
                }
              >
                Apply
              </Button>
            </div>
          </div>
        </div>
      )}

      {pending !== null && pending !== 'set-model' && pending !== 'set-runtime-version' && pending !== 'set-resources' && (
        <ConfirmDialog
          open={true}
          title={confirmTitle(pending)}
          message={agentActionConfirmMessage(pending, name)}
          confirmLabel={pending === 'delete' ? 'Delete' : 'Confirm'}
          dangerous={pending === 'delete'}
          loading={isActing}
          onConfirm={() => void executeAction()}
          onCancel={() => setPending(null)}
        />
      )}
    </div>
  )
}

// confirmTitle gives each ActionKind a readable dialog header. The generic
// `${capitalize(kind)} agent?` template breaks down for multi-word kinds
// like 'restart-session' ("Restart-session agent?" reads poorly).
function confirmTitle(kind: ActionKind): string {
  switch (kind) {
    case 'restart-session':
      return 'Restart session?'
    case 'compact-session':
      return 'Compact session?'
    case 'restart':
      return 'Restart pod?'
    case 'retry-startup':
      // Same header as 'restart' on purpose — from the operator's side it is
      // the same move; only the endpoint underneath differs (kyber#26).
      return 'Restart pod?'
    case 'force-needs-auth':
      return 'Require re-auth?'
    case 'delete':
      return 'Delete agent?'
    default:
      return `${capitalize(kind)} agent?`
  }
}

function capitalize(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1)
}
