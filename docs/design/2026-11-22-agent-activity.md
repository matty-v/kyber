# MAT-216: current work inventory and Agent activity

Implementation and qualification: [PR 431](https://github.com/matty-v/kyber/pull/431).
The operator approved all three runtimes and the independently collapsible UI.
The PR records exact-head CI, review findings and final controlled test evidence.
Production rollout and verification are separate from test-cluster qualification.

Issue: https://linear.app/matty-v/issue/MAT-216/fix-dashboard-activity-reporting-and-redesign-recently-active

## Shared contract

`pkg/activity` projects a complete, current native inventory with lifecycle
precedence. Stopped or pending-stop agents are excluded. Missing contact,
missing/partial inventory and expired observations are unavailable, never idle.
Foreground work wins over background work without double-counting an agent.
Verified human input can remain visible beyond the 24-hour idle-history cutoff.
The proposed bounds are a 15-second observation cadence, 60-second work lease,
90-second contact lease and 1024 foreground/background handles per inventory.
These are platform bounds, not native API guarantees.

The public optional `activity.work` projection carries state, serverTime,
expiresAt, observedAt, lastWorkedAt, stateSince, safe counts, needsInput and a
reason. It omits process ids, native handles, commands, outputs and credentials.
The wire type is populated only from admitted native snapshots; legacy
reports cannot populate it. Legacy state is not treated as a live work lease. The frontend
anchors the server baseline conservatively at request start with a monotonic
clock and carries that anchor through React Query structural sharing. Charging
the full round trip prevents a delayed response from extending the work lease.
Cached views expire without another successful request; Waiting history sorts
by native last-work time rather than observer restart/contact time.

Agent General/list badges and dashboard use the same projection. The dashboard
has four independently collapsible groups with persistent per-connection boolean
preferences and always-visible counts. A stopped preview target is not silently
replaced when explicitly selected. A focused row moving into a collapsed group
returns focus to its destination header without opening the group.

## Native qualification evidence

The registry contains Claude Code, Codex and Hermes. Neither provider/model
choice nor token accounting is proof of active work. Versions below are source
qualification points, not a promise that every installed version implements
these interfaces.

| Harness | Evidence | Remaining qualification/integration |
| --- | --- | --- |
| Claude Code 2.1.330 | Read-only native mod observes turn boundaries, native agent scheduling and tool-result identities. A Bash-specific shell override registers the native child PID/start-time before exact exec; it does not intercept tool execution or alter MCP argv. Staged GCP native TUI reported Background work after its foreground turn ended. | Packaged boot/default/latest, native subagent scheduling and remaining lifecycle/failure cases remain release gates. |
| Codex 0.171.0 | Live native WebSocket control socket provides loaded/read/backgroundTerminals/list and thread/turns/list. Controlled GCP idle, silent foreground, foreground-to-background, job exit and short-turn history checks passed. | Revalidate packaged default/reset/latest. The installed old 0.146.0 server rejects thread/turns/list; old pins fail closed rather than invent activity. |
| Hermes 0.21.0, 29112bef099274229cadff79cdff7bf7b99c4b77 | In-process native turn boundaries plus ProcessRegistry publish count-only snapshots, validated by owner PID/start-time and pod scope. GCP silent foreground, cancellation, two background jobs exiting independently and blocked-publisher recovery passed. | Packaged revalidation, explicit background cancellation and remaining restart/auth regressions remain release gates. |

Sources:

- https://code.claude.com/docs/en/hooks
- https://github.com/openai/codex/blob/rust-v0.169.0/codex-rs/rollout/src/policy.rs
- https://github.com/openai/codex/blob/rust-v0.169.0/codex-rs/app-server-protocol/src/protocol/v2/thread.rs
- https://github.com/openai/codex/blob/rust-v0.169.0/codex-rs/core/src/unified_exec/async_watcher.rs
- https://github.com/NousResearch/hermes-agent/blob/29112bef099274229cadff79cdff7bf7b99c4b77/tools/process_registry.py
- https://github.com/NousResearch/hermes-agent/blob/29112bef099274229cadff79cdff7bf7b99c4b77/agent/turn_finalizer.py

## Qualification and rollout requirements

1. Use only the explicitly designated `datawire-dev/us-central1-a/kyber-dev`
   test cluster for native canaries. It is distinct from Datawire production.
   Verify the serving SHA, fresh/default/latest boots, current native work,
   cancellation, job exit, observation loss/recovery and lifecycle exclusion.
2. Qualify every registered harness; provider/model choice, metadata updates,
   process CPU and legacy transcript state cannot stand in for native work.
3. Preserve purpose-separated sidecar admission, direct Agent/Pod/container
   lifetime checks, ordered original timestamps and optimistic status writes.
   Unit and real-API-server checks cover schema, replay, conflict and replacement.
4. Match CRD/deepcopy, public OpenAPI and TypeScript contracts. Validate shared
   UI and embedded builds, independent collapse persistence, desktop/mobile,
   error-free rendering and no horizontal overflow.
5. Preserve explicit version pins, custom shells, authentication profiles,
   native background handoff, MCP arguments, PVC data and individual disk usage.
   Review findings must be fixed and exact-head CI green before merge.
6. Production runtime/control-plane rollout, package publication, Holocron
   dependency delivery and Datawire browser verification are later delivery
   steps. No production rollout is authorized by this document.

## Native interface boundaries

Claude Code's pinned 2.1.119 artifact was inspected alongside its official hook
reference. Its task registry includes local Bash tasks, but the structured
`task_started`/`task_updated` emission inspected in that artifact is gated to
non-interactive SDK/print mode. TaskCompleted is the work-list hook, not Bash
exit. Status-line input does not include that registry. Current documented mods
require newer versions, and their `agent.list` is explicitly subagents/teammates,
not Bash terminals. A `tool.call` observer is unsuitable as a transparent fix:
the current declarations warn that matching a tool can disable its background
handoff. Do not patch the native binary, switch the operator's TUI to print mode,
or count every shell/MCP service as work to sidestep the missing qualification.
The new 2.1.330 adapter uses native turn/agent scheduling and result identities,
plus a Bash-only exec wrapper, without intercepting tool.call/tool.run. No command
text, outputs or prompts are persisted or sent to the platform. Uncorrelated
native work, unsupported operator shell overrides, stale native publishers or
missing scheduling provenance produce unavailable coverage, never idle.

Baked and chart default versions are now Claude Code 2.1.330 and Codex 0.171.0.
Explicit operator pins remain unchanged. Older pinned interfaces are not claimed
as qualified. Claude boot seeds the new renderer upsell acknowledgement while
preserving the operator's actual renderer choice. The inline launcher carries
observer environment through initial launch, resume and watchdog restart.

Primary interface evidence:
- https://github.com/openai/codex/blob/rust-v0.146.0/codex-rs/app-server-protocol/src/protocol/common.rs
- https://github.com/openai/codex/blob/rust-v0.169.0/codex-rs/app-server-protocol/src/protocol/v2/thread.rs
- https://github.com/openai/codex/blob/rust-v0.169.0/codex-rs/app-server-transport/src/transport/unix_socket.rs
- https://code.claude.com/docs/en/plugins/mods/create
- https://github.com/anthropics/claude-code/blob/main/mods/types/claude-code.d.ts

## Controlled test-cluster checkpoint (2026-11-23)

The disposable `mat216-codex-canary` on `kyber-dev` uses an inert local model
stub and no operator model credentials. Managed latest resolved to 0.171.0.
Direct native reads verified idle, a silent 45-second foreground request,
foreground completion while a native 75-second terminal remained running,
and the terminal's eventual exit. Completed-turn timing also survives between
polls. An unrelated model-stub process does not count as native work.

Pinning the canary through the supported API to image-default 0.146.0 preserved
its PVC sentinel. That release's default TUI did not expose a live control
socket. A separate native app-server bound to a private Unix socket accepts its
standard `--remote` TUI, but the installed server rejects `thread/turns/list`;
protocol declarations alone do not establish installed method support. This
compatibility path is not qualified; the new baked default is 0.171.0.

Matt explicitly reaffirmed the full Claude Code/Codex/Hermes scope. Green CI
alone does not replace native qualification. Final qualification and merge status
are recorded in PR 431; GitGuardian's test-fixture incident is resolved.


## Codex history and current-work coverage

Codex 0.171.0 explicitly rejects persisted history reads on a newly loaded,
unmaterialized thread before its first user message. Only that exact native
response on an idle thread permits empty initial history; unsupported methods,
other invalid requests and a different thread identity remain coverage errors.

Native ephemeral threads explicitly have no persisted turn history. Their live
status and background-terminal inventory still contribute current work; absence
of a history API does not make those native counts incomplete. Ephemeral work
updates last-work time when observed running. Metadata `updatedAt` is never used
as a replacement activity timestamp. Persistent native turn timing retains short
completed turns between observer polls.

## Controlled qualification evidence

GCP canaries verified silent foreground and native background work, cancellation,
independent native job exits, stale-publisher unavailability and recovery, and
CLI/pod lifetime transitions. Fresh packaged Claude/Codex/Hermes reporters each
produced complete zero-work inventories. Claude and Codex Latest/reset paths
preserved PVC test data. A running background Hermes agent became excluded as
soon as stop intent was accepted. The live dashboard passed independent collapse
persistence, desktop/mobile, no page errors and no horizontal overflow. These
checks used inert local model stubs and disposable agents, without operator model
credentials. Auth profile regressions are additionally covered by boot fixtures.
Detailed final-head evidence and any follow-up findings are in PR 431.
