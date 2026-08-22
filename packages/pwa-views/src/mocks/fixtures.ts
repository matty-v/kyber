// Mock fixtures for MSW handlers. Used only when VITE_ENABLE_MOCKS=1 is set
// at build/dev time. See src/mocks/init.ts for the wire-up.
//
// Fixtures are shaped to exercise the real feature surface — a populated
// agent with scheduled jobs, recent job runs (mixed outcomes), secrets, and
// a plausible token-usage snapshot — so UI screenshots land on real states
// rather than the cold empty-first-boot one.

import type {
  Agent,
  AgentJob,
  AgentJobRun,
  AgentListResponse,
  AgentSecret,
  ComputeConfig,
  Machine,
  MachineListResponse,
  TokenUsage,
} from '../lib/types'

export const MOCK_AGENT_NAME = 'alice'
export const MOCK_MACHINE_NAME = 'demo'

export const mockAgentJobs: AgentJob[] = [
  {
    name: 'morning-triage',
    schedule: '0 9 * * *',
    prompt: 'Summarize any PRs opened overnight and flag ones needing review.',
    exclusive: true,
  },
  {
    name: 'hourly-heartbeat',
    schedule: '0 * * * *',
    prompt: 'Post a one-line status update to the #kyber-status channel.',
  },
  {
    name: 'nightly-cleanup',
    schedule: '0 2 * * *',
    prompt: 'Prune any abandoned branches older than 30 days from scratch repos.',
  },
]

export const mockLastJobRuns: AgentJobRun[] = [
  {
    jobName: 'morning-triage',
    startedAt: new Date(Date.now() - 4 * 3600_000).toISOString(),
    finishedAt: new Date(Date.now() - 4 * 3600_000 + 22_000).toISOString(),
    outcome: 'success',
  },
  {
    jobName: 'hourly-heartbeat',
    startedAt: new Date(Date.now() - 12 * 60_000).toISOString(),
    finishedAt: new Date(Date.now() - 12 * 60_000 + 3_000).toISOString(),
    outcome: 'success',
  },
  {
    jobName: 'nightly-cleanup',
    startedAt: new Date(Date.now() - 9 * 3600_000).toISOString(),
    finishedAt: new Date(Date.now() - 9 * 3600_000 + 1_500).toISOString(),
    outcome: 'failed',
    error: 'gh api rate limit exceeded — retry next scheduled run',
  },
]

export const mockTokenUsage: TokenUsage = {
  model: 'claude-sonnet-4-6',
  tokens: {
    used: 128_432,
    limit: 200_000,
    input: 108_200,
    cacheCreation: 12_450,
    cacheRead: 7_782,
  },
  percentage: 64.2,
  effortLevel: 'medium',
  speed: 'normal',
  updatedAt: new Date(Date.now() - 35_000).toISOString(),
}

export const mockAgent: Agent = {
  id: MOCK_AGENT_NAME,
  phase: 'Running',
  machine: MOCK_MACHINE_NAME,
  runtime: 'claude-code',
  model: 'claude-sonnet-4-6',
  resources: { cpu: '2', memory: '4Gi', disk: '50Gi' },
  status: {
    phase: 'Running',
    podName: `agent-${MOCK_AGENT_NAME}`,
    podIP: '10.244.0.12',
    restartCount: 0,
  },
  identityRepo: {
    repo: 'matty-v/alice-agent',
    phase: 'Ready',
    tokenExpiresAt: new Date(Date.now() + 45 * 60_000).toISOString(),
    lastMinted: new Date(Date.now() - 15 * 60_000).toISOString(),
  },
  jobs: mockAgentJobs,
  lastJobRuns: mockLastJobRuns,
  tokenUsage: mockTokenUsage,
  createdAt: new Date(Date.now() - 36 * 3600_000).toISOString(),
}

export const mockAgentList: AgentListResponse = {
  items: [mockAgent],
}

// Activity fixtures for the Agent list (kyber#417). mockAgent deliberately
// carries no `activity` field — adding one would perturb the many existing
// tests that consume mockAgent — so the idle / no-activity cases live in
// dedicated fixtures the AgentList activity tests render against.
export const mockIdleAgent: Agent = {
  ...mockAgent,
  id: 'idle-han',
  activity: {
    state: 'idle',
    lastActivityAt: new Date(Date.now() - 60 * 60_000).toISOString(),
  },
}

export const mockNoActivityAgent: Agent = {
  ...mockAgent,
  id: 'quiet-luke',
  // No `activity` field — visible-by-absence: the list renders nothing for
  // this element (no dot, no badge).
}

export const mockMachine: Machine = {
  id: MOCK_MACHINE_NAME,
  phase: 'Running',
  spec: {
    provider: 'mock',
    capacity: { cpu: '16', memory: '32Gi', ephemeralStorage: '200Gi' },
  },
  status: {
    phase: 'Running',
    nodeName: 'demo',
    agentCount: 1,
    allocatable: { cpu: '14', memory: '28Gi' },
    // observedCapacity / assignableCapacity / availableCapacity mirror the
    // controller-populated fields the API surfaces since #142. Mock values
    // model a node with 14 CPU / 28GiB allocatable, 2 CPU / 4GiB reserved
    // for the platform, and one running agent that takes 2 CPU / 4GiB —
    // leaving 10 CPU / 20GiB available. Disk: 200Gi total, 10Gi platform
    // reservation → 190Gi assignable; alice consumes 50Gi → 140Gi available
    // (matches mockAgent.resources.disk = '50Gi' below).
    observedCapacity: { cpu: '14', memory: '28Gi', ephemeralStorage: '200Gi' },
    assignableCapacity: { cpu: '12', memory: '24Gi', ephemeralStorage: '190Gi' },
    availableCapacity: { cpu: '10', memory: '20Gi', ephemeralStorage: '140Gi' },
    // Legacy alias fields kept for backward-compat consumers.
    availableCPU: '10',
    availableMemory: '20Gi',
  },
  createdAt: new Date(Date.now() - 14 * 24 * 3600_000).toISOString(),
}

export const mockMachineList: MachineListResponse = {
  items: [mockMachine],
}

export const mockSecrets: AgentSecret[] = [
  {
    key: 'GITHUB_TOKEN',
    kind: 'kv',
    size: 40,
    sha256Prefix: 'a3f2',
    createdAt: new Date(Date.now() - 20 * 24 * 3600_000).toISOString(),
    updatedAt: new Date(Date.now() - 2 * 24 * 3600_000).toISOString(),
  },
  {
    key: 'OPENAI_API_KEY',
    kind: 'kv',
    size: 51,
    sha256Prefix: 'c1b9',
    createdAt: new Date(Date.now() - 14 * 24 * 3600_000).toISOString(),
    updatedAt: new Date(Date.now() - 14 * 24 * 3600_000).toISOString(),
  },
  {
    key: 'deploy-key.pem',
    kind: 'file',
    size: 3072,
    sha256Prefix: '7e44',
    createdAt: new Date(Date.now() - 7 * 24 * 3600_000).toISOString(),
    updatedAt: new Date(Date.now() - 7 * 24 * 3600_000).toISOString(),
  },
]

export const mockComputeConfig: ComputeConfig = {
  compute: { provider: 'mock' },
  models: [
    { id: 'claude-sonnet-4-6', contextWindow: 200_000 },
    { id: 'claude-opus-4-7', contextWindow: 200_000 },
    { id: 'claude-haiku-4-5', contextWindow: 200_000 },
  ],
  identity: { repoOwner: 'matty-v' },
}

// GitHub repos the App "has access to" in mock mode. Includes the
// kyber-agent-template and a couple of plausible existing identity repos
// so the dropdown screenshot communicates the feature.
export const mockGitHubRepos = {
  repos: [
    {
      fullName: 'matty-v/alice-agent',
      description: 'Identity repo for Alice — strategic planning agent.',
      private: true,
      defaultBranch: 'main',
      isTemplate: false,
    },
    {
      fullName: 'matty-v/han-agent',
      description: 'Identity repo for Han — pragmatic execution agent.',
      private: true,
      defaultBranch: 'main',
      isTemplate: false,
    },
    {
      fullName: 'matty-v/kyber',
      description: 'Kyber control plane (parent project).',
      private: false,
      defaultBranch: 'main',
      isTemplate: false,
    },
  ],
  templates: [
    {
      fullName: 'matty-v/kyber-agent-template',
      description: 'Stamped into new identity repos by the agent controller.',
      private: true,
      defaultBranch: 'main',
      isTemplate: true,
    },
  ],
}

// Plausible pod boot log — the kind of thing LogViewer renders in the
// Activity > Pod Boot Log sub-tab. Realistic enough that screenshots
// communicate what the feature's showing.

// Mock inbound bindings for the Webhooks tab (#222 screenshots). Two
// bindings so the table doesn't look empty when the screenshot lands.
export const mockInboundBindings = [
  {
    name: 'ci-watch',
    existingSecret: `${MOCK_AGENT_NAME}-ci-watch-hmac`,
    signatureHeader: 'X-Hub-Signature-256',
    signaturePrefix: 'sha256=',
    eventHeader: 'X-GitHub-Event',
    matchEvents: ['push', 'pull_request'],
    action: 'Triage this CI event. If a build failed, summarise the first error.',
    limits: { maxPerMinute: 30 },
    url: `https://kyber.example.com/webhooks/inbound/${MOCK_AGENT_NAME}/ci-watch`,
    stats: {
      lastReceivedAt: '2026-05-03T10:24:00Z',
      totalDispatched: 47,
      dropped: {
        'rate-limited': 0,
        'queue-full': 0,
        'sig-mismatch': 1,
        'missing-secret': 0,
        'unmatched-event': 3,
        'filter-rejected': 0,
        dedup: 2,
      },
    },
  },
  {
    name: 'stripe',
    existingSecret: `${MOCK_AGENT_NAME}-stripe-hmac`,
    signatureHeader: 'Stripe-Signature',
    action: 'Review the charge and decide whether it warrants a retry.',
    limits: { maxPerMinute: 60 },
    url: `https://kyber.example.com/webhooks/inbound/${MOCK_AGENT_NAME}/stripe`,
    stats: {
      lastReceivedAt: '2026-05-03T08:00:00Z',
      totalDispatched: 12,
      dropped: {
        'rate-limited': 0,
        'queue-full': 0,
        'sig-mismatch': 0,
        'missing-secret': 0,
        'unmatched-event': 0,
        'filter-rejected': 0,
        dedup: 0,
      },
    },
  },
]

export const mockBootLog = `[2026-04-21T14:32:01Z] [kyber] Overlay mounted (kernel overlayfs)
[2026-04-21T14:32:02Z] [kyber] Returning pod — upper layer has state
[2026-04-21T14:32:02Z] [kyber] Filesystem ready. Entering chroot.
[2026-04-21T14:32:03Z] [kyber] dbus=unix:path=/run/user/1000/bus
[2026-04-21T14:32:03Z] [kyber] installed /etc/cron.d/kyber-jobs from ConfigMap
[2026-04-21T14:32:03Z] [kyber] cron daemon started
[2026-04-21T14:32:04Z] [kyber] ~/.claude.json written (onboarding bypass)
[2026-04-21T14:32:04Z] [kyber] settings.json written
[2026-04-21T14:32:05Z] [kyber] Telegram access pre-seeded for chat 1000000001
[2026-04-21T14:32:05Z] [kyber] Telegram plugin already installed
[2026-04-21T14:32:06Z] [kyber] identity repo matty-v/alice-agent present at /home/kyber/dev/alice-agent — pulling
[2026-04-21T14:32:07Z] [kyber] cached access_token still valid (expires_at=1745244127000) — skipping refresh
[2026-04-21T14:32:07Z] [kyber] credentials.json written
[2026-04-21T14:32:07Z] [kyber] credentials.json present — Claude Code will authenticate via keychain
[2026-04-21T14:32:07Z] [kyber] token-reporter launched (pid=142) cp_url=http://kyber-control-plane.kyber-system.svc:8082 home=/home/kyber
[2026-04-21T14:32:08Z] [kyber] Starting Claude Code in tmux (cwd=/home/kyber/dev/alice-agent): claude --dangerously-skip-permissions --channels plugin:telegram@claude-plugins-official --model claude-sonnet-4-6[1m]
[2026-04-21T14:32:09Z] [kyber] tmux session 'agent' started — waiting
`
