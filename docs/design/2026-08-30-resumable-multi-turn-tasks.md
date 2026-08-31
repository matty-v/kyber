# Resumable multi-turn agent tasks

**Status:** Proposed
**Date:** 2026-08-30
**Tracker:** [MAT-22](https://linear.app/matty-v/issue/MAT-22/designplatform-resumable-multi-turn-agent-tasks)
**Depends on:** [MAT-19](2026-08-30-durable-agent-tasks.md), [MAT-20](2026-08-30-task-progress-typed-results.md), and [MAT-21](2026-08-30-cooperative-task-cancellation.md)
**Origin:** [MAT-6](https://linear.app/matty-v/issue/MAT-6/spikeplatform-what-formal-a2a-protocol-support-would-require-for-kyber), [A2A gap study](2026-08-30-a2a-protocol-support.md), and gap-study PR #183

## 1. Decision

Extend Kyber's durable native task with one outstanding typed interaction at a
time. An agent may pause for:

- bounded clarification or structured input;
- an explicit caller confirmation/choice; or
- completion of a platform-managed authorization flow.

An authorized caller supplies one idempotent response, and Kyber resumes the
same task as a new harness turn. The common path continues the existing Claude
Code or Codex conversation. If that session is lost, Kyber creates a bounded
recovery capsule from durable task-visible messages, interactions, progress,
and result metadata. It never replays transcripts, hidden reasoning, raw tool
output, environment state, or credentials.

Restart-safe replay is required in v1 by Matt's decision on 2026-08-30. The
task/context identity remains stable, but every resumed harness turn receives a
fresh delivery-attempt token. This keeps stale pods and earlier turns from
responding to or completing the resumed task.

Kyber remains a task coordinator, not a second agent loop or general-purpose
chat service. Arbitrary participants, concurrent branches, unbounded history,
and provider-specific conversation objects are out of scope.

## 2. User outcomes

### Clarification

An automation asks an agent to prepare a deployment. The agent reports
`input_required` with one choice request: which region should it use? The
caller answers `us-central1`, and the same task resumes. Polling callers can
see both the request and the accepted response without reading the harness
transcript.

### Confirmation

An agent prepares a destructive plan and requests a yes/no confirmation. The
recorded response authorizes only the described task step; it does not bypass
Claude Code/Codex sandbox or tool approval policy and does not grant a new
Kyber permission.

### Authorization

An agent needs a GitHub connection. It requests a named platform connection
and bounded scopes. Kyber creates or references an authorization flow. The
caller completes that flow outside task content, and the task resumes with an
opaque connection reference. Tokens, OAuth codes, private keys, passwords, and
secret values never enter task messages.

## 3. Current state and retained boundaries

Attached terminals and chat channels are naturally conversational, but their
messages belong to one long-lived agent session rather than one API task. The
bounded request/reply API accepts one prompt and one final text response. A
caller cannot distinguish a request for clarification from task completion,
cannot safely answer on the original task, and cannot recover after a harness
session disappears.

Retain these boundaries from earlier gaps:

- MAT-19 owns durable task/context identity, dispatch attempts, delivery
  receipts, deadlines, and row-lock concurrency.
- MAT-20 owns task-visible progress, typed result parts, safe update sequence,
  and loopback MCP/internal service.
- MAT-21 owns cancellation races and requires current attempt identity on every
  runtime mutation.
- The status sidecar remains the only pod component with a control-plane
  credential.
- Claude Code and Codex remain the conversation engines. Kyber stores only the
  public task contract needed to resume and serve callers.

## 4. State model

Add public non-terminal states `input_required` and `auth_required`, plus
terminal `rejected`:

```text
queued -> dispatched -> completed | failed
              |
              +-> input_required -- response --> queued -> dispatched
              |
              +-> auth_required --- auth done --> queued -> dispatched
              |
              +-> rejected
              |
              +-> canceling -> canceled | failed(cancel_unconfirmed)
```

The transition back through `queued` is intentional. The continuation is a new
durable delivery with a new attempt token and receipt, even when it is sent to
the same native conversation. The task ID, context ID, messages, results, and
retention remain unchanged.

Internally, an interaction has a pause acknowledgment:

```text
requested -> pause_pending -> paused -> answered -> dispatching -> consumed
                         \-> expired
```

The agent's `request_input` or `request_authorization` call atomically creates
the interaction and changes the public task state. The MCP response instructs
the agent to end its turn immediately. MAT-20 rejects further progress/result
publication from that attempt after the request commits.

The managed normal-turn Stop hook marks the interaction `paused`. A caller may
answer while pause acknowledgment is pending, but the dispatcher does not send
the continuation until `paused`. If the prior turn never stops, the pause
deadline follows the same honest ambiguity policy as cancellation; Kyber never
pastes a follow-up into a still-running unknown turn.

`rejected` is allowed only before any material result or external-work
acknowledgment, with a stable safe reason such as `unsupported`,
`invalid_task`, or `policy_declined`. It is not a substitute for failed after
work begins.

## 5. Interaction types

Exactly one live interaction exists per task. Version one types are:

| Type | Agent supplies | Caller supplies |
| --- | --- | --- |
| `text` | question, optional validation hints | bounded UTF-8 text |
| `choice` | 1–20 stable option IDs and labels | exactly one option ID |
| `confirm` | bounded action summary and consequence | `approved: true|false` |
| `json` | question plus bounded JSON Schema subset | validated JSON value |
| `authorization` | platform connection kind, requested scopes, reason | completed flow or existing opaque connection reference |

The JSON Schema subset permits objects, arrays, strings, booleans, finite
numbers, enums, required properties, lengths, and numeric bounds. It prohibits
remote `$ref`, executable formats, regex features with unbounded complexity,
and schemas beyond the platform depth/token caps.

Choice labels, questions, and action summaries are untrusted agent output.
Clients render them as text, never HTML. A confirmation response records user
intent for this task interaction only. Runtime-native tool approvals remain a
separate harness security boundary.

## 6. Message and context model

Store an ordered task-visible message log independent of the harness
transcript. Messages have stable IDs, sequence, role, kind, typed parts,
creator reference, timestamps, and content digest. Allowed roles are
`caller`, `agent`, and `platform`; allowed kinds are:

- `task_instruction`;
- `input_request` and `input_response`;
- `authorization_request` and `authorization_completed`;
- `continuation_instruction` generated by Kyber; and
- `terminal_summary`.

Agent-authored hidden reasoning, raw shell/tool output, provider events, MCP
credentials, and local filesystem paths are never messages. MAT-20 results are
referenced by stable result ID and safe metadata rather than duplicated inline.

The original prompt is sequence 1. Each interaction request and accepted
response appends one immutable message in the same database transaction as its
state transition. Message IDs and response idempotency keys prevent duplicate
replay.

## 7. Runtime MCP contract

Extend `kyber-task` with:

```text
request_input(
  task_id, attempt_id, interaction_id,
  type, question, options?, schema?, expires_in?
)

request_authorization(
  task_id, attempt_id, interaction_id,
  connection_kind, scopes, reason, expires_in?
)

reject(task_id, attempt_id, rejection_id, code, message?)
```

All calls are idempotent by their supplied ID and complete through the
sidecar-authenticated internal route. The control plane verifies task agent,
current attempt, task state, count/size limits, and absence of another live
interaction.

The tool response confirms durable acceptance and says the agent must stop
task work and end the turn. It does not return a caller response synchronously.
The continuation arrives as a later harness turn. This avoids holding an MCP
request, model request, pod connection, or control-plane replica open for
hours.

The task instruction tells both harnesses to use these tools instead of asking
for input only in prose. Plain transcript questions do not change task state.

## 8. Public continuation APIs

### Read interaction

Task Get includes the one current interaction and a bounded history summary.
It never includes authorization secrets or provider callback material.

### Respond

```http
POST /api/v1/agents/{agent}/tasks/{task}/interactions/{interaction}/respond
Idempotency-Key: <caller-generated key>
Content-Type: application/json

{
  "response": {"optionId": "us-central1"}
}
```

The transaction locks the task and interaction, checks authorization, current
state, interaction ID, expiry, schema, and response hash, appends the caller
message, marks the interaction answered, changes the task to `queued` only
after pause acknowledgment, creates the continuation dispatch row, and
increments task version.

An answer arriving before pause acknowledgment is stored as `answered` but not
dispatched. An identical replay returns the canonical interaction. A different
body under the same key conflicts. A reply to an old, expired, replaced, or
terminal interaction returns `409 stale_interaction` without creating a new
message.

### Complete authorization

Authorization uses a separate endpoint or platform connector callback that
accepts only an opaque flow/reference ID. The task API never accepts a token or
secret-shaped generic string as an authorization response.

```http
POST /api/v1/agents/{agent}/tasks/{task}/interactions/{interaction}/authorize

{"authorizationFlowId":"authflow_..."}
```

Kyber verifies that the flow was created for the same principal, task,
interaction, connection kind, and allowed scopes and is already complete. It
stores a non-secret connection reference in the message log and makes the
credential available through the existing runtime secret/connector boundary.

## 9. Same-session continuation

When the original native session is healthy and the adapter can correlate it
to the paused attempt, the continuation envelope contains:

- task/context ID;
- new attempt ID;
- interaction ID and accepted response message ID;
- response typed parts or completed connection reference; and
- an instruction to continue the existing task and use current durable MCP
  state/results.

It does not repeat the original prompt. The existing Claude Code/Codex
conversation supplies provider-native context. Delivery still requires the
MAT-28 pre-model receipt for the new attempt before public state returns to
`dispatched`.

The adapter never injects a response until the normal Stop/pause receipt proves
the prior turn ended. Generic global idle text or pane scraping is insufficient.

## 10. Restart-safe recovery capsule

If the recorded native session is absent, restarted, cannot be resumed, or
lacks a reliable session query, the dispatcher builds a deterministic recovery
capsule from PostgreSQL:

```text
[kyber-task:<task-id>] attempt=<new-attempt-id> context=<context-id> recovery=1

Original task instruction:
<bounded caller-visible content>

Durable task-visible history:
1. agent requested choice interaction <id>: <question/options>
2. caller answered: <typed response>
3. published results: <IDs, names, kinds, digests, safe metadata>
4. current progress: <message/percent/time>

Continue from this durable state. Do not repeat completed work merely because
the harness session was lost. Query Kyber task tools for canonical state.
```

The capsule is generated from canonical typed records, not concatenated raw
strings. Each section uses explicit delimiters and role/type labels so
participant content cannot masquerade as platform instructions. The envelope
carries a version and SHA-256 digest persisted with the continuation attempt.

Recovery cannot guarantee that a model will not repeat an external side
effect. Durable results/progress and domain idempotency keys reduce that risk,
but MAT-19's no-exactly-once guarantee remains. If safe resumption requires
unrecorded hidden state, the agent must fail the task honestly rather than
invent it.

## 11. Persistence

Add normalized interaction and message tables:

```sql
CREATE TABLE agent_task_interactions (
  id                   TEXT PRIMARY KEY,
  task_id              TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
  attempt_id           TEXT NOT NULL,
  type                 TEXT NOT NULL,
  status               TEXT NOT NULL,
  request_body         JSONB NOT NULL,
  response_body        JSONB,
  request_digest       TEXT NOT NULL,
  response_digest      TEXT,
  response_key_hash    TEXT,
  requested_at         TIMESTAMPTZ NOT NULL,
  pause_acknowledged_at TIMESTAMPTZ,
  answered_at          TIMESTAMPTZ,
  expires_at           TIMESTAMPTZ NOT NULL,
  consumed_at          TIMESTAMPTZ,
  CHECK (type IN ('text','choice','confirm','json','authorization')),
  CHECK (status IN ('pause_pending','paused','answered','dispatching','consumed','expired'))
);

CREATE UNIQUE INDEX one_live_task_interaction
  ON agent_task_interactions(task_id)
  WHERE status IN ('pause_pending','paused','answered','dispatching');

CREATE TABLE agent_task_messages (
  task_id          TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
  sequence         BIGINT NOT NULL,
  id               TEXT NOT NULL UNIQUE,
  role             TEXT NOT NULL,
  kind             TEXT NOT NULL,
  parts            JSONB NOT NULL,
  content_digest   TEXT NOT NULL,
  created_by       TEXT NOT NULL,
  created_at       TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (task_id, sequence),
  CHECK (role IN ('caller','agent','platform'))
);

CREATE TABLE agent_task_continuations (
  task_id             TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
  interaction_id      TEXT NOT NULL REFERENCES agent_task_interactions(id),
  attempt_id          TEXT NOT NULL UNIQUE,
  mode                TEXT NOT NULL CHECK (mode IN ('same_session','recovery')),
  context_digest      TEXT NOT NULL,
  status              TEXT NOT NULL,
  lease_owner         TEXT,
  lease_until         TIMESTAMPTZ,
  created_at          TIMESTAMPTZ NOT NULL,
  updated_at          TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (task_id, interaction_id)
);
```

Interaction request/response JSON contains only its validated type-specific
shape. Authorization provider state and credentials live in their dedicated
secret/connection store; task tables retain opaque references and safe scopes.

## 12. Races and failure behavior

| Race or failure | Required behavior |
| --- | --- |
| Two simultaneous input requests | Row lock/unique index accepts one; other conflicts |
| Agent requests input then keeps working | Later task mutations from that attempt reject; wait for Stop |
| Caller answers before Stop | Store answer; do not dispatch until pause acknowledgment |
| Duplicate response, same key/body | Return canonical accepted interaction |
| Duplicate key, different body | `409 idempotency_conflict` |
| Reply to old interaction | `409 stale_interaction`; never apply to current question |
| Pause expires before answer | Mark interaction expired and task failed/input_timeout |
| Authorization flow expires | Task failed/auth_timeout; delete provider transient state |
| Cancel races input response | MAT-21 row lock decides; cancel prevents later continuation |
| Completion races request_input | First valid transition wins; no terminal task reopens |
| Same-session delivery response lost | MAT-28 receipt query reconciles exact new attempt |
| Harness restarts before continuation | Build recovery capsule with same task, new attempt |
| Harness restarts after receipt | MAT-19 ambiguity/reconciliation applies; no blind duplicate |
| Recovery capsule over cap | Fail context_too_large; never truncate silently |
| Credential appears in generic response | Reject and direct caller to authorization endpoint |
| Revoked connection before resume | Remain/return auth_required or fail auth_revoked safely |

Input/auth expiry defaults to 24 hours, capped at seven days and never later
than the task deadline. Expiry is a terminal failure in v1; callers create a
new task rather than reviving an interaction whose runtime context may be
gone.

## 13. Limits and retention

| Limit | Default | Compiled maximum |
| --- | ---: | ---: |
| Interactions per task | 16 | 64 |
| Outstanding interactions | 1 | 1 |
| Question/action summary | 4 KiB | 16 KiB |
| Text response | 16 KiB | 64 KiB |
| Choice options | 20 | 100 |
| Option ID/label | 128 B / 512 B | 256 B / 1 KiB |
| JSON response | 64 KiB | 256 KiB |
| Task-visible message count | 64 | 256 |
| Total replayable context | 128 KiB | 512 KiB |
| Pause duration | 24 h | 7 d |

Kyber does not model-summarize older context because that would create another
unreviewed semantic agent step. When a task reaches the replayable context
cap, new interaction requests fail with `context_capacity_exhausted`; the
agent can complete/fail with existing results. No content is silently dropped.

Messages and interactions inherit task retention. Authorization transient
state uses the shorter of provider expiry, interaction expiry, or installation
policy. Deleting/expiring the task removes task records and object results via
MAT-20 cleanup, then revokes task-specific authorization grants where the
connector supports it.

## 14. Authorization and confused-deputy controls

- MAT-23 defines which principal can view and answer each interaction. Until
  then, only the provisional task creator and administrators may respond.
- Possession of a task, interaction, message, or flow ID is never authority.
- A participant's response is untrusted task input, not a platform command.
  Recovery rendering preserves role/type boundaries and never promotes it to a
  system instruction.
- Confirmation cannot expand the agent's sandbox, tool approval, cluster RBAC,
  task ownership, or connector scopes.
- Authorization requests select only registered connection kinds and
  installation-allowed scopes. Agent-supplied callback URLs, OAuth endpoints,
  client IDs, secret names, and raw credential requests are rejected.
- The completed flow is bound to principal, task, interaction, provider,
  scopes, nonce, and expiry. A reference from another task cannot be replayed.
- Logs and traces contain safe IDs, types, sizes, state, and outcome, never
  question/response bodies, credentials, URLs with codes, or provider tokens.
- Public UIs render every message as escaped untrusted content and clearly
  distinguish agent request, caller response, and platform authorization.

## 15. Observability

Metrics:

- interactions requested/answered/expired by safe type and outcome;
- current input-required/auth-required counts and oldest age;
- pause acknowledgment latency and failures;
- continuation dispatch attempts, same-session versus recovery mode, receipt
  reconciliation, and context-too-large failures;
- stale/duplicate/idempotency conflicts;
- authorization flow started/completed/expired/revoked; and
- terminal races with cancellation/completion.

Task and interaction IDs are trace/log fields, not metric labels. Audit events
record request, pause acknowledgment, response accepted, authorization
completed, continuation dispatched/received, recovery mode, and terminal
outcome as separate facts.

## 16. API and channel presentation

The native API is authoritative. PWA, Telegram, and Discord may render an
interaction with buttons or forms, but a chat message is not accepted merely
because it follows a question. The channel adapter submits the explicit task,
interaction, response, principal, and idempotency key through the same API.

Buttons use stable option IDs rather than labels. Expired keyboards are removed
or return a stale-interaction message. Authorization links are generated by
Kyber and scoped to the authenticated operator; channel adapters never receive
the resulting credential.

G7 later adds resumable subscriptions. MAT-22 polling and ordinary channel
notifications do not define an event-stream cursor.

## 17. A2A projection

The later A2A edge maps:

- native `input_required` to A2A input-required status plus a task-visible
  Message;
- native `auth_required` to A2A auth-required only for supported registered
  flows;
- accepted follow-up content to a new A2A Message on the same task/context;
- rejected and timeout outcomes to their honest terminal state/error; and
- native typed parts to the MAT-20 Artifact/Part projection.

The edge cannot accept raw credentials, arbitrary participants, or a message
that the native principal policy rejects. Provider session IDs and recovery
capsules remain internal.

## 18. Rollout, tests, and estimate

1. Add message/interaction schema and read-only task fields behind a feature
   gate.
2. Add request-input, pause acknowledgment, text/choice/confirm response, and
   same-session continuation.
3. Add JSON validation, rejection, expiry reconciliation, and channel/PWA
   presentation.
4. Add registered authorization flows and opaque connection references.
5. Enable restart-safe recovery capsules after deterministic rendering,
   context cap, receipt, restart, and prompt-boundary tests pass.

Tests cover every transition/race above, multi-replica row locks, idempotency,
JSON/schema fuzzing, prompt-boundary injection, authorization binding,
credential redaction, same-session continuation, session loss before/after
receipt, recovery context determinism, both runtimes, channels, cleanup, and
A2A projection fixtures.

Estimated implementation is four to six engineer weeks after MAT-19/MAT-20:

- one to two weeks for persistence, typed input, APIs, and MCP;
- one week for pause/continuation dispatch and runtime tests;
- one to two weeks for authorization integration and security tests; and
- one week for recovery capsules, restart matrix, rollout, and UI/channel
  presentation.

## 19. Non-goals and recommendation

Non-goals:

- arbitrary participant chat or shared rooms;
- multiple outstanding questions, branching, merge, or message edits;
- raw transcript/history export or hidden-reasoning replay;
- raw secrets in task content;
- bypass of native harness approvals;
- streaming subscriptions (MAT-25); and
- implementation in this design issue.

Approve MAT-22 with restart-safe replay. It formalizes a useful Kyber-native
pause/resume workflow while keeping provider harnesses in charge of reasoning
and tools. The bounded public message log is the minimum durable context needed
to survive a lost session; it is not a replacement conversation engine.

