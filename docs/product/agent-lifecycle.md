# Agent Lifecycle — product behavior

> **Verification status:** the phase names and their operator meaning are
> grounded in the shipped agent phase set (the same vocabulary the product
> surfaces and the architecture deep-dive uses). The exact wording and placement
> of each state in the PWA is **_Unverified_ pending a spot-check against a
> running instance** (the one-command dev/test environment, `scripts/devenv/`,
> kyber#399); states are described by what they mean, not by their pixel
> rendering. Maintained by Yoda; see [README](README.md).

## Concept

An **agent** is a long-lived worker. Over its life it moves through a set of
named **phases** that tell an operator, at a glance, whether the agent is
working, paused, needs attention, or is gone. Most transitions are automatic —
Kyber drives the agent toward the operator's declared intent (run it, stop it,
suspend it) and recovers it across machine interruptions. A few states are
**human-required**: Kyber deliberately stops and waits for the operator rather
than retrying into a loop.

The authoritative state list and the exact transitions between phases are owned
by the architecture (HOW) set — see
[Agent / session lifecycle](../architecture/overview.md#5-agent--session-lifecycle).
This page describes the *same* phases purely as **observable states**.

An agent may also own a private **identity repo** — its own GitHub repository of
persona, memory, and session state. When it does, Kyber keeps that repo in sync
across restarts (below) using a repo-scoped credential minted by the platform's
**Kyber Platform GitHub App**; the operator has nothing to manage per-agent once
the App is configured for the install. See
[`../agents-identity-repos.md`](../agents-identity-repos.md) for what lives in the
repo and how it is provisioned.

## Observable behavior

When an operator creates an agent, it provisions and comes up on its own
(`Creating` → `Starting` → `Running`) without further action. From `Running`, the
operator's declared intent moves it: stopping it parks it in `Stopped` with its
filesystem preserved; suspending it scales it to zero; restarting it replaces the
underlying pod while preserving the agent's work.

Kyber also moves an agent **on its own** to keep it healthy: if the cheaper
interruptible machine under an agent is reclaimed, the agent is gracefully
drained and parked until a replacement machine is ready, then brought back —
the operator sees the state change but doesn't have to act. Suspension is the
single resting state that covers both "nothing to do right now" and "the machine
went away"; either way, new work (or a ready machine) wakes the agent back to
`Running`.

When the agent **runtime image** changes for a whole environment at once (e.g. a
fleet-wide digest bump), Kyber rolls the affected agents onto the new image in
**bounded, canary-gated waves** rather than all at once: one agent rolls first as
a canary, and the rest stay `Running` on the old image until that canary comes up
healthy on the new image. If the new image is bad or unpullable, the blast radius
is contained — **only the canary** lands in `ImagePullBackOff`; the remaining
agents keep running on the old image (and Kyber records a `RuntimeImageRollHeld`
event explaining the hold) instead of the whole fleet going down together. A
single-agent environment is unaffected by the pacing: the lone agent is its own
canary and rolls immediately, exactly as before. The wave size and the canary
wait are controller defaults that preserve this behavior; there is no operator
knob to set for the default experience.

Kyber also **self-heals an agent whose monitoring or transcript sidecars die.**
Each agent pod runs small helper containers alongside the agent — the one that
reports the heartbeat the dashboard reads, plus the transcript shippers. If one
of those helpers exits for any reason, Kyber now restarts it automatically and
the heartbeat resumes on its own. Previously a dead helper could leave an agent
**running but invisible** — its work continuing while the dashboard showed a
frozen heartbeat — until an operator manually deleted the pod to recover it. That
manual pod-delete is no longer needed; recovery is automatic, with no operator
action. (The agent container's own recovery is unchanged — if the agent itself
exits, Kyber recreates the pod as before.)

Two situations are **deliberately human-required** — Kyber stops and waits rather
than retrying, because a silent retry would just hide the real problem: an agent
whose stored authorization has expired (`NeedsAuth`), and an agent killed for
running out of memory (`MemoryExhausted`).

## States

| Phase | What it means to an operator | What they should do |
|---|---|---|
| `Creating` | The agent is being provisioned (storage, identity, pod). | Nothing — wait; it proceeds on its own. |
| `Starting` | The agent's pod exists but isn't ready yet. | Nothing — wait. |
| `Running` | The agent is up and its readiness check passes — the normal working state. | Nothing — it's healthy. |
| `Stopping` | The agent is being gracefully shut down (operator-initiated). | Nothing — wait for `Stopped`. |
| `Stopped` | The agent isn't running; its filesystem is preserved. `Stopped` is an **authoritative kill switch**: once an operator stops an agent it stays down — Kyber will not recreate its pod, even if the agent was crash-looping — until the operator starts it again. | Start it again when you want it back. |
| `Restarting` | The agent's pod is being replaced; its work is captured and a new pod is starting. This is usually operator-initiated (see Operator actions), but Kyber may also enter it **on its own** — without anyone touching the agent — when the agent's runtime image is updated, so it rolls onto the new image (work preserved, returns to `Running`). On the way back up, the agent's identity repo is re-synced to its **default branch**, so changes the team merged while the agent was busy land on the restarted agent; the agent's own branches, unpushed commits, and uncommitted edits are preserved. | Nothing — wait for `Running`. |
| `Suspended` | The agent is scaled to zero (no pod), filesystem preserved — either idle or parked due to machine interruption. | Nothing — new work or a ready machine wakes it. |
| `Draining` | The agent is being gracefully drained ahead of its machine being reclaimed. | Nothing — Kyber is protecting its work. |
| `WaitingForMachine` | The agent is waiting for a replacement machine after its machine was reclaimed. | Nothing — it resumes when capacity returns. |
| `NeedsAuth` | The agent's stored authorization is no longer valid; it can't work until re-authorized. Kyber reaches this automatically when an agent's authorization expires, and an operator can also **force** an agent here to recover it from a wedged state (see Operator actions). | **Re-authorize it** (see Operator actions). |
| `MemoryExhausted` | The agent was killed for exceeding its memory limit; Kyber will **not** auto-restart it. | **Give it more memory, then restart it** (see Operator actions). |
| `Failed` | The agent hit an unrecoverable error or used up its automatic restart attempts. | Investigate, then restart it. |
| `Deleted` | The agent has been fully removed, including its storage and identity. | Nothing — it's gone; recreate if needed. |

> Beyond the phase, an operator may also see **flags** on an otherwise-`Running`
> agent — for example, that its configured model couldn't be resolved, that a
> requested runtime version didn't take, or that it should be restarted to pick
> up a platform update. Each flag names the problem and the remedy.
> _Unverified — the exact set and wording of these flags is pending a spot-check
> against a running instance (#399)._

## Operator actions

| Action | Where | Expected result |
|---|---|---|
| Create an agent | PWA → create-agent flow | Agent provisions and comes up: `Creating` → `Starting` → `Running`. |
| Stop an agent | PWA (Agents) | Agent moves to `Stopping` → `Stopped`; filesystem preserved. Stop is **authoritative** — it halts the agent from any incident phase (`Running`, `Starting`, `Failed`, `MemoryExhausted`, `Suspended`) and pre-empts auto-restart, so it reliably stops a crash-looping agent. The agent stays `Stopped` until started again. |
| Start a stopped agent | PWA (Agents) | Agent returns `Stopped` → `Running`. |
| Suspend an agent | PWA (Agents) | Agent scales to zero (`Suspended`); wakes on new work. |
| Restart an agent | PWA (Agents) | Agent's pod is replaced (`Restarting` → `Running`), work preserved. The restart re-syncs the agent's identity repo to its **default branch**, picking up changes merged by the team — local branches, unpushed commits, and uncommitted edits are preserved. Kyber also performs this same roll **automatically** when an agent's runtime image is updated — no operator action needed; the agent picks up the new image with its work preserved. |
| Re-authorize an agent | PWA → Re-authorize action | Clears `NeedsAuth`; agent returns to service once authorization completes. |
| Require re-auth (force a wedged agent to re-authorize) | PWA → Agents → Lifecycle menu → **Require re-auth** (confirmation required) | Drops a wedged agent to `NeedsAuth`, tearing down its running pod; re-authorize to bring it back. Available from `Running`, `Starting`, `Failed`, `MemoryExhausted`, `Stopped`, and `Suspended`. The clean recovery path when an agent is stuck on stale state. |
| Recover a memory-exhausted agent | Increase the agent's memory, then restart it | Agent leaves `MemoryExhausted` and returns to `Starting`. |
| Delete an agent | PWA (Agents) | Agent and its storage/identity are removed (`Deleted`). Delete is **guarded against accidents**: it requires an explicit confirmation matching the agent's name before anything is removed, so a stray click or script can't destroy an identity. A completed delete also clears the agent's leftover usage/accounting state, so nothing is orphaned behind it. **A confirmed delete is irreversible** — there are no off-cluster backups yet, so a deleted agent's identity and storage cannot be recovered. |

### Who may drive these transitions

Driving a lifecycle transition through the API requires an authorized caller, not
merely a valid API key. The fail-safe verbs (start / stop / restart) need a
`lifecycle:write`-scoped caller; the more impactful **Suspend** and **Require
re-auth** need the higher `lifecycle:admin` scope, so a caller cleared for routine
start/stop cannot suspend or wedge an agent. **Delete** — the single most
destructive action — needs that same `lifecycle:admin` scope *and* the typed
confirmation above, so it takes both the highest authority and a deliberate
acknowledgement to remove an agent. Operators using the legacy
full-scope key — and the PWA — can drive every transition as before; the gate
only narrows what a deliberately *scoped* key may do, and is opt-in per cluster.
See [`../api-keys.md`](../api-keys.md) for issuing scoped keys.

## See also

- **How** the lifecycle works (events, transitions, the state machine):
  [`../architecture/overview.md` § Agent / session lifecycle](../architecture/overview.md#5-agent--session-lifecycle)
- The surfaces these actions happen on: [`pwa-holocron.md`](pwa-holocron.md)
