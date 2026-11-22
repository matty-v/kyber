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
The wire type is reserved in this draft; production serialization does not yet
populate it. Legacy state is not treated as a live work lease. The frontend
anchors the server baseline at response receipt with a monotonic clock, including
React Query's structurally shared result, and expires cached views without
waiting for another successful request.

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
| Claude Code | Official hook reference documents UserPromptSubmit, Stop, PreToolUse, PostToolUse, failure and subagent hooks. TaskCompleted is an agent-team task event, not a shell-exit event. | Qualify the installed release's background Bash inventory/exit path, silent waits, interrupt/error handling and subagent coverage. Do not claim task hooks solve background shell exits. |
| Codex 0.169.0 | Installed CLI advertises app-server daemon/proxy. Pinned native protocol has loaded-thread/read status and live command start/end events. Rollout policy explicitly drops ExecCommandBegin/End/OutputDelta. | Qualify a read-only live inventory/event connection, completeness on reconnect and native job exits. The local proxy initialize probe timed out without mutations; discovery is not a validated adapter. Image default remains 0.146.0, so default/reset/latest compatibility and any required native upgrade must be explicitly tested. |
| Hermes 0.21.0, 29112bef099274229cadff79cdff7bf7b99c4b77 | Native ProcessRegistry tracks background sessions with identity/exit state and detached ownership; hooks cover pre_llm_call and on_session_end, the latter includes completed/failed/interrupted outcomes. | Add a native in-process inventory publisher, qualify thread/session scope, cancellation, partial registry access and controlled failure. A shell hook seeing only one tool result cannot prove the whole registry is empty. |

Sources:

- https://code.claude.com/docs/en/hooks
- https://github.com/openai/codex/blob/rust-v0.169.0/codex-rs/rollout/src/policy.rs
- https://github.com/openai/codex/blob/rust-v0.169.0/codex-rs/app-server-protocol/src/protocol/v2/thread.rs
- https://github.com/openai/codex/blob/rust-v0.169.0/codex-rs/core/src/unified_exec/async_watcher.rs
- https://github.com/NousResearch/hermes-agent/blob/29112bef099274229cadff79cdff7bf7b99c4b77/tools/process_registry.py
- https://github.com/NousResearch/hermes-agent/blob/29112bef099274229cadff79cdff7bf7b99c4b77/agent/turn_finalizer.py

## Remaining release gates

1. Authenticated, read-only Datawire captures for Mando, stopped agent and owned
   background work, including installed native versions. Public cluster-info is
   accessible, but this session's agent API request returned 401. Requested
   operator credential delivery as USER_DATAWIRE_KYBER_API_KEY; no key in chat.
2. Qualified native inventory producers for every registered harness. Do not
   silently narrow the accepted scope or infer completeness from old transcripts.
3. Platform-side envelope/identity, ordering and freshness admission. The
   runtime's self-supplied pod UID is not authority. Persist accepted inventory
   with current Agent/pod/observer epoch and monotonic sequence, reject replay,
   protect concurrent heartbeat updates, then populate the public projection.
4. Generated CRDs/deepcopy if persistent inventory fields are added; matching
   OpenAPI/TS wire contract and fake-cluster/API ordering/replacement tests.
5. Native defaults/reset/latest and provider/auth regression checks, shared UI
   and embedded builds, contract tests, review and CI.
6. Runtime/control-plane rollout as required, package publication, reviewed
   Holocron dependency update and production deployment, then Datawire browser
   verification on desktop/mobile. No production deployment is authorized by
   this document, and none has been performed for this task.
