# Agent runtimes

Kyber V1 supports Claude Code, Codex, and Hermes as long-lived agent harnesses.
They run inside the standard Kyber agent pod and use whole-disk persistence.
Each runtime descriptor declares the lifecycle, authentication, channel, and
optional platform features that its pinned integration supports.

## Codex with a ChatGPT subscription

ChatGPT subscription login is the default. After the operator creates a Codex
agent, its pod runs `codex login --device-auth` and the agent-detail page shows
the resulting URL and device code. The operator completes login with their
ChatGPT account; no local `auth.json` is copied through the browser.

Codex writes the resulting `auth.json` into its whole-disk-persistent home. The
Codex credential syncer also pushes each CLI refresh into the per-agent
`<agent>-codex-auth` Secret because ChatGPT refresh tokens are single-use. On a
pod replacement, Kyber preserves the locally refreshed copy and seeds from the
Secret only when the operator has supplied a genuinely newer credential. This
keeps the subscription login active for as long as Codex and OpenAI allow.

When credentials become invalid, the agent enters `NeedsAuth`. **Start device
login** on the agent-detail page launches the same in-pod device flow and resumes
the agent after authorization.

Codex also supports an explicit **OpenAI API key** mode at creation time. Kyber
stores that key in `<agent>-openai`, injects it as `OPENAI_API_KEY`, and prepares
the native login using `codex login --with-api-key` over stdin. Subscription
login is bypassed entirely. Auth mode is fixed at creation time; recreate the
agent to switch modes.

Available models come from the agent’s authenticated runtime catalog. An empty
model setting lets the harness select its native default; the reported current
model is observation, not a persistent pin. Kyber runs Codex with non-interactive approvals and its unrestricted
sandbox because the Kubernetes pod is the agent's host-isolation boundary.
Agent containers are de-privileged by default, run in a Linux user namespace so
in-pod root maps to an unprivileged host uid, retain only the mount capability
needed to assemble their chroot, and do not receive Kubernetes ServiceAccount
tokens. Agent pods also default-deny pod-network ingress and are denied the
infrastructure ranges outbound when NetworkPolicy is enabled. See
[`design/agent-pod-isolation.md`](design/agent-pod-isolation.md). Codex's
startup update check is disabled because Kyber centrally manages the pinned
harness: use **Set harness version** in the agent action menu to upgrade or
downgrade explicitly.

## Hermes preview with OpenRouter

Hermes 0.21.0 is available when the installation pins `image.hermes.tag`.
Creation requires an OpenRouter API key, stored in `<agent>-openrouter` and
injected only into that agent. The requested model is passed to Hermes as its
OpenRouter model identifier.

The preview supports fresh session restart, native resume from Hermes's
persisted SQLite state, `/compress`, an authenticated OpenRouter model catalog,
and native provider/model/context reporting. Kyber can validate and roll a
model change through the shared model action. The harness dialog can browse
stable Hermes GitHub releases, but installing a different source release still
requires a new pinned runtime image.

Hermes does not advertise durable tasks, job turn hooks, subscription login,
or in-place runtime repair. Its pinned pre-model hook cannot enforce Kyber's
fail-closed task receipt boundary, so durable task dispatch remains disabled
for this runtime.

## Telegram, Discord, and Slack

The Create Agent wizard offers only the channels declared by the selected
runtime and authentication mode. The runtime-neutral `kyber-mcp-telegram` sidecar long-polls the Telegram Bot
API, filters every update through the configured numeric-user allowlist, and
HMAC-forwards accepted messages into Kyber's inbound dispatcher. Replies go
through the sidecar's MCP server or localhost fallback, so the runtime never receives
the bot token. No public inbound tunnel is required.

Claude Code's former in-process Telegram plugin is retired. Discord uses its
gateway-backed `kyber-mcp-discord` sidecar for either runtime. See
[`agents-comms.md`](agents-comms.md#telegram) for Telegram setup and features.

Slack uses a Socket Mode sidecar and supports text/thread replies through
`kyber-slack`. Configure it through Comms and restart the pod to apply changes;
see [channel configuration](agents-comms.md#slack).

## Runtime implementation

Runtime adapters live under `pkg/runtimes/` and register themselves with the
control-plane runtime registry. Runtime-specific image and boot logic live under
`images/<runtime>/`; shared pod lifecycle, persistence, transcript, inbound, and
status behavior stays in the controller and sidecars.

Integration authors should start with the [Kyber Agent Harness Contract](architecture/agent-harness-contract.md)
and its [conformance/onboarding guide](architecture/agent-harness-conformance.md).
The v1 contract distinguishes current behavior from migration requirements.
