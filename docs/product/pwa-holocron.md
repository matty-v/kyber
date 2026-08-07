# Operator Surfaces: the PWA & Holocron — product behavior

> **Verification status:** grounded in the shipped operator-facing PWA overview
> (`docs/pwa/overview.md`) and the product's described surfaces. The exact
> current layout and labels should be spot-checked against a running instance
> (the one-command dev/test environment, `scripts/devenv/`, kyber#399); anything
> not confirmable there is marked _Unverified_. Maintained by Yoda; see
> [README](README.md).

## Concept

Kyber gives an operator a **browser-based control surface** for a cluster — the
**PWA, the Fleet Command Console** — where they see and control the agents and
machines in that cluster. For operators running more than one cluster,
**Holocron** is a multi-cluster hub that presents one view per cluster from a
single place. The PWA is the per-cluster cockpit; Holocron is the fleet-of-fleets
hub.

## Observable behavior

The PWA is served by the cluster itself and is reached in a browser. Access is
gated by an API key the operator sets once on the Settings surface; until it's
set, the console can't talk to the cluster.

Every screen shows a **cluster identifier** — the cluster's name and version
(for example, `kyber-falcon v1.2.0`) — so an operator can confirm *which* cluster
and *which* build they're about to act on before doing anything. If that
identifier reads `version unavailable`, the cluster's API is unreachable. If a
newer version has been deployed since the operator opened the tab, a refresh
indicator appears next to the version; clicking it reloads onto the new build.

Holocron mounts the same console once per cluster, so an operator managing
several Kyber installs navigates between them without juggling URLs or separate
logins per surface.

## States

The orientation signal an operator relies on most:

| What they see | What it means | What they should do |
|---|---|---|
| `kyber-<name> v<version>` | Connected to that named cluster on that build. | Confirm it's the right cluster/build before acting. |
| `version unavailable` | The cluster's API is unreachable from the console. | Check the cluster/control surface health before relying on what's shown. |
| Refresh indicator next to the version | A newer build was deployed since the tab was opened. | Click it to reload onto the current build. |

## Operator actions

The console is organized into surfaces, each for one job:

| Surface | What an operator does there |
|---|---|
| **Fleet** | See an overview of all machines and agents in the cluster at once. |
| **Machines** | Create, view, and manage the machines agents run on; reach a machine's terminal. |
| **Agents** | Create, view, and manage agents; stream an agent's logs; drive lifecycle actions (stop, start, suspend, restart, re-authorize, require re-auth, delete). |
| **Metrics** | Read per-agent token usage, cost, and rate metrics. Two honesty rules govern what's shown. The token-budget % is measured against each model's **real context window**, auto-detected from the model provider — so a newly-released model gets a correct budget with no manual step — and a model whose window can't be confirmed is marked an _estimate_ rather than shown as a confidently-wrong number over 100%. Likewise, **cost** is computed from a maintained pricing dataset; a model with **no known price** shows `—` ("unpriced") rather than a believable `$0.00`. See [Token usage & cost](#token-usage--cost-where-the-numbers-come-from) below. |
| **Settings** | Set and manage the API key; see the full cluster version breakdown. |

The detailed version breakdown (build SHA, build date, chart version, the
substrate the cluster runs on) lives on the **Settings** surface.

### Token usage & cost: where the numbers come from

The **Metrics** surface reports each agent's token usage and an estimated
**cost**. Two things keep those figures trustworthy as the model stack changes,
both built on the same rule — *never show a confident-looking number Kyber can't
stand behind*:

- **Context window — auto-detected.** The token-budget percentage is measured
  against each model's real context window, which Kyber learns automatically
  from the model provider rather than from a hand-maintained list. A brand-new
  model gets a correct budget without anyone editing anything. If a window
  genuinely can't be confirmed, the budget is shown as an _estimate_ and flagged
  "unverified" — never a misleading reading over 100%.

- **Pricing — sourced from an open dataset, not hand-typed.** Per-token prices
  come from a **public, community-maintained pricing dataset** (the open-source
  **LiteLLM** model-price feed), vendored into the product and refreshed on a
  schedule. Two clarifications worth stating plainly, because the name invites a
  misread:
  - LiteLLM here is **only a pricing dataset**, read when the product is built.
    It is **not** a proxy or gateway, and **no agent traffic is routed through
    it**. Adding it changed how *prices are sourced* — nothing about how agents
    run, or which model providers they reach.
  - Prices are **not hand-maintained**. A scheduled refresh keeps the dataset
    current, and every price change is reviewed before it ships, so a wrong or
    poisoned upstream value is caught rather than shown live.

- **What "unpriced" / `—` means to you.** If the Token Usage panel shows `—` or
  an "unpriced" badge for a model instead of a dollar figure, that model simply
  isn't in the pricing dataset yet (or its price failed a sanity check). The
  usage numbers are still real; only the cost is unavailable, and Kyber says so
  plainly rather than reporting a misleading `$0.00`. This is the same honesty
  rule the budget gauge uses for an unverified window.

- **Adding a model is near-zero-touch for cost reporting.** Because the context
  window (auto-detected) and the price (dataset-sourced, auto-refreshed) both
  arrive on their own, bringing a newly-released model onto the fleet no longer
  needs any hand-editing for it to report correctly: if the model is in the
  dataset its cost appears, and if it isn't yet it shows `—` until the next
  refresh — never a wrong number in the meantime.

### Agent activity on the list

Each entry in the **Agents** list surfaces a short activity signal —
**"Working"** while the agent is mid-turn, or **"Idle &lt;relative-time&gt;"**
(for example, "Idle 1h ago") once it's waiting for input — alongside the
existing working/idle status dot. This is the same activity text the agent
**detail** view shows, so an operator scanning the fleet can tell at a glance
which agents have been idle longest without opening each one. An agent that
hasn't reported an activity state yet shows nothing for this signal (no
placeholder, no fabricated state).

### Agent Activity: the structured history

The agent **detail** view's **Activity** tab is one structured record of what the
agent has actually done — its conversation, its tool calls, and the work it
delegated. It is a single view: there are no sub-tabs and no raw-terminal
toggle, and **Refresh** and **Export** sit in the history header.

**The tab covers the last 7 days.** That window is fixed — everything below is
scoped to it, the header states it alongside the session count ("N sessions ·
last 7 days"), and an agent with nothing in that window reads "No conversation
history in the last 7 days" rather than appearing to have done nothing at all.
A window holding more than can be returned at once is shown **truncated**, with
a banner saying so and the earliest part of the window displayed.

- **The recent conversation is pinned at the top**, expanded, so the latest
  turns are what an operator sees first. The full per-session history sits
  below it with each session collapsed.
- **Delegated (subagent) work appears in place.** When an agent hands part of a
  job to a subagent, that stretch of work is shown as its own collapsible,
  badged block at the point it happened, containing the subagent's own messages
  and tool calls. This work used to be omitted from the view entirely, which is
  why a session driven mostly by delegation could read as nearly empty.
- **Conversation messages carry timestamps.** What was asked and what the agent
  answered each show their time; tool calls, thinking, and subagent blocks do
  not. Each session is labeled with the time range it spans plus an **Active
  today** tag when it reaches into the current day — so a long-running session
  that started yesterday no longer looks missing.
- **Export** downloads everything the tab is showing — the whole 7-day window,
  not only the session on screen — as a plain text file, for archiving or
  reading outside the console.

Activity is the agent's *work* record. For the pod's own output — boot and
wrapper logs — use the logs panel below.

### Agent logs: Live and Archive

The agent **detail** view's logs panel has two sources, chosen with a
**Live / Archive** toggle:

- **Live** (default) — the agent pod's current stdout, tailed straight from the
  node. This is the existing behavior: it follows new output as it happens, but
  only covers the *current* pod lifetime. A deploy, restart, OOM, or node drain
  starts a fresh buffer, so Live can't show you what an agent logged yesterday
  or before its last restart.

- **Archive** — a durable, off-cluster copy of the agent's logs. Pick a
  **from** and **to** time and press **Load** to read every line that agent
  emitted in that absolute window, even across restarts. This is the surface for
  reconstructing what happened during a multi-day piece of work, or for reading
  back the logs of an agent that has since restarted.

The archive is retained for a bounded window (default **30 days**); lines older
than that are auto-expired to keep storage cost bounded, so the Archive view
reaches back that far and no further. The retention window is an operator
setting, not a fixed product guarantee.

Archive and Transcript reads are **memory-bounded**: a single read returns at
most a fixed number of lines, so a very large or very wide window comes back
**truncated** (the earliest part of the window) rather than unbounded. The API
flags a truncated read with an `X-Kyber-Log-Truncated: true` response header; if
you don't see everything you expected, narrow the from/to window and read in
smaller spans. This bound is why one heavy log read can no longer destabilize the
control plane for everyone (kyber#455).

Reads are also **concurrency-bounded** (kyber#463): only a few Archive/Transcript
reads run at once (default 2; operator-tunable via `controlPlane.maxConcurrentReads`).
If more arrive together, the extra ones get an immediate `429 Too Many Requests`
with a `Retry-After` header instead of a slow or dropped stream — just retry after
the indicated delay. This keeps a burst of large reads from starving the control
plane (so `/version`, the agents list, and the health probe stay up throughout).

Both Live and Archive carry the agent pod's **stdout**, which for Claude Code
agents is mostly boot/wrapper output — not the agent's actual work. A third log
source, **Transcript**, fills that gap: requested at the API level with
`?source=transcript` (and the same absolute from/to window as Archive), it
returns the agent's real Claude Code session record — every user message,
assistant turn, tool call, and result — durably archived under its own storage
prefix. This is the surface for auditing or debugging *what an agent actually
did*, not just that it booted. A dedicated PWA control for the Transcript source
is a planned follow-on; today it is read through the agent logs API.

The Transcript lane is **de-duplicated by message id on read** (kyber#454): the
log-tailer re-ships each session from the beginning on restart, so the stored
objects hold each message 2-3x, but the read path returns exactly one record per
message (first occurrence kept, timestamp order preserved). A consumer can
therefore **sum token/cost directly off the transcript lane without client-side
dedup** — the figure is no longer ~2-2.5x inflated by re-ships. (The Archive
lane, `?source=archive`, is unaffected and unchanged.)

> Verification: the Archive source is covered by automated tests (API + PWA).
> Pending end-to-end observation on a running instance per `scripts/devenv/`
> once the log-shipper (kyber#431 PR-A) is deployed; this note will be marked
> verified at that point.

## See also

- The agent states the **Agents** surface shows, and the action for each:
  [`agent-lifecycle.md`](agent-lifecycle.md)
- The product overview: [`overview.md`](overview.md)
- **How** the operator surfaces are built and published:
  [`../architecture/overview.md`](../architecture/overview.md) and the PWA publish
  boundary doc it links to.
- **How** the Metrics figures (token usage, the auto-detected window, and the
  feed-derived, fail-loud pricing described above) are sourced and computed:
  [`../architecture/metrics-data-flow.md`](../architecture/metrics-data-flow.md).
