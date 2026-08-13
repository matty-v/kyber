# Upgrade hardening — confirmation, in-flight behaviour, rollback, notifications

**Status:** design, awaiting one decision from Matt
**Date:** 2026-08-13
**Author:** Dave
**Context:** razer and falcon both left ArgoCD on 2026-08-13 and now self-upgrade
from the PWA. The Install button is currently a single click with no
confirmation, no way back, and no signal beyond a line of text.

---

## TL;DR

Four asks, one real fork.

1. **Confirmation dialog** — straightforward. The `ConfirmDialog` component already
   exists and is already used for the `main`-channel opt-in. Reuse it with
   `dangerous`, and say plainly that every agent restarts and loses its session.
2. **Block the UI vs cancel** — *this is the fork.* Recommendation: **do neither
   as asked.** Cancel is unsafe to build and the UI blocks itself whether we ask
   it to or not. Build an honest in-flight state instead. Reasoning below.
3. **Rollback** — worth building, with one real constraint: Helm never downgrades
   CRDs, so a rollback across a schema change is not symmetric with the upgrade.
4. **In-app notifications** — the WebSocket event bus already exists; upgrades
   just aren't on it. Small, and it's what makes items 2 and 3 usable.

---

## What exists today

| Piece | State |
|---|---|
| `POST /api/v1/updates/apply` | Creates a Job. Guards: `canSelfUpgrade`, version sane, none in flight. |
| Run phases | `pending` / `running` / `succeeded` / `failed`, read off the Job. |
| Rollback on failure | **Already exists** — `Runner.rollback()` returns to the pre-upgrade revision on every failure path. Helm's `--atomic` is deliberately not used, to keep one rollback path rather than two. |
| Cancel | Does not exist. |
| Confirmation | **None.** `onClick={() => apply.mutate(undefined)}` fires immediately. |
| In-flight UI | Install button disables while `lastRun.phase` is pending/running. Nothing else changes. |
| Notifications | None for upgrades. `/api/v1/events` streams CRD informer events only. |
| Job log | Retained 7 days (`upgradeJobTTL`) — the log *is* the upgrade record. |

---

## 1. Confirmation dialog

Reuse `ConfirmDialog` with `dangerous`. The warning has to name the consequence
the operator will actually feel, which is not "the control plane restarts" — it
is **every agent dies and loses its session.**

> **Install v1.0.4?**
>
> This restarts the whole cluster, in this order:
>
> - The control plane restarts. This page will go offline for roughly a minute
>   and come back on the new version.
> - **All 8 agents are then restarted, a few at a time.** Each one loses its
>   current session and any work in progress. They do not resume where they
>   left off.
>
> Agents currently working: **lando, chewie, han** *(live count)*
>
> If this fails, Kyber automatically returns to v1.0.3.
>
> `[Cancel]` `[Install v1.0.4 — restart 8 agents]`

Two details that make it honest rather than decorative:

- **Name the live agent count**, and ideally which are mid-task. An operator who
  can see "3 agents are working right now" makes a different call at 2pm than at
  2am. The data is already there — the Agent CRs carry phase.
- **State the automatic-rollback promise**, because it is real and it is the
  single thing most likely to make the operator comfortable pressing the button.

## 2. The fork: block the UI, or support cancel?

**Recommendation: neither, as literally stated. Build an honest in-flight state.**

Both options are shaped by one fact that isn't obvious from outside the code:
**during an upgrade the control plane is the process being replaced, and the
control plane is what serves the PWA.** The Job runs the *current* control-plane
image precisely because the thing driving the upgrade cannot be the thing being
upgraded.

What follows from that:

**Blocking the UI is mostly redundant.** The UI stops being served the moment
the control-plane pod rolls. The operator gets a hard block whether we build one
or not — it just currently looks like the page hanging, which reads as a bug. A
modal overlay adds nothing after that point, and it is *lost on reload* anyway,
so it can't be the mechanism that carries state across the blackout.

**Cancel is the one I'd push back on.** Three reasons:

1. **The window is nearly empty.** `helm upgrade --wait` is one long call. Before
   it, there is nothing to cancel; during it, cancelling means killing the Job
   mid-apply.
2. **Killing the Job skips the rollback.** `Runner.rollback()` fires on the
   runner's *own* error paths. A SIGKILL from outside doesn't reach them — it
   leaves a half-applied release with no one to clean it up. We'd be adding a
   button whose main effect is to create the exact state the design works hard to
   avoid.
3. **The operator can't see it to press it.** By the time an upgrade is worth
   cancelling, the page serving the Cancel button is already gone.

Cancel is the wrong primitive here. **You cannot reliably stop this operation,
but you can reliably reverse it** — which is item 3, and is why I'd fold the
effort there.

**What to build instead — an in-flight state that survives the blackout:**

- A persistent banner driven by `lastRun`, not by local component state, so it
  is correct after a reload and after the control plane comes back.
- Three honest phases: *Installing…* → *Control plane restarting, reconnecting…*
  (expected; not an error) → *Restarting agents (3 of 8)* → done.
- The reconnect gap **named as expected**, so a dropped connection mid-upgrade
  doesn't read as a failure.
- Disable the mutating controls that would conflict — Install, policy changes,
  agent create/delete — while a run is in flight. That is the useful half of
  "block the UI", and it's a handful of `disabled` props rather than an overlay.

If Matt still wants a stop button, the safe version is **"cancel while pending"**
only — before the Job's Helm call starts, where deleting the Job is clean. That's
cheap and I'd include it. It is not, however, the "stop this upgrade" that the
word cancel implies, and shouldn't be labelled as though it is.

## 3. Rollback

Operator-initiated return to the previous version. Distinct from what exists,
which is automatic rollback *within* a failed run.

**Proposed:** `POST /api/v1/updates/rollback` → same Job mechanism, running
`helm rollback <release> <revision> --wait`, with the same guards
(`canSelfUpgrade`, none in flight) and the same Run/phase reporting. Target the
**immediately-previous Helm revision** rather than an arbitrary version — that
is what `helm history` gives us cleanly, and it keeps the blast radius to one
known-good state.

**The constraint that must be surfaced in the UI:** `ApplyCRDs` server-side-applies
the *target* chart's CRDs on the way up, and `helm rollback` does **not** put the
old ones back. So after a rollback the cluster runs old templates against new
CRDs.

- Additive schema change (the normal case): fine. Old controller ignores fields
  it doesn't know.
- Removed / renamed / newly-required field: not fine, and it fails later and
  somewhere else — the exact failure mode `ApplyCRDs`'s own comment warns about.

We can't judge that in general, but we *can* detect whether the CRDs differ
between the two chart versions and say so:

> Rolling back from v1.0.4 to v1.0.3. **The Agent CRD changed between these
> versions and will not be reverted** — v1.0.3 will run against v1.0.4's schema.
> This is usually safe, but if the rollback is to escape a schema problem it will
> not fix it. Rolling forward with a patch release is the cleaner escape.

Rollback also restarts every agent again, so it carries the same confirmation as
an upgrade.

## 4. In-app notifications

The transport exists — `/api/v1/events` over WebSocket, `EventBus.Publish`. It
currently carries only CRD informer events, so upgrades are invisible to it.

**Proposed:** publish upgrade lifecycle events onto the same bus —
`upgrade.started`, `upgrade.control-plane-restarting`, `upgrade.agents-rolling`,
`upgrade.succeeded`, `upgrade.failed`, `upgrade.rolled-back` — and render them as
toasts. There is no toast component yet; that's the one new UI primitive here,
and it's reusable well beyond upgrades.

Two things worth getting right:

- **The events must survive the reconnect.** The most important notification —
  "upgrade finished" — is emitted by a control plane that just restarted, to a
  client that just reconnected. Toasts alone will drop it. The banner reading
  `lastRun` is what actually carries the outcome; toasts are the nicety on top.
- **Failure needs to be loud and durable**, not a toast that auto-dismisses after
  five seconds while nobody is looking at the tab.

---

## Sequencing

1. Confirmation dialog + in-flight banner + disable-conflicting-controls (items
   1 and 2) — small, and closes the "I clicked once and everything restarted"
   gap immediately.
2. Upgrade events on the bus + toasts (item 4) — makes 1 and 3 legible.
3. Rollback endpoint + UI + the CRD-drift warning (item 3) — biggest piece.

## The one decision

**Item 2.** I'm recommending no cancel button and no blocking overlay — instead
an honest in-flight state, plus optionally a "cancel while still pending" that is
safe but narrow. Options:

- **A.** Take the recommendation: in-flight state, no cancel. *(recommended)*
- **B.** Recommendation + cancel-while-pending only, labelled honestly.
- **C.** Full cancel anyway — I'd want to talk through what it should do to a
  half-applied release first, because "kill the Job" is not it.

Everything else I'll build as specced above unless told otherwise.
