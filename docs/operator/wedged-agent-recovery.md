# Recovering a Wedged Agent — "Require re-auth"

When an agent is **wedged** — stuck `Running` but unresponsive, `Failed` past
its retries, `MemoryExhausted`, or otherwise not making progress — the cleanest
recovery is often to drop it all the way back to the re-authorization flow and
let it rebuild from scratch with fresh credentials. The **Require re-auth**
operator action (kyber#395) does exactly that.

---

## What it does

Triggering **Require re-auth** on an agent:

1. Sets the agent's desired phase to `NeedsAuth`.
2. **Deletes the running pod** if the agent has one (`Running` / `Starting`), so
   it stops executing on bad state. For phases with no live pod (`Failed`,
   `MemoryExhausted`, `Stopped`, `Suspended`) it just flips the status — there
   is nothing to tear down.
3. Parks the agent in `NeedsAuth` until an operator re-authorizes it.

It does **not** touch the agent's stored credentials, persistent disk, memory,
or identity. The existing **Re-authorize** flow overwrites the agent's OAuth
credentials with fresh tokens when you recover it — this action just gets the
agent *into* the state where that flow applies. (This is wedged-agent recovery,
not a credential-compromise response; if you need to invalidate stored tokens,
that is a separate operation.)

## Where it's available

From the **agent detail page → Lifecycle menu** (the "More" dropdown), labeled
**Require re-auth**. It is offered only from the phases a wedged agent can be
recovered from:

`Running`, `Starting`, `Failed`, `MemoryExhausted`, `Stopped`, `Suspended`.

It is **not** offered from transient/cleanup phases (`Creating`, `Stopping`,
`Restarting`, `Draining`, `WaitingForMachine`, `Deleted`) or from `NeedsAuth`
itself (the agent is already there — nothing to force).

## It requires confirmation

Because it tears down a running pod, the action does **not** fire immediately —
it opens a confirmation dialog first. Confirm to proceed. On success the page
shows a "Forced {name} into re-authorization" toast and the agent moves to
`NeedsAuth`.

> **Authorization note.** Require re-auth carries the same operator privilege as
> Stop / Suspend / Restart — anyone with the platform API key can invoke it.
> Its blast radius is a denial-of-availability for the targeted agent until a
> human re-authorizes it, the same as Stop/Suspend. It adds no new privilege
> tier.

---

## Recovery runbook: wedged agent → Running

1. **Identify the wedged agent.** Agent detail page shows the phase. Typical
   wedged signatures: stuck `Running` but unresponsive, `Failed` after retries,
   `MemoryExhausted`.
2. **Require re-auth.** Lifecycle menu → **Require re-auth** → confirm. The pod
   (if any) is deleted and the agent moves to `NeedsAuth`.
3. **Re-authorize.** Use the existing **Re-authorize** action on the agent and
   complete the Claude sign-in. This writes fresh credentials and sets the agent
   back to `Running` via a clean pod rebuild (`NeedsAuth → Starting → Running`).
4. **Confirm recovery.** The agent returns to `Running` and resumes work with a
   fresh pod and fresh tokens.

If step 3's re-authorization is itself the thing that was broken, re-running it
from the clean `NeedsAuth` state is the point — the agent is no longer running
on stale state while you do it.

---

## Stopping a crash-looping agent for offline remediation (kyber#468)

**Require re-auth** rebuilds an agent from fresh credentials. Sometimes you don't
want it to come back yet — a crash-looping agent (cycling `Failed` /
`MemoryExhausted` and auto-restarting) needs to be held **down** while you clear
its backlog or inspect its disk. That is what **Stop** is for, and as of
kyber#468 Stop is an **authoritative kill switch**: it halts the agent from any
incident phase and the controller will **not** recreate the pod until you start
it again.

> **Before kyber#468**, Stop was honored only from `Running`. A crash-looping
> agent never sits in `Running` — it cycles through `Failed` / `MemoryExhausted`
> and auto-restarts — so an operator's Stop was silently ignored and the
> controller kept recreating the pod, fighting the operator (the 2026-06-06
> incident). Stop now wins from those phases and pre-empts auto-restart.

### What Stop does now

1. Sets the agent's desired phase to `Stopped`.
2. **Deletes the pod** (routing live/terminal-pod phases — `Running`, `Starting`,
   `Failed`, `MemoryExhausted` — through `Stopping`) or, for the pod-less
   `Suspended` phase, flips status straight to `Stopped`.
3. **Pre-empts auto-restart** and **stays down across resyncs**: the agent is a
   stable `Stopped` fixed point — no pod is recreated on the periodic reconcile —
   until you set it back to `Running`.

Stop preserves the agent's **persistent disk** (session state, transcripts,
offsets) — it deletes only the pod. That is exactly what makes it safe for
offline remediation: stop the agent, inspect or clear the PVC, then start it.

### Runbook: reliably stop a crash-looping agent, then bring it back

1. **Identify the crash loop.** Agent detail page shows the phase flapping
   between `Failed` / `MemoryExhausted` and `Starting`, with `RestartCount`
   climbing.
2. **Stop it.** PWA (Agents) → **Stop** (or `PATCH desiredPhase=Stopped` via the
   API / the `stop` action verb). The pod is deleted.
3. **Confirm it stays down.** `Status.Phase` converges to `Stopped`,
   `RestartCount` stops climbing, and the pod is **not** recreated across at
   least one full resync interval (`kubectl get pod agent-<name> -n kyber-system`
   → `NotFound` and stays `NotFound`). The agent is now safely halted.
4. **Remediate offline.** With the pod gone and the agent held `Stopped`, clear
   the transcript backlog / inspect the PVC / fix whatever caused the loop. The
   disk is intact and nothing is racing you.
5. **Bring it back.** PWA (Agents) → **Start** (`desiredPhase=Running`). The
   agent rebuilds its pod and converges `Stopped → Starting → Running`.

> **Authorization note.** Stop carries the same operator privilege as
> Suspend / Restart / Require re-auth — anyone with the platform API key can
> invoke it. Broadening which phases honor Stop (kyber#468) does **not** widen
> who can call it, and Stop is **fail-safe**: a forged or stale
> `desiredPhase=Stopped` can only *halt* an agent (recoverable by starting it),
> never keep one running or run stale state.

## See also

- The lifecycle states and transitions:
  [`../architecture/agent-lifecycle.md`](../architecture/agent-lifecycle.md)
  (the `DesiredNeedsAuth` and `DesiredStopped` rows and the live-pod-vs-pod-less
  Action split — the two kill switches share one shape).
- The operator-facing lifecycle and actions:
  [`../product/capabilities/agents-and-persistence.md`](../product/capabilities/agents-and-persistence.md).
