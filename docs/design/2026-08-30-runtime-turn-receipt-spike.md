# Runtime turn receipt and recovery spike

**Status:** Prototype validated; sidecar handshake and destructive cut-point matrix pending
**Date:** 2026-08-30
**Tracker:** [MAT-28](https://linear.app/matty-v/issue/MAT-28/spikeruntime-native-turn-receipt-and-recovery-capabilities)
**Consumer:** [MAT-19](https://linear.app/matty-v/issue/MAT-19/designplatform-durable-externally-addressable-agent-tasks)

## Question

Can Kyber obtain a supported, durable receipt that a particular task envelope
entered a Claude Code or Codex turn, so a control-plane crash after delivery
does not force either blind redelivery or an ambiguous terminal outcome?

This spike tests Kyber's supported long-lived interactive harness model. It
does not replace either harness with a custom loop, use transcripts as task
state, or treat a session ID as equivalent to a task/turn ID.

## Versions and evidence sources

| Runtime | Kyber production pin | Evidence available in this environment |
| --- | --- | --- |
| Claude Code | 2.1.119 | Kyber managed-hook wiring/tests plus the official Claude Code hooks reference; binary not installed locally |
| Codex | 0.146.0 | Kyber's prior real-TUI hook spike and tests against 0.146.0 |
| Codex comparison | 0.151.0 | Installed official CLI and generated app-server v2 JSON schemas |

The production pins come from
[`deploy/helm/kyber/values.yaml`](../../deploy/helm/kyber/values.yaml). The
Codex 0.146.0 behavioral evidence is recorded in
[`images/codex/INSTALL_NOTES.md`](../../images/codex/INSTALL_NOTES.md).
Live tests must also record the deployed image and installed runtime because
`Agent.spec.runtimeVersion` may override the image pin.

## Preliminary capability matrix

| Capability | Claude Code 2.1.119 path | Codex 0.146.0 path | Codex 0.151.0 app server |
| --- | --- | --- | --- |
| Stable session ID | `UserPromptSubmit.session_id` | observed `UserPromptSubmit.session_id` | `thread/start`, `thread/resume`, `thread/read` |
| Prompt acceptance boundary | managed `UserPromptSubmit` before model processing | managed `UserPromptSubmit` before model processing | `turn/start` response and `turn/started` notification |
| Stable turn ID | not documented in hook payload | observed `turn_id` in Kyber's 0.146.0 spike | turn response, notification, and list schemas |
| Durable turn query | no supported CLI query established | no supported query established through current TUI | thread read/resume and turn listing are present |
| Completion signal | `Stop` hook plus Kyber explicit MCP completion | `Stop` hook plus Kyber explicit MCP completion | `turn/completed` plus Kyber explicit MCP completion |
| Current Kyber integration | long-lived tmux TUI | long-lived tmux TUI | not used by the pinned image |

The official
[Claude Code hooks reference](https://code.claude.com/docs/en/hooks) states
that `UserPromptSubmit` runs before Claude processes the prompt and includes
`session_id` and exact `prompt`. It also states that exit code 2 blocks
processing and erases the prompt from context. The hook is therefore a
supported positive acceptance boundary, not transcript inference.

Kyber's Codex 0.146.0 spike drove a real TUI against a stub Responses API and
observed the same managed hook with `session_id`, `turn_id`, and exact `prompt`.
The managed configuration avoids the user-hook trust prompt.

The installed Codex 0.151.0 binary generated official app-server v2 schemas
containing `thread/start`, `thread/resume`, `thread/read`, `turn/start`,
`turn/started`, `turn/completed`, thread turn listing, and explicit thread/turn
IDs. This is evidence of a potentially stronger future Codex adapter, not proof
for Kyber's pinned 0.146.0 TUI integration.

## Prototype design

### Common managed-hook receipt

Add a disposable managed `UserPromptSubmit` hook for each harness. The hook:

1. reads the exact prompt and extracts a strict task-envelope header plus
   random execution-attempt token;
2. ignores every prompt that is not a Kyber task envelope;
3. posts task ID, attempt token, runtime, session ID, optional native turn ID,
   and hook timestamp to a pod-loopback sidecar endpoint;
4. lets the sidecar authenticate as the pod and idempotently persist the
   receipt in the control plane; and
5. returns without adding output to model context.

The receipt means **the harness invoked its pre-processing hook for this exact
prompt**. It does not mean the first model request began, the turn is still
running, or side effects are safe to repeat.

The production design must decide failure behavior carefully. Blocking the
prompt when receipt persistence fails avoids untracked execution, but a lost
HTTP response after the database commit can leave Kyber believing the prompt
was accepted while the hook blocks it. The prototype must test an idempotent
POST-then-query recovery handshake before claiming this closes the ambiguity
window.

### Codex app-server comparison

Separately test whether the pinned or an explicitly upgraded Codex runtime can
host the TUI on its app-server daemon while Kyber submits `turn/start` and later
queries the same turn. The test must preserve the long-lived interactive agent,
managed hooks, MCP tools, approvals, credentials, and `/persist` behavior. A
headless one-process-per-task run is not an acceptable substitute.

Even if successful, this is a Codex-specific stronger capability. The native
task service can store an opaque adapter receipt and expose one public
lifecycle while Claude Code uses the common hook receipt.

## Required failure matrix

For each pinned runtime:

| Cut point | Evidence to capture | Safe expected outcome |
| --- | --- | --- |
| Before tmux paste | no hook receipt | retry permitted |
| After paste, before Enter | no hook receipt | retry only after proving input was cleared/not submitted |
| After Enter, before hook POST | hook eventually posts or blocks processing | no blind immediate retry |
| Hook POST committed, response lost | idempotent query resolves receipt | allow once or fail closed without false `dispatched` |
| Hook process exits nonzero | verify whether prompt proceeds for that harness/version | outcome must be version-pinned |
| Control-plane restarts during hook | sidecar retry/query behavior | at most one accepted attempt token |
| Harness/TUI restarts after receipt | query session/turn where supported | task survives; execution status remains honest |
| Pod restarts after receipt | test session resume and old attempt rejection | never redeliver automatically without evidence |

Also verify that ordinary Telegram, terminal, and scheduled-job prompts do not
create receipts, and that task hooks do not leak prompt content into logs.

## Preliminary conclusion

The earlier MAT-19 draft was too pessimistic in saying neither adapter exposes
a receipt. Both pinned integrations have a supported pre-model
`UserPromptSubmit` hook that can become a positive, task-correlated acceptance
receipt through the existing sidecar trust boundary. Codex additionally has a
native `turn_id` in the verified hook payload.

That is not yet a durable queryable turn contract. Until the failure matrix is
run, MAT-19 must retain `delivery_unknown` as a fallback and must not
automatically redeliver after the hook ambiguity boundary. If the sidecar
handshake proves fail-closed and recoverable, the public transition to
`dispatched` should require the persisted hook receipt rather than the return
from tmux paste.

Codex 0.151.0's app server appears capable of a stronger thread/turn adapter,
but adopting it requires an explicit runtime-version and architecture decision.
It cannot silently become a dependency while Kyber pins 0.146.0.

## Live dev-cluster evidence

The prototype ran on 2026-08-30 in the guarded `datawire-dev` / `kyber-dev`
cluster, namespace `kyber-system`, using purpose-built agents on `peek-test`.
The existing unrelated `echo` agent was not modified.

| Runtime | Deployed image | Installed interactive runtime | Result |
| --- | --- | --- | --- |
| Codex | `codex:mat16-20260828233132-8264b3d` | 0.151.0 | exact task and attempt correlated to native session and turn IDs |
| Claude Code | `claude-code:mat16-20260828233132-8264b3d` | TUI reported 2.1.251; image default was 2.1.119 and `latest` was requested | exact task and attempt correlated to native session ID; no turn ID |

For each runtime, Kyber delivered a real task envelope through
`kyber-job-dispatch`. The disposable managed `UserPromptSubmit` hook persisted
only the task ID, attempt ID, event, session ID, and optional turn ID under
`/persist`; it did not persist prompt or transcript content. The harness then
consumed the same envelope and produced the exact expected response.

The observed receipts were:

- Codex: task `task_11111111111111111111111111111111`, attempt
  `attempt_22222222222222222222222222222222`, session
  `01a053cf-4daa-73d0-84a1-282d32f9ed99`, turn
  `01a053cf-a7b4-7811-9eb1-e31b92954d2a`.
- Claude Code: task `task_33333333333333333333333333333333`, attempt
  `attempt_44444444444444444444444444444444`, session
  `d9fe597e-cd64-472a-ae95-90e072654e00`, with an empty optional turn ID.

Both receipt files survived a Kyber session restart. Claude's settings-backed
disposable hook also survived that restart. Codex's file under
`/etc/codex/managed_config.toml` was regenerated and the disposable addition
was removed, so a production Codex hook must be rendered by the runtime boot
path rather than patched into a live container.

The test also confirmed an operational namespace constraint: a plain
Kubernetes exec could not see the runtime tmux socket. Dispatch succeeded when
run as the `kyber` user inside the runtime mount namespace, matching how the
platform must enter the isolated agent environment.

The purpose-built agents `sol-test-mat28-codex` and
`sol-test-mat28-claude` were explicitly deleted after evidence capture. The
only remaining agent on `peek-test` was the pre-existing `echo` agent.

### What remains unproven

This validates a positive, pre-model, task-correlated acceptance receipt on
both harnesses. It does not yet validate the sidecar POST-then-query protocol,
a lost response after database commit, a control-plane restart during the
hook, hook exit-code behavior at the deployed versions, or automatic recovery
after a pod restart. Those cut points require an implementation of the
loopback receipt endpoint rather than a file-only evidence hook. Until then,
an attempt crossing that boundary without a reconcilable receipt remains
`delivery_unknown` and must not be blindly redelivered.

## Recommendation for MAT-19

- Add receipt fields and a `receipt_pending` internal dispatch state to the
  schema; do not expose a new public state.
- Require a matching execution-attempt token on hook receipt and completion.
- Define `dispatched` as a persisted positive harness receipt, not successful
  tmux injection.
- Allow retry only before the hook ambiguity boundary is crossed.
- Preserve `delivery_unknown` for an attempt that cannot be reconciled.
- Keep the adapter receipt opaque so Codex can later store thread/turn IDs while
  Claude Code stores session/hook identity.
