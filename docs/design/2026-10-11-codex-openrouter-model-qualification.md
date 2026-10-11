# Codex OpenRouter model qualification

MAT-115 records **observations**, not a support promise for every request on a
model ID. OpenRouter may choose a different upstream provider after the test.
Record the Codex image digest and installed version, model ID, route URL,
observed upstream (or `unknown`), date, task IDs, API terminal states, and any
route/provider settings used for each run. Never put API keys or prompt content
in the evidence file.

## Fresh Kyber dev run (2026-10-11)

On `datawire-dev/kyber-dev` at main `c48c8d7`, a disposable Codex agent used
`openai/gpt-4.1-mini` through the OpenRouter Responses endpoint. The running
harness reported `0.162.1` from its installed overlay; the baked image CLI is
`0.146.0`. The agent image digest was
`sha256:3b2a5d14a342a6f4d4f4908046a579e874da43c6413bf1fb836b1f69a2c973c4`.
The upstream provider was not observable. [Redacted task evidence](fixtures/2026-10-11-gpt41mini-task-contract.json)
uses synthetic ID aliases while preserving task/attempt binding, receipts,
marker-only completions, and terminal states. It contains no credentials or
prompt content.

| Task | Receipt and model tool call | API outcome |
| --- | --- | --- |
| First completion | Exact task/attempt receipt; `complete` returned its own `UNIQUE-RESPONSE-1` marker | `completed` |
| Second completion | Exact task/attempt receipt; `complete` returned its own `UNIQUE-RESPONSE-2` marker | `completed` |
| Cancellation | Exact task/attempt receipt; Codex said it would monitor cancellation, then ended the turn without `get_control` or `ack_cancel` | `failed`, `cancel_unconfirmed` after five minutes; `cancel.applied=false` |

The cancellation request above arrived **after** Codex ended that turn. It
establishes a quiescent-turn failure, not an active-turn result. A separate
active-turn test on the same image requested cancellation while Codex was
working. Codex called `get_control` and saw `cancel_requested=true`, then
called `ack_cancel` twice without `task_id` and with an `attempt_` value as
`acknowledgment_id`. Both calls were rejected by the sidecar's generic
validation error. The model ended its turn without acknowledging cancellation.
This exposes an avoidable tool-guidance problem in addition to the route's
unqualified cancellation behavior. The sidecar now describes the required
fields and generated `ack_` ID and returns field-specific validation errors.
This guidance change needs a fresh live qualification run before any support
claim changes.

The earlier pilot had one successful cancellation. This fresh failure means
task cancellation is **not qualified** for this route and model. The evidence
descriptor reports `task-cancel: fail`; it must not imply repeatable support
from a single earlier pass. The evaluator correctly fails the redacted file
because its cancellation acknowledgment is absent. After the run, the
disposable agent and managed credential were deleted; dev returned to zero
Agents with durable tasks disabled.

## Three-task fixture

Run this only on the dedicated Kyber GCP dev cluster, with a disposable Agent.
The run uses two sequential completion tasks and one cancellation task. Give
each task a distinct `UNIQUE-RESPONSE-N` marker. The harness receipt hook
records delivery; ask the model to call the completion/cancellation tools using
the task and attempt IDs from its delivered prompt. Do not manually repair a
failed model tool call; record it. Observe the Agent's task API and native
tool-call arguments.

Save a redacted JSON object with a `tasks` array in delivery order:

```json
{
  "tasks": [
    {
      "taskId": "task_11111111111111111111111111111111",
      "attemptId": "attempt_11111111111111111111111111111111",
      "marker": "UNIQUE-RESPONSE-1",
      "receipt": {"taskId": "task_11111111111111111111111111111111", "attemptId": "attempt_11111111111111111111111111111111"},
      "completion": {"taskId": "task_11111111111111111111111111111111", "attemptId": "attempt_11111111111111111111111111111111", "response": "UNIQUE-RESPONSE-1"},
      "state": "completed"
    },
    {
      "taskId": "task_22222222222222222222222222222222",
      "attemptId": "attempt_22222222222222222222222222222222",
      "marker": "UNIQUE-RESPONSE-2",
      "receipt": {"taskId": "task_22222222222222222222222222222222", "attemptId": "attempt_22222222222222222222222222222222"},
      "completion": {"taskId": "task_22222222222222222222222222222222", "attemptId": "attempt_22222222222222222222222222222222", "response": "UNIQUE-RESPONSE-2"},
      "state": "completed"
    },
    {
      "taskId": "task_33333333333333333333333333333333",
      "attemptId": "attempt_33333333333333333333333333333333",
      "marker": "UNIQUE-RESPONSE-3",
      "receipt": {"taskId": "task_33333333333333333333333333333333", "attemptId": "attempt_33333333333333333333333333333333"},
      "cancelAck": {"taskId": "task_33333333333333333333333333333333", "attemptId": "attempt_33333333333333333333333333333333", "acknowledgmentId": "ack_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
      "state": "canceled",
      "ackSource": "agent"
    }
  ]
}
```

Replace the sample values with **observed** values. The receipt and tool-call
IDs must come from the trace, and `state`/`ackSource` from the API. If an
expected call is missing, omit its object rather than inventing a successful
one. Run `python3 scripts/devenv/qualify-openrouter-task-contract.py evidence.json`;
any failed check disqualifies the affected task workflow. Run the evaluator
tests with `python3 -m unittest discover -s scripts/devenv -p
'test_qualify_openrouter_task_contract.py'`.

The checker catches mismatched task/attempt IDs, absent completion receipts,
malformed cancellation IDs, missing agent acknowledgment, and a response that
uses a prior task's marker. It cannot prove semantic quality or upstream
identity. A pass applies only to the recorded model, Codex version, route and
date; requalify after a harness or route change. Do not silently translate a
pass into provider-wide support while upstream selection is unknown.

## Missed receipt behavior

Kyber already records an attempt that may have reached the runtime as terminal
`delivery_unknown` when its receipt lease expires. The task API and terminal
event expose that failure code. Automatically retrying that ambiguous attempt
could execute the same user request twice, so qualification does not introduce
a retry. A caller should show `delivery_unknown` as a delivery uncertainty and
let the user decide whether to submit a new task. This is separate from a
runtime accepting delivery but the model failing to call `complete`; the fixture catches
that with a missing completion and nonterminal task state.
