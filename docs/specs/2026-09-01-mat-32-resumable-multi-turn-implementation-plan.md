# MAT-32 resumable multi-turn tasks — implementation plan

Status: implemented on `feat/mat-32-resumable-multi-turn-tasks`

References: [MAT-32](https://linear.app/matty-v/issue/MAT-32/a2a-49-implement-resumable-multi-turn-tasks), [accepted design](../design/2026-08-30-resumable-multi-turn-tasks.md), [durable task architecture](../architecture/durable-tasks.md).

## Checkpoints

1. Extend the protocol-neutral task model with `input_required`, `auth_required`, and `rejected`, plus typed interactions and immutable task-visible messages.
2. Persist interactions, messages, response idempotency, expiry, and continuation dispatch atomically in PostgreSQL. Enforce one live interaction with a partial unique index.
3. Expose `request_input`, `request_authorization`, and `reject` through the runtime MCP boundary. Require the exact current task attempt.
4. Expose owner-only typed response and opaque authorization-reference routes. A response creates a fresh dispatch attempt while retaining bounded task-visible context.
5. Cover type validation, ownership, replay/conflict behavior, stale attempts, restart recovery, cancellation, expiry, and continuation envelopes.
6. Deploy to kyber-dev, exercise both supported harnesses, then open and merge the PR after review and CI.

## Security boundaries

- Authorization completion accepts only an opaque registered-flow reference, never credentials.
- Continuations contain the original prompt plus bounded task-visible interaction context; transcripts, hidden reasoning, raw tool output, environment state, and credentials are not stored or replayed.
- Caller responses are owner-only and idempotency keys are scoped to caller, task, and interaction.
