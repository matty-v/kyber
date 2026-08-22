# Fleet console

The fleet console is the browser-based control surface for a Kyber cluster: a web app (a PWA) served by the cluster itself, where you create agents, watch what they are doing, read their logs and metrics, and act when something needs attention.

![The fleet console dashboard](../../assets/pwa-dashboard.png)

## One console, five surfaces

The console is organized into surfaces, each for one job:

- **Dashboard**: the cluster at a glance. Agent status counts, the recently active agents, which agents are running low on context budget, and a live read-only peek into any agent's terminal.
- **Machines**: create and manage the machines agents run on.
- **Agents**: create and manage agents, stream logs, open a shell into an agent, and drive lifecycle actions such as stop, start, suspend, restart, re-authorize, and delete. An agent's detail view carries its own tabs: Overview, Comms, Secrets, Jobs, Webhooks, Activity, and Shell.
- **Metrics**: per-agent working time, token usage, cost, and live node resource gauges. See [Metrics](#metrics) below.
- **Settings**: manage the API key, choose the update channel and install cluster updates, see the full version breakdown (build, chart version, and the substrate the cluster runs on), and set fleet-wide harness defaults.

Every screen shows a cluster identifier with the cluster's name and version, so you can confirm which cluster and which build you are about to act on before doing anything. The version shown is polled live: it reflects what the cluster is running now, not what the tab loaded with. If the cluster's API is unreachable, the identifier reads "version unavailable". When a newer Kyber release is available, an update indicator appears next to the version and opens the update details, and clusters configured for it can install the update from right there.

## Agent status and activity

Each agent in the Agents list shows a status dot plus a short activity signal: "Working" while the agent is mid-turn, or "Idle" with a relative time once it is waiting for input. This is the same signal the agent detail view shows, so you can tell at a glance which agents have been idle longest without opening each one. An agent that has not reported an activity state yet shows nothing rather than a fabricated state.

The agent detail view's Activity tab is a structured record of what the agent has actually done: its conversation, its tool calls, and the work it delegated to subagents, shown in place as collapsible blocks at the point it happened. The recent conversation is pinned at the top; the full per-session history sits below it. The tab opens on the last 24 hours and widens on request to 3 and then 7 days, and Export downloads everything loaded as a plain text file. A window holding more than can be returned at once is shown truncated, with a banner saying so. The tab reads from the cluster's durable log archive, so it needs the archive bucket configured; on an install without one it shows an error instead of history.

## Agent logs

Logs come from two sources, chosen with a Live / Archive toggle:

- **Live** tails the current pod's output as it happens. It covers only the current pod lifetime: a restart starts a fresh buffer.
- **Archive** is a durable, off-cluster copy. Pick a time window and read what the agent logged in that window, even across restarts, back to a retention window that defaults to 30 days. The retention window is an operator setting.

Very large reads come back truncated rather than unbounded, and the API says so explicitly, so one heavy log read cannot destabilize the control plane. A third source, Transcript, is available at the API level: the agent's real session record, every message and tool call, durably archived. Live and Archive carry the pod's own output; Transcript is the surface for auditing what an agent actually did. Archive and Transcript both depend on the cluster's log archive bucket being configured; without it those reads return an explicit unavailable error while Live keeps working.

## Secrets, model changes, and webhooks

The Secrets tab manages an agent's own secrets. Adding a brand-new key never interrupts a running agent; the value becomes available at the agent's next natural pod start. Replacing an existing value, moving a key between the text and file kinds, or deleting a key restarts the agent's pod automatically so a stale value never lingers.

Changing an agent's model from its detail page restarts a live agent's pod so the new model takes effect right away. The restart takes around half a minute, and a message that arrives during that window is held and delivered once the agent is back up. A stopped or suspended agent keeps the new model for its next start without being woken, and a failed agent is started fresh on it. The model list comes from the catalog the agent's own authenticated runtime reports, so it shows what your subscription actually offers; a newly created agent's list fills in once its runtime has reported.

The Webhooks tab manages the signed inbound bindings that let other senders reach the agent. The signing secret is shown exactly once when a binding is created and once again when it is rotated, and is never readable afterward, so capture it then. Rotating keeps the old secret valid for 24 hours so an in-flight cutover does not drop traffic. Each binding is rate-limited to 10 requests per minute by default, duplicate request bodies are dropped for 24 hours, and each agent queues at most 5 pending messages; excess load is shed with an explicit reason rather than buffered without bound.

## Metrics

The Metrics surface is a read-only observability dashboard for the current cluster, with a time range picker from the last 15 minutes up to 7 days. Its panels:

- **Fleet summary**: how many agents are working, idle, and offline right now.
- **Agent activity breakdown** and **working time trend**: how each agent's time split between working and idle over the selected range, and cumulative working hours per agent, useful for attributing spend.
- **Token usage and cost**: input, cache-creation, and cache-read tokens per agent and model, with an estimated cost.
- **Last active**: each agent's time since its last heartbeat, with stale agents visually flagged (the staleness threshold defaults to 5 minutes).
- **Node resources**: live CPU, memory, and disk gauges per node, refreshed every 30 seconds.
- **State change frequency**: how often each agent transitioned between states, for spotting reliability regressions.

The whole surface follows one rule: never show a confident number Kyber cannot stand behind. Cost is computed from an open, community-maintained pricing dataset that is refreshed on a schedule and reviewed before it ships; a model with no known price shows "unpriced" rather than a believable $0.00. The usage numbers are still real in that case, only the cost is unavailable. Likewise, each model's context-window budget is measured against its real context window, auto-detected from the model provider, so a brand-new model gets a correct budget with no manual step; a window that cannot be confirmed is marked as an estimate rather than shown as a confidently wrong number.

## Learn more

- [Agents and persistence](agents-and-persistence.md): the lifecycle actions the console drives, and what each agent state means.
- [Multi-cluster and the API](multi-cluster-and-api.md): running several clusters from one place, and the API behind the console.
- [Metrics tab reference](../../metrics-tab.md): every panel in detail.
- [How the metrics figures are sourced](../../architecture/metrics-data-flow.md)
