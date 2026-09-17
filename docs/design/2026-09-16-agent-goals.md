# Agent goals

**Status:** Implemented
**Date:** 2026-09-16
**Tracker:** [MAT-62](https://linear.app/matty-v/issue/MAT-62/show-current-one-line-agent-goals-in-the-kyber-ui)

## 1. Problem

Kyber tells an operator whether an agent is working, but not what the work is.
The only reliable answer today is to open the activity history or terminal. That
does not scale to a fleet and is especially awkward on a phone.

The fleet UI needs one short, current goal per agent. It must appear without
depending on an agent remembering an instruction, must not turn an untrusted
runtime into a writer for another agent, and must not leak a raw prompt into a
more visible fleet surface.

## 2. Decision

Kyber owns goal lifecycle. A managed `UserPromptSubmit` hook reports every
accepted prompt to the pod-local status sidecar. The sidecar immediately writes
a safe fallback goal and records the accepted-at timestamp. The registered
`kyber-request-reply` MCP server exposes a self-scoped `set_goal` tool so the
agent can replace that fallback with a useful semantic summary during the turn.

This is deliberately a hybrid:

- the managed hook guarantees that a newly accepted turn cannot leave the old
  goal looking current;
- the fallback is generic (`Working on a new request`) and therefore never
  republishes prompt text or secrets;
- the agent, which already has the prompt in context, supplies the useful
  one-line summary without a second model call or a new provider credential;
- the sidecar and internal API derive agent identity from pod configuration and
  authentication. The runtime cannot write another agent's goal.

The platform does not attempt heuristic prompt truncation. Truncation is not
summarization and would copy sensitive input into fleet-wide list responses.

## 3. Data model

`Agent.status.goal` is the single source of truth:

```yaml
goal:
  summary: "Implement one-line agent goals in Kyber"
  source: agent        # platform | agent
  acceptedAt: 2026-09-16T12:00:00Z
  updatedAt: 2026-09-16T12:00:04Z
```

`summary` is plain text, collapsed to one line, UTF-8 safe, and limited to 120
Unicode code points. `source` makes fallback state explicit. `acceptedAt` is
the ordering key from the latest managed prompt hook; `updatedAt` is the last
successful write.

The initial implementation intentionally does not persist prompt text, session
IDs, turn IDs, or prompt-source metadata. All prompt sources that reach a
supported harness (PWA, Telegram/Discord/Slack, inbound webhook, cron, durable
task, and A2A) converge at `UserPromptSubmit`, so source-specific control-plane
paths do not independently write goals.

## 4. Lifecycle and ordering

1. The harness accepts a prompt and runs Kyber's managed hook before model
   processing.
2. The hook signals the sidecar goal endpoint without forwarding the prompt.
   The trusted sidecar supplies the revision timestamp; runtime input cannot
   forge a future revision.
3. The sidecar writes the generic platform fallback with `source=platform`.
4. The turn instruction tells the agent to call `set_goal` once it understands
   the request. The tool writes `source=agent` and retains the latest
   `acceptedAt` value.
5. A later prompt replaces the prior goal with a new fallback before the later
   turn starts. A late refinement is accepted only when it names the current
   `acceptedAt` revision returned by `get_goal`; stale writes receive a conflict.

The goal is not cleared when activity becomes idle. It remains the most recent
accepted goal, while the existing activity badge communicates whether work is
currently running. Pod/session restarts also retain it because Agent status is
the authority. A newly accepted prompt after restart replaces it normally.

Hook or sidecar failure must never block a user prompt. The prior goal can be
stale in that degraded case, but its `acceptedAt` remains visible and the
existing heartbeat exposes sidecar health. Goal writes are best-effort and
bounded; they do not join the durable-task delivery contract.

## 5. Trust, privacy, and access

- Only the pod-token-authenticated internal endpoint may patch goal status.
- The endpoint applies act-on-self identity exactly like the existing status
  and task paths.
- The hook transmits no prompt content. The fallback contains no user data.
- Agent summaries are treated as untrusted display text: normalized, length
  limited, escaped by React, and never interpreted as Markdown or HTML.
- `GET /api/v1/agents` already requires authenticated operator access. Goal data
  is not added to public capabilities or unauthenticated A2A discovery.

## 6. UI placement

Use the same `Agent.goal` response field everywhere; no component generates its
own summary.

- **Recently active:** show the goal under the agent name, clamped to one line.
- **Dashboard terminal peek:** show the selected agent's goal above the live
  terminal. Changing the selector changes both terminal and goal.
- **Agent detail:** show the goal above the agent terminal on Overview.
- **Agent list:** show the goal below the name on desktop and below the card
  header on mobile.

Platform fallbacks use muted text. Agent-authored goals use secondary text.
The existing activity badge remains authoritative for working versus idle; goal
copy must not duplicate or replace it. An absent goal adds no spacer.

## 7. Compatibility and rollout

All fields are optional. A new PWA works with an old control plane, and a new
control plane works with old pods that never report goals. Unknown goal events
retain the status pipeline's forward-compatible behavior.

This change touches an API response, so the Go response, OpenAPI contract, and
TypeScript type ship together. Adding status fields requires regenerated CRDs
and deep-copy code. Both supported runtimes register the managed hook and the
existing request/reply MCP endpoint gains the bounded tools.

Hermes and future runtimes must implement the same prompt-accepted signal as
part of harness conformance before claiming goal support. Until then, their UI
correctly shows no goal rather than fabricating one.

## 8. Verification

- Hook tests prove prompt content is never transmitted and hook failures do not
  block the turn.
- Internal API tests cover initial fallback, normalization, length bounds,
  stale-revision conflicts, self-only writes, and restart persistence.
- MCP tests cover `get_goal` and `set_goal`, including stable error mapping.
- API/OpenAPI/TypeScript contract tests cover optional goal fields.
- PWA tests cover all selected surfaces, absent/fallback/agent-authored states,
  selector changes, and mobile-safe layout.
- Runtime conformance covers Claude Code and Codex managed-hook registration.

## 9. Non-goals

- a second LLM inference service in the control plane;
- exposing raw prompts or transcript text in list APIs;
- task progress, percent complete, or multi-step plans;
- goal history or analytics in the first version;
- inferring completion from terminal output.
