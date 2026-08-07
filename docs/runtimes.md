# Agent runtimes

Kyber V1 supports Claude Code and Codex as long-lived agent harnesses. Both run
inside the standard Kyber agent pod, use whole-disk persistence, receive inbound
work through Kyber's signed dispatch path, and expose the same lifecycle actions.

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
stores that key in `<agent>-openai`, injects it as `OPENAI_API_KEY`, and bypasses
subscription login entirely. Auth mode is fixed at creation time; recreate the
agent to switch modes.

Codex V1 models are `gpt-5.6-sol`, `gpt-5.6-terra`, and `gpt-5.6-luna`. Sol is
the default. Kyber runs Codex with non-interactive approvals and its unrestricted
sandbox because the Kubernetes pod is the agent's isolation boundary. Codex's
startup update check is disabled because Kyber centrally manages the pinned
harness: use **Set harness version** in the agent action menu to upgrade or
downgrade explicitly.

## Telegram and Discord

Telegram and Discord may be enabled in the Create Agent wizard for either
runtime. The runtime-neutral `kyber-mcp-telegram` sidecar long-polls the Telegram Bot
API, filters every update through the configured numeric-user allowlist, and
HMAC-forwards accepted messages into Kyber's inbound dispatcher. Replies go
through the sidecar's MCP server or localhost fallback, so the runtime never receives
the bot token. No public inbound tunnel is required.

Claude Code's former in-process Telegram plugin is retired. Discord uses its
gateway-backed `kyber-mcp-discord` sidecar for either runtime. See
[`agents-comms.md`](agents-comms.md#telegram) for Telegram setup and features.

## Runtime implementation

Runtime adapters live under `pkg/runtimes/` and register themselves with the
control-plane runtime registry. Runtime-specific image and boot logic live under
`images/<runtime>/`; shared pod lifecycle, persistence, transcript, inbound, and
status behavior stays in the controller and sidecars.
