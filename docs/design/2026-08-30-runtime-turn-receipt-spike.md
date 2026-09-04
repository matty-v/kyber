# Runtime turn receipt and recovery spike

**Status:** Complete — runtime receipts, production handshake, and recovery matrix validated
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

### Prototype boundary before production implementation

This validates a positive, pre-model, task-correlated acceptance receipt on
both harnesses. A separate executable
[`mat28-sidecar-handshake`](prototypes/mat28-sidecar-handshake/handshake_test.go)
prototype validates the proposed loopback sidecar protocol over HTTP:

- POST is create-or-read, keyed by the random attempt ID;
- an identical POST replay returns the immutable stored receipt;
- reuse of an attempt ID with different receipt data conflicts;
- after a deliberately dropped POST response following a successful commit,
  an exact GET by attempt ID proves acceptance; and
- when neither POST nor GET proves the exact receipt, the hook fails closed so
  the harness can block model processing.

The protocol prototype passes all four cases on Go 1.26. At the time of the
prototype this resolved the committed-but-response-lost algorithm, but the real
loopback endpoint and PostgreSQL repository did not yet exist. The production
implementation and its destructive recovery evidence are recorded below.

## Production implementation and recovery evidence

The matrix was completed on 2026-09-04 against the guarded
`datawire-dev/us-central1-a/kyber-dev` cluster in namespace `kyber-system`.
All four components used the same worktree image tag
`worktree-20260904154423-7d16514`. The control plane reported a PostgreSQL task
store. Tests used only purpose-built `sol-test-mat28-*` agents; the pre-existing
`echo` agent was not modified.

The destructive matrix ran first on Claude and was then repeated on Codex at
Matt's request. The fresh Codex agent dynamically selected Codex CLI 0.153.2
inside the deployed image whose default remains 0.146.0. The rerun recorded
the deployed Claude image but did not recapture its dynamically selected CLI
version before cleanup; version-specific Claude claims therefore remain
anchored to the 2026-08-30 Claude Code 2.1.251 observation rather than an
inferred version from the image.

MAT-29 had by then implemented the protocol proposed by this spike:

- the dispatcher moves an injected attempt to internal `receipt_pending` and
  does not publish `dispatched` until it accepts the matching hook receipt;
- the status sidecar forwards create-or-read POST and exact-attempt GET calls
  over the pod-loopback trust boundary;
- PostgreSQL stores the immutable runtime, session ID, optional turn ID, and
  random attempt token; and
- both managed harness configurations render the receipt hook at boot.

### Live production-path results

| Case | Observed result |
| --- | --- |
| Positive Claude task | `task_f15eb542bbee25d216604579b7f76563` moved `queued -> dispatched -> completed`; attempt `attempt_27baea7138eb20642470f5d627378dc7` stored Claude session `874c9509-d00c-4dad-be96-0a9da3763885`; response was exactly `RECEIPT_OK` |
| Positive Codex task | on Codex CLI 0.153.2, `task_d53763316ae06a64853fb12c4c9a5c30` moved `queued -> dispatched -> completed`; attempt `attempt_c6483e16ca42341d3f057e3a6b9648c6` stored session `01a06d70-40c4-70d1-8a4f-85d56b29aaa0` and turn `01a06d71-e2b6-77b0-8cbd-4299fa72928c`; response was exactly `CODEX_RECEIPT_OK` |
| Idempotent POST | replaying the identical persisted receipt returned HTTP 200 and did not create a second receipt |
| Conflicting POST | reusing the attempt token with a different session or turn ID returned HTTP 409 |
| Ordinary prompt | the managed receipt command returned 0, created no receipt, and emitted no prompt content into pod logs |
| Receipt service unavailable | the managed receipt command returned 2 |
| Real Claude fail-closed turn | `task_b61b0a3110274ea6c1bf022270e2c4ce` produced a local hook receipt but no database receipt, no task user turn, no assistant execution, and no response; after the two-minute ambiguity lease it terminated as `failed/delivery_unknown` |
| Real Codex fail-closed turn | `task_8992f095f5e10ed6170e8fdfd12c400b` produced a local receipt with native session and turn IDs but no database receipt, no task record in the Codex session transcript, no assistant output, and no response; after the two-minute ambiguity lease it terminated as `failed/delivery_unknown` |
| Control-plane replacement after commit | the completed Claude and Codex tasks stayed at version 3 and their exact receipts remained queryable through newly started control-plane processes |
| Agent pod replacement after commit | both completed tasks and receipts survived new runtime pods; neither attempt was redelivered and no second completion appeared |

The fail-closed Claude transcript contained only an informational hook record
for the rejected task envelope. It contained no corresponding user turn and no
assistant output, confirming the documented exit-code-2 behavior in Kyber's
long-lived TUI integration rather than inferring acceptance from a transcript.
The database event sequence was `task.created`, then
`task.terminal(failed, delivery_unknown)` with no intervening dispatched event.
The Codex fail-closed session likewise contained no record of the rejected task
envelope and no assistant output containing its sentinel response.

The earlier executable prototype remains the deliberately reproducible test
for a response lost *after* commit: an exact GET recovers the immutable receipt.
The production endpoint adds the same create-or-read, replay, conflict, and GET
semantics over PostgreSQL. Together with process and pod replacement above,
this closes the original ambiguity question without requiring transcript
parsing or a custom runtime loop.

### Final classification

- Claude Code: **class 2**, a durable Kyber acceptance receipt built from the
  supported pre-model hook and native session ID, queryable from Kyber's
  database; the harness itself still exposes no supported durable turn query.
- Codex TUI: **class 2**, with the same contract plus a native turn ID in hook
  payloads. A future app-server adapter may reach class 1, but it is not a
  dependency of this design.

An attempt with no reconcilable receipt must still terminate conservatively as
`delivery_unknown`; it must never be blindly redelivered. A persisted receipt
is sufficient to make `dispatched` durable across control-plane and pod
replacement, but it does not make arbitrary model/tool side effects
repeatable.

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
