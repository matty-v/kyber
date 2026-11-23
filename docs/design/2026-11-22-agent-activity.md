# MAT-216: current work inventory and Agent activity

Status: implementation in progress; not qualified for release. The operator
approved the issue and collapsible mockup. The draft must not merge as a complete
activity fix until native producers, ingestion and Datawire verification exist.

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

## Remaining release gates

1. Controlled native qualification now has authenticated GCP access to the
   explicitly designated empty `datawire-dev/us-central1-a/kyber-dev` cluster.
   PR431 head 3364e4e passed an empty-cluster embedded UI/API smoke check there:
   exact serving SHA, independent collapse/reload persistence, desktop/mobile,
   zero page errors and no horizontal overflow. This does not prove native job
   lifecycle reporting. Original Datawire Mando/stopped/background captures
   remain unavailable; do not confuse that production install with the test target.
2. Qualified native inventory producers for every registered harness. Do not
   silently narrow the accepted scope or infer completeness from old transcripts.
3. The draft now includes platform envelope admission: a purpose-separated
   credential in a separate Secret mounted only in the status sidecar (the
   legacy pod token is also mounted in the runtime and is not sufficient).
   Registration uses direct Kubernetes reads of the current Agent UID, owned
   Pod UID and running native sidecar ContainerID/StartedAt, then issues an epoch.
   Inventories carry a monotonic sequence and original native snapshot time;
   stale, future, duplicated and out-of-order observations cannot renew a lease.
   Optimistic status patches protect admission against concurrent updates.
   Container replacement invalidates the epoch while retaining historical last
   work time. Targeted unit tests and the real-API-server schema/lifetime test pass;
   installed-runtime canaries and full CI remain qualification gates.
4. Generated CRDs/deepcopy if persistent inventory fields are added; matching
   OpenAPI/TS wire contract and fake-cluster/API ordering/replacement tests.
5. Native defaults/reset/latest and provider/auth regression checks, shared UI
   and embedded builds, contract tests, review and CI.
6. Runtime/control-plane rollout as required, package publication, reviewed
   Holocron dependency update and production deployment, then Datawire browser
   verification on desktop/mobile. No production deployment is authorized by
   this document, and none has been performed for this task.

## Current native interface constraint

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

Matt explicitly reaffirmed the full Claude Code/Codex/Hermes scope. PR 431
remains draft until all three are qualified; green checkpoint CI is insufficient
for merge. GitGuardian's test-fixture incident is resolved.
