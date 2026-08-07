// MSW handlers for mock mode. Only the endpoints the Agent detail / list /
// create flows actually hit — not a complete backend mirror. Add more as new
// UI PRs need them. Activated conditionally via VITE_ENABLE_MOCKS=1; see
// src/mocks/init.ts.

import { http, HttpResponse, ws } from 'msw'
import {
  MOCK_AGENT_NAME,
  MOCK_MACHINE_NAME,
  mockAgent,
  mockAgentList,
  mockBootLog,
  mockComputeConfig,
  mockGitHubRepos,
  mockInboundBindings,
  mockMachine,
  mockMachineList,
  mockSecrets,
  mockTokenUsage,
} from './fixtures'
import { twoSessionJsonl } from '../lib/transcript-fixtures'

// Canned "attached to agent tmux" output for the exec WebSocket. Includes
// the Connected banner the terminal prints on onopen plus a plausible
// Claude Code prompt framing so screenshots communicate what the Shell +
// Activity tabs look like in the populated state.
const execAttachFrames: string[] = [
  '\r\x1b[32mConnected\x1b[0m\r\n',
  '\x1b[?1049h\x1b[H', // enter alt-screen + cursor home (simulated claude TUI)
  '\x1b[1m\x1b[38;5;45m╭─ Claude Code ──────────────────────────────────────────╮\x1b[0m\r\n',
  '\x1b[1m\x1b[38;5;45m│\x1b[0m Hello! I\u2019m ready to help with Kyber-related tasks.     \x1b[1m\x1b[38;5;45m│\x1b[0m\r\n',
  '\x1b[1m\x1b[38;5;45m│\x1b[0m Last job \u201cmorning-triage\u201d completed at 10:00 UTC.      \x1b[1m\x1b[38;5;45m│\x1b[0m\r\n',
  '\x1b[1m\x1b[38;5;45m│\x1b[0m Summary: 3 PRs opened overnight, 1 needs review.       \x1b[1m\x1b[38;5;45m│\x1b[0m\r\n',
  '\x1b[1m\x1b[38;5;45m╰────────────────────────────────────────────────────────╯\x1b[0m\r\n',
  '\r\n',
  '\x1b[90m> \x1b[0m',
]

const execShellFrames: string[] = [
  '\r\x1b[32mConnected\x1b[0m\r\n',
  '\r\n[Kyber agent shell — debug helpers]\r\n\r\n',
  '  agent          attach to the Claude Code tmux session (read/write)\r\n',
  '  peek           attach read-only — safer if the agent is mid-task\r\n',
  '  as-agent       become the kyber user (where the agent runs)\r\n',
  '  creds          show ~/.claude/.credentials.json (jq-formatted)\r\n',
  '  tg             show ~/.claude/channels/telegram/access.json\r\n',
  '  boot           tail bootstrap log\r\n',
  '  tokens         tail token-reporter log\r\n',
  '  cron-log       tail /persist/var/log/kyber-jobs.log\r\n',
  '  jobs           show rendered crontab\r\n',
  '  restart-agent  kill the tmux session — pod will restart cleanly\r\n',
  '\r\n',
  '\x1b[38;5;208mroot@agent-alice\x1b[0m:\x1b[38;5;39m/\x1b[0m# ',
]

const execEndpoint = ws.link('*/api/v1/agents/*/exec*')

// Counter shared by the /api/v1/fleet handler so its return values walk
// over time — used for sparkline screenshots and dev-mode demos.
let mockFleetTick = 0

// Match on path suffix — the PWA builds URLs from a base that the dev server
// reverse-proxies. Use wildcards so localhost:5173 and any configured server
// URL both resolve.
export const handlers = [
  // cluster-info is the embedded app's bootstrap gate (EmbeddedClusterProvider
  // fetches it before mounting anything) — without this handler every
  // mock-backed page renders the "Could not reach the kyber control plane"
  // error and the screenshot suite fails at the gate.
  http.get('*/api/v1/cluster-info', () =>
    HttpResponse.json({ name: 'kyber-demo', version: '1.0.0', capabilities: [] }),
  ),

  // Sidebar + Settings poll this for the running build; without it the header
  // renders "version unavailable".
  http.get('*/api/v1/version', () =>
    HttpResponse.json({
      sha: 'a1b2c3d',
      buildDate: '2026-08-07T00:00:00Z',
      chartVersion: '1.0.0',
      substrate: 'k3s',
    }),
  ),

  http.get('*/api/v1/config', () => HttpResponse.json(mockComputeConfig)),

  // Rotate API key — return a deterministic-looking key so screenshots
  // are reproducible. Real backend generates 32 random bytes.
  http.post('*/api/v1/rotate-api-key', () =>
    HttpResponse.json({
      apiKey: 'rotated-mock-key-0000000000000000000000000000000000000000000000000000',
    }),
  ),

  // GitHub repo list — drives the Create Agent identity-repo dropdown (#134).
  http.get('*/api/v1/github/repos', () => HttpResponse.json(mockGitHubRepos)),

  // GitHub repo collision check — used by the create-new-from-template flow.
  // The mock returns true only for repos that already exist in mockGitHubRepos
  // (existing or templates) so the wizard's availability badge has both
  // outcomes to demonstrate.
  http.get('*/api/v1/github/repos/:owner/:name/exists', ({ params }) => {
    const fullName = `${params.owner}/${params.name}`
    const exists =
      mockGitHubRepos.repos.some((r) => r.fullName === fullName) ||
      mockGitHubRepos.templates.some((r) => r.fullName === fullName)
    return HttpResponse.json({ exists })
  }),

  http.get('*/api/v1/agents', () => HttpResponse.json(mockAgentList)),
  http.get(`*/api/v1/agents/${MOCK_AGENT_NAME}`, () => {
    // Allow screenshot / e2e tests to inject a scheduling-failure status
    // without mutating the global fixture. Set window.__mockAgentScheduling
    // before page.goto via page.addInitScript; cleared by reload.
    const override = (window as unknown as {
      __mockAgentScheduling?: import('../lib/types').AgentSchedulingStatus
    }).__mockAgentScheduling
    return HttpResponse.json(override ? { ...mockAgent, scheduling: override } : mockAgent)
  }),
  http.get(`*/api/v1/agents/${MOCK_AGENT_NAME}/token-usage`, () =>
    HttpResponse.json(mockTokenUsage),
  ),
  http.get(`*/api/v1/agents/${MOCK_AGENT_NAME}/secrets`, () =>
    HttpResponse.json({ items: mockSecrets }),
  ),

  // Inbound bindings — list, plus a no-op PATCH used by the edit dialog
  // (#222). The PATCH handler echoes the incoming body merged onto the
  // matched binding so the wizard's success path reflects the operator's
  // edits.
  http.get(`*/api/v1/agents/${MOCK_AGENT_NAME}/inbound-bindings`, () =>
    HttpResponse.json({ bindings: mockInboundBindings }),
  ),
  http.patch(
    `*/api/v1/agents/${MOCK_AGENT_NAME}/inbound-bindings/:bindingName`,
    async ({ request, params }) => {
      const body = (await request.json()) as Record<string, unknown>
      const target = mockInboundBindings.find((b) => b.name === params.bindingName)
      if (!target) {
        return HttpResponse.json(
          { error: { code: 'not_found', message: 'binding not found' } },
          { status: 404 },
        )
      }
      return HttpResponse.json({ ...target, ...body })
    },
  ),
  // Pod boot log endpoint streams — LogViewer is happy with a single blob
  // returned synchronously for screenshot purposes. source=archive (#431)
  // returns a distinct durable-window blob so the Archive view has content.
  http.get(`*/api/v1/agents/${MOCK_AGENT_NAME}/logs`, ({ request }) => {
    const url = new URL(request.url)
    const source = url.searchParams.get('source')
    if (source === 'transcript') {
      return HttpResponse.text(twoSessionJsonl, {
        headers: { 'Content-Type': 'text/plain; charset=utf-8' },
      })
    }
    const body =
      source === 'archive'
        ? `[archive ${url.searchParams.get('since') ?? ''} → ${url.searchParams.get('until') ?? ''}]\n${mockBootLog}`
        : mockBootLog
    return HttpResponse.text(body, {
      headers: { 'Content-Type': 'text/plain; charset=utf-8' },
    })
  }),

  http.get('*/api/v1/machines', () => HttpResponse.json(mockMachineList)),
  http.get(`*/api/v1/machines/${MOCK_MACHINE_NAME}`, () => HttpResponse.json(mockMachine)),

  // Fleet summary endpoint. Returns a baseline snapshot, but jitters the
  // counts across calls so the sparkline screenshots (#104) show a real
  // trend as the rolling window fills. The jitter only affects mock
  // mode — production responses are still authoritative.
  http.get('*/api/v1/fleet', () => {
    const tick = mockFleetTick++
    // A small periodic walk that visits 0–3 in each phase. Looks like a
    // fleet that's restarting / scaling, not a flat line.
    const wave = (offset: number) => Math.abs((tick + offset) % 7 - 3)
    return HttpResponse.json({
      machineCount: 2 + wave(0) % 2,
      agentCount: 3 + wave(2) % 3,
      machinesByPhase: {
        Running: 2 + (wave(0) % 2),
        Failed: wave(1) > 2 ? 1 : 0,
      },
      agentsByPhase: {
        Running: 3 + (wave(2) % 2),
        Restarting: wave(3) > 2 ? 1 : 0,
      },
    })
  }),

  // PATCH / mutation endpoints — echo the request body so optimistic updates
  // resolve cleanly in the mock. Screenshots don't interact with these but
  // toasts fire on unhandled mutations.
  http.patch(`*/api/v1/agents/${MOCK_AGENT_NAME}`, async ({ request }) => {
    const body = (await request.json()) as Partial<typeof mockAgent>
    return HttpResponse.json({ ...mockAgent, ...body })
  }),

  // WebSocket mock for the exec endpoint — picks frames based on ?mode=…
  // query param. Pushes the canned banner + a few frames, then stays open
  // so the terminal shows a populated screen instead of [connection error].
  execEndpoint.addEventListener('connection', ({ client }) => {
    const encoder = new TextEncoder()
    const url = new URL(client.url)
    const frames = url.searchParams.get('mode') === 'shell' ? execShellFrames : execAttachFrames
    // Small stagger so the terminal can paint intermediate frames rather
    // than receiving everything in a single write — makes screenshots of
    // in-progress rendering more realistic.
    frames.forEach((frame, i) => {
      setTimeout(() => client.send(encoder.encode(frame)), 20 * i)
    })
  }),
]
