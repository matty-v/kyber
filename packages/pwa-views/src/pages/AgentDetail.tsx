import { useEffect, useState } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import { usePrefixedPath } from '../lib/route-prefix'
import { AlertTriangle, ArrowLeft, Play, Square, RotateCcw, KeyRound, Cpu, Trash2, MoreHorizontal, Minimize2, ScrollText, Wrench, Menu, X } from 'lucide-react'
import {
  useAgent,
  useStartAgent,
  useStopAgent,
  useRestartAgent,
  useRestartAgentSession,
  useCompactAgentSession,
  useForceNeedsAuthAgent,
  useRepairAgentRuntime,
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
  useSetRequestReplyEnabled,
  useUpdateAgentProfile,
} from '../hooks/useAPI'
import { useEffectiveModelList } from '../lib/models'
import { StatusBadge } from '../components/StatusBadge'
import { SchedulingFailureBanner } from '../components/SchedulingFailureBanner'
import { SchedulingFailureBadge } from '../components/SchedulingFailureBadge'
import { AgentActivityBadge } from '../components/AgentActivityBadge'
import { AgentResourceUsage } from '../components/AgentResourceUsage'
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
import { PublicCapabilitiesEditor } from '../components/PublicCapabilitiesEditor'
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
  | 'repair-runtime'
  | 'retry-startup' // NeedsAuth "try again with what you have" (kyber#26); POSTs /start
  | 'delete'
  | 'set-model'
  | 'set-runtime-version'
  | 'set-resources'
type AgentSection = 'overview' | 'activity' | 'shell' | 'jobs' | 'webhooks' | 'general' | 'comms' | 'secrets' | 'capabilities'

type AgentNavItem = {
  id: AgentSection
  label: string
  description: string
}

const agentNavigation: { label: string; items: AgentNavItem[] }[] = [
  { label: 'Observe', items: [
    { id: 'overview', label: 'Overview', description: 'Health and live resources' },
    { id: 'activity', label: 'Activity', description: 'Conversation and tool history' },
    { id: 'shell', label: 'Shell', description: 'Interactive terminal' },
  ] },
  { label: 'Automate', items: [
    { id: 'jobs', label: 'Jobs', description: 'Scheduled prompts' },
    { id: 'webhooks', label: 'Webhooks', description: 'External triggers' },
  ] },
  { label: 'Configure', items: [
    { id: 'general', label: 'General', description: 'Identity and runtime' },
    { id: 'comms', label: 'Comms', description: 'Connected channels' },
    { id: 'secrets', label: 'Secrets', description: 'Injected credentials' },
    { id: 'capabilities', label: 'Capabilities', description: 'Published and detected' },
  ] },
]

const agentSectionIds = new Set(agentNavigation.flatMap((group) => group.items.map((item) => item.id)))

function AgentNavigation({
  value,
  onChange,
}: {
  value: AgentSection
  onChange: (value: AgentSection) => void
}) {
  return (
    <nav aria-label="Agent pages" className="space-y-5">
      {agentNavigation.map((group) => (
        <div key={group.label}>
          <h2 className="mb-1 px-3 text-[11px] font-semibold uppercase tracking-wider text-text-muted">{group.label}</h2>
          <div className="space-y-1">
            {group.items.map((item) => {
              const active = item.id === value
              return (
                <button
                  key={item.id}
                  type="button"
                  aria-current={active ? 'page' : undefined}
                  onClick={() => onChange(item.id)}
                  className={`w-full rounded-lg px-3 py-2 text-left transition-colors ${active ? 'bg-accent-muted text-accent' : 'text-text-secondary hover:bg-surface-overlay hover:text-text-primary'}`}
                >
                  <span className="block text-sm font-medium">{item.label}</span>
                  <span className={`mt-0.5 block text-xs leading-snug ${active ? 'text-accent' : 'text-text-muted'}`}>{item.description}</span>
                </button>
              )
            })}
          </div>
        </div>
      ))}
    </nav>
  )
}

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
  const showBrokenRuntime = agent.phase === 'BrokenRuntime' || agent.runtimeVersion?.usable === false
  // A failed executable probe is authoritative for this boot. Suppress model
  // and version signals that may be left over from an earlier successful boot;
  // they are not actionable until the harness itself works again.
  const showMismatch = !showBrokenRuntime && Boolean(agent.runtimeVersionMismatch)
  const showUnsupported = !showBrokenRuntime && Boolean(agent.modelUnsupported)
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
    !showBrokenRuntime &&
    !showUnsupported &&
    agent.runtimeVersion?.modelSupported !== false &&
    Boolean(agent.runtimeVersion?.modelProbeMessage)
  if (!showMismatch && !showUnsupported && !showImageMissing && !showModelUnresolved && !showBrokenRuntime && !showProbeInconclusive) return null
  const installed = agent.runtimeVersion?.installedVersion
  const requested = agent.runtimeVersion?.requestedVersion
  return (
    <div className="space-y-2">
      {showBrokenRuntime && (
        <Card className="border-danger/40 bg-danger/5">
          <div className="flex items-start gap-3">
            <AlertTriangle className="h-5 w-5 text-danger shrink-0 mt-0.5" aria-hidden="true" />
            <div className="space-y-1">
              <h2 className="text-sm font-semibold text-text-primary">Runtime harness is unusable</h2>
              <p className="text-xs text-text-muted">
                The <code className="font-mono">{agent.runtimeVersion?.runtime ?? agent.runtime}</code> executable
                failed before authentication was checked. Re-authorizing will not help. Repair the runtime harness,
                then restart the agent; Kyber will not auto-restart this state.
              </p>
              {(agent.runtimeVersion?.probeMessage || agent.status?.message) && (
                <p className="text-xs text-text-muted font-mono border-l-2 border-danger/40 pl-2">
                  {agent.runtimeVersion?.probeMessage ?? agent.status?.message}
                </p>
              )}
            </div>
          </div>
        </Card>
      )}
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

export function RequestReplyCard({
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
      <h2 className="text-sm font-medium text-text-primary mb-2">Bounded requests</h2>
      <label className="flex items-start gap-2 text-sm text-text-primary">
        <input
          type="checkbox"
          className="mt-0.5"
          checked={enabled}
          disabled={pending}
          onChange={(e) => onChange(e.target.checked)}
        />
        <span>
          Allow authenticated request/reply submissions to this agent
          <span className="mt-0.5 block text-xs text-text-muted">
            Off by default. Callers still need dedicated request scopes, and
            responses remain bounded and explicit. Disabling blocks new work;
            requests already in flight can finish or expire.
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

// LifecycleMenuItems now lives in components/AgentActionMenuItems.tsx so the
// agent LIST renders the identical per-phase menu. Re-exported here because it
// was exported from this module for isolated testing (kyber#599) and callers
// and tests still import it from this path.
export { LifecycleMenuItems } from '../components/AgentActionMenuItems'
import { LifecycleMenuItems as LifecycleItems, SessionMenuItems } from '../components/AgentActionMenuItems'

export function AgentDetail() {
  const { name = '', section } = useParams<{ name: string; section?: string }>()
  const navigate = useNavigate()
  const prefixed = usePrefixedPath()
  const activeSection: AgentSection = section && agentSectionIds.has(section as AgentSection) ? section as AgentSection : 'overview'
  const { data: agent, isLoading, error } = useAgent(name)
  const tokenUsage = useTokenUsage(name, agent?.phase === 'Running')
  const { data: computeConfig } = useComputeConfig()
  // The public /available catalog supplies harness versions. Model choices
  // come separately from this agent's authenticated provider catalog.
  const effective = useEffectiveModelList(agent?.runtime)
  const [pending, setPending] = useState<ActionKind | null>(null)
  const [mobileNavOpen, setMobileNavOpen] = useState(false)
  const [newModel, setNewModel] = useState('')
  const [newRuntimeVersion, setNewRuntimeVersion] = useState('')
  const [newCPU, setNewCPU] = useState('')
  const [newMemory, setNewMemory] = useState('')
  const [startupPrompt, setStartupPrompt] = useState('')
  const [profileAlias, setProfileAlias] = useState('')
  const [profileDescription, setProfileDescription] = useState('')
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
  const repairAgentRuntime = useRepairAgentRuntime()
  const setAgentModel = useSetAgentModel()
  const setAgentRuntimeVersion = useSetAgentRuntimeVersion()
  const setAgentResources = useSetAgentResources()
  const patchAgent = usePatchAgent()
  const updateAgentProfile = useUpdateAgentProfile()
  const setSessionResume = useSetSessionResume()
  const setRequestReplyEnabled = useSetRequestReplyEnabled()
  const deleteAgent = useDeleteAgent()
  const reauthorizeAgent = useReauthorizeAgent()

  useEffect(() => {
    setStartupPrompt(agent?.startupPrompt ?? '')
  }, [agent?.startupPrompt])

  useEffect(() => {
    setProfileAlias(agent?.profile?.alias ?? '')
    setProfileDescription(agent?.profile?.description ?? '')
  }, [agent?.profile?.alias, agent?.profile?.description])

  const isActing =
    startAgent.isPending ||
    stopAgent.isPending ||
    restartAgent.isPending ||
    restartAgentSession.isPending ||
    compactAgentSession.isPending ||
    forceNeedsAuthAgent.isPending ||
    repairAgentRuntime.isPending ||
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
      if (action === 'repair-runtime') await repairAgentRuntime.mutateAsync(name)
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

  const activeNavItem = agentNavigation.flatMap((group) => group.items).find((item) => item.id === activeSection)!

  function selectSection(nextSection: AgentSection) {
    setMobileNavOpen(false)
    navigate(prefixed(nextSection === 'overview' ? `/agents/${name}` : `/agents/${name}/${nextSection}`))
  }

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
      <div className="mb-4 flex flex-wrap items-center gap-2 sm:gap-3">
        <Button variant="ghost" size="sm" onClick={() => navigate(prefixed('/agents'))}>
          <ArrowLeft className="h-4 w-4" />
        </Button>
        <h1 className="min-w-0 truncate text-xl font-bold text-text-primary">{agent.id}</h1>
        <div className="order-3 flex w-full flex-wrap items-center gap-2 pl-10 sm:order-none sm:w-auto sm:pl-0">
          <StatusBadge phase={agent.phase} />
          <SchedulingFailureBadge agent={agent} />
          <AgentActivityBadge agent={agent} />
        </div>
        <div className="ml-auto flex items-center gap-2">
          <Button variant="secondary" size="sm" onClick={() => navigate(prefixed(`/logs?agent=${encodeURIComponent(name)}`))}>
            <ScrollText className="h-4 w-4" /> <span className="hidden sm:inline">Logs</span>
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
                  <SessionMenuItems phase={agent.phase} onSelect={setPending} />
                </>
              )}
              {hasPodActions && (
                <>
                  {hasAgentActions && <DropdownMenuSeparator />}
                  <DropdownMenuLabel>Pod actions</DropdownMenuLabel>
                  <LifecycleItems phase={agent.phase} onSelect={setPending} />
                </>
              )}
              <DropdownMenuSeparator />
              <DropdownMenuItem variant="danger" onSelect={() => setPending('delete')}>
                <Trash2 className="h-3.5 w-3.5" />
                Delete
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      <button
        type="button"
        onClick={() => setMobileNavOpen(true)}
        className="mb-4 flex w-full items-center justify-between rounded-lg border border-border-default bg-surface-raised px-4 py-3 text-left sm:hidden"
        aria-haspopup="dialog"
      >
        <span>
          <span className="block text-sm font-semibold text-text-primary">{activeNavItem.label}</span>
          <span className="block text-xs text-text-muted">{activeNavItem.description}</span>
        </span>
        <Menu className="h-5 w-5 text-text-secondary" />
      </button>

      <div className="sm:grid sm:grid-cols-[12rem_minmax(0,1fr)] sm:items-start sm:gap-6">
        <aside className="hidden sm:sticky sm:top-4 sm:block">
          <AgentNavigation value={activeSection} onChange={selectSection} />
        </aside>
        <main className="min-w-0">
        {activeSection === 'overview' && (
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
                {agent.blockedReason ??
                  'This agent\'s OAuth credential is missing, expired, or invalid. Re-authorize to resume.'}
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
          <MismatchBadges agent={agent} />
          <AgentTerminalPeek agentName={name} hasPod={Boolean(agent.status.podName)} />
          <TokenUsageCard data={tokenUsage.data} isLoading={tokenUsage.isLoading} />
          <Card>
            <h2 className="mb-3 text-sm font-semibold text-text-primary">Live pod resources</h2>
            {agent.activity?.resources ? (
              <AgentResourceUsage usage={agent.activity.resources} />
            ) : (
              <p className="text-sm text-text-muted">Usage will appear when the running pod reports metrics.</p>
            )}
          </Card>
          <details className="rounded-lg border border-border-subtle bg-surface-raised px-4 py-3">
            <summary className="cursor-pointer text-sm font-medium text-text-secondary">Technical details</summary>
            <div className="mt-4 grid gap-4 sm:grid-cols-2">
              <div>
                <h2 className="mb-3 text-xs font-medium text-text-muted">Pod status</h2>
                <StatusCardBody status={agent.status} />
              </div>
              <dl className="space-y-2 text-sm">
                <div className="flex justify-between gap-3">
                  <dt className="text-text-muted">Model</dt>
                  <dd className="truncate font-mono text-xs text-text-primary">{agent.currentModel || agent.model || 'Harness default'}</dd>
                </div>
                <div className="flex justify-between gap-3">
                  <dt className="text-text-muted">Runtime</dt>
                  <dd className="text-text-primary">{agent.runtime}{agent.runtimeVersion?.installedVersion ? ` ${agent.runtimeVersion.installedVersion}` : ''}</dd>
                </div>
                <div className="flex justify-between">
                  <dt className="text-text-muted">Machine</dt>
                  <dd className="text-text-primary">{agent.machine}</dd>
                </div>
                <div className="flex justify-between">
                  <dt className="text-text-muted">CPU / memory</dt>
                  <dd className="text-text-primary">{agent.resources.cpu} / {agent.resources.memory}</dd>
                </div>
                <div className="flex justify-between">
                  <dt className="text-text-muted">Disk</dt>
                  <dd className="text-text-primary">{agent.resources.disk}</dd>
                </div>
                <div className="flex justify-between">
                  <dt className="text-text-muted">Auth</dt>
                  <dd className="text-text-primary">{agent.authType || 'Default'}</dd>
                </div>
              </dl>
            </div>
          </details>
          {agent.identityRepo && (
            <details className="group">
              <summary className="cursor-pointer text-sm font-medium text-text-secondary">Identity repository</summary>
              <div className="mt-3"><IdentityRepoCard data={agent.identityRepo} /></div>
            </details>
          )}
          </div>
        )}

        {activeSection === 'activity' && <ActivityTab agentName={name} />}
        {activeSection === 'jobs' && <JobsTab agentName={name} />}
        {activeSection === 'webhooks' && <WebhooksTab agentName={name} />}

        {activeSection === 'general' && (
                <div className="space-y-4">
                  <Card>
                    <h2 className="mb-1 text-sm font-semibold text-text-primary">Agent profile</h2>
                    <p className="mb-3 text-xs text-text-muted">An operator-facing identity. Changes do not restart the agent.</p>
                    <div className="space-y-3">
                      <label className="block text-xs font-medium text-text-muted">
                        Alias
                        <input
                          value={profileAlias}
                          maxLength={80}
                          onChange={(event) => setProfileAlias(event.target.value)}
                          placeholder={name}
                          className="mt-1 w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm text-text-primary placeholder-text-disabled focus:border-accent focus:outline-none"
                        />
                      </label>
                      <label className="block text-xs font-medium text-text-muted">
                        Description
                        <textarea
                          value={profileDescription}
                          maxLength={500}
                          rows={3}
                          onChange={(event) => setProfileDescription(event.target.value)}
                          placeholder="What this agent is for"
                          className="mt-1 w-full resize-y rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm text-text-primary placeholder-text-disabled focus:border-accent focus:outline-none"
                        />
                      </label>
                      <Button
                        type="button"
                        variant="secondary"
                        size="sm"
                        loading={updateAgentProfile.isPending}
                        disabled={updateAgentProfile.isPending || profileAlias === (agent.profile?.alias ?? '') && profileDescription === (agent.profile?.description ?? '')}
                        onClick={() => updateAgentProfile.mutate({ name, profile: { alias: profileAlias, description: profileDescription } })}
                      >
                        Save profile
                      </Button>
                    </div>
                  </Card>
                  <Card>
                    <div className="flex flex-wrap items-start justify-between gap-3">
                      <div>
                        <h2 className="text-sm font-semibold text-text-primary">Runtime</h2>
                        <p className="mt-1 text-xs text-text-muted">Model, harness version, and compute allocation.</p>
                      </div>
                      <div className="flex flex-wrap gap-2">
                        <Button size="sm" variant="secondary" onClick={() => { setNewModel(agent.currentModel || agent.model); setPending('set-model') }}>Model</Button>
                        <Button size="sm" variant="secondary" onClick={() => { setNewRuntimeVersion(agent.runtimeVersion?.requestedVersion ?? ''); setPending('set-runtime-version') }}>Harness</Button>
                        <Button size="sm" variant="secondary" onClick={() => { setNewCPU(agent.resources.cpu); setNewMemory(agent.resources.memory); setPending('set-resources') }}>Resources</Button>
                      </div>
                    </div>
                    <dl className="mt-4 grid gap-x-8 gap-y-2 border-t border-border-subtle pt-3 text-sm sm:grid-cols-2">
                      <div className="flex justify-between gap-3"><dt className="text-text-muted">Model</dt><dd className="truncate font-mono text-xs text-text-primary">{agent.currentModel || agent.model || 'Harness default'}</dd></div>
                      <div className="flex justify-between gap-3"><dt className="text-text-muted">Harness</dt><dd className="truncate font-mono text-xs text-text-primary">{agent.runtimeVersion?.installedVersion || agent.runtime}</dd></div>
                      <div className="flex justify-between gap-3"><dt className="text-text-muted">CPU</dt><dd className="text-text-primary">{agent.resources.cpu}</dd></div>
                      <div className="flex justify-between gap-3"><dt className="text-text-muted">Memory</dt><dd className="text-text-primary">{agent.resources.memory}</dd></div>
                    </dl>
                  </Card>
                  <Card>
                    <h2 className="mb-2 text-sm font-semibold text-text-primary">Startup prompt</h2>
                    <p className="mb-3 text-xs text-text-muted">Sent as the first user turn on every new session. Saving marks this agent for restart without interrupting the live session.</p>
                    <textarea
                      value={startupPrompt}
                      onChange={(event) => setStartupPrompt(event.target.value)}
                      maxLength={32768}
                      rows={6}
                      className="w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm text-text-primary focus:border-accent focus:outline-none"
                      placeholder="No startup prompt configured"
                    />
                    <div className="mt-2 flex items-center justify-between gap-3">
                      <span className="text-xs text-text-muted">{startupPrompt.length.toLocaleString()} / 32,768</span>
                      <Button size="sm" loading={patchAgent.isPending} disabled={patchAgent.isPending || startupPrompt === (agent.startupPrompt ?? '')} onClick={() => patchAgent.mutate({ name, startupPrompt })}>Save prompt</Button>
                    </div>
                  </Card>
                  <SessionResumeCard enabled={agent.sessionResume ?? false} pending={setSessionResume.isPending} onChange={(enabled) => setSessionResume.mutate({ name, sessionResume: enabled })} />
                  <RequestReplyCard enabled={agent.requestReplyEnabled ?? false} pending={setRequestReplyEnabled.isPending} onChange={(enabled) => setRequestReplyEnabled.mutate({ name, requestReplyEnabled: enabled })} />
                </div>
        )}
        {activeSection === 'comms' && <CommsTab agentName={name} onRestartPod={() => setPending('restart')} />}
        {activeSection === 'secrets' && <SecretsTab agentName={name} />}
        {activeSection === 'capabilities' && (
          <div className="space-y-4">
            <PublicCapabilitiesEditor agent={agent} />
            <SkillsTab agentName={name} />
          </div>
        )}
        {activeSection === 'shell' && <ShellTab agentName={name} />}
        </main>
      </div>

      {mobileNavOpen && (
        <div className="fixed inset-0 z-50 bg-surface-base sm:hidden" role="dialog" aria-modal="true" aria-label="Agent navigation">
          <div className="flex items-center justify-between border-b border-border-subtle px-4 py-3">
            <div>
              <p className="text-sm font-semibold text-text-primary">{agent.id}</p>
              <p className="text-xs text-text-muted">Agent navigation</p>
            </div>
            <Button variant="ghost" size="sm" onClick={() => setMobileNavOpen(false)} aria-label="Close navigation">
              <X className="h-5 w-5" />
            </Button>
          </div>
          <div className="h-[calc(100dvh-4rem)] overflow-y-auto p-4 pb-24">
            <AgentNavigation value={activeSection} onChange={selectSection} />
          </div>
        </div>
      )}

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
    case 'repair-runtime':
      return 'Repair runtime?'
    case 'delete':
      return 'Delete agent?'
    default:
      return `${capitalize(kind)} agent?`
  }
}

function capitalize(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1)
}
