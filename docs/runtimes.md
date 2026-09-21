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

## Hermes preview

Hermes 0.21.0 is available when the installation pins `image.hermes.tag`.
Creation requires an OpenRouter API key, stored in `<agent>-openrouter` and
injected only into that agent. The requested model is passed to Hermes as its
OpenRouter model identifier. An agent carrying `spec.inference` uses that
endpoint instead and needs no OpenRouter key — see *A model endpoint Kyber does
not host* below.

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

## A model endpoint Kyber does not host

`spec.inference` points an agent at an inference endpoint you run. Hermes reads
it; a runtime that does not declare the `custom-inference-endpoint` feature
rejects the field rather than storing a setting it would ignore.

```yaml
spec:
  inference:
    baseURL: https://llm.example.com/v1
    api: openai
    model: qwen3.6-35b-a3b
    credential:
      existingSecret: falcon-llm
      key: token
```

`api` names the **wire protocol, not a vendor**, so any server speaking it
qualifies — llama.cpp's `llama-server`, vLLM, SGLang, Ollama, or a hosted
provider Kyber has never heard of. `openai` is the only protocol implemented
today.

Such an agent needs no OpenRouter key: it authenticates to its own endpoint and
never contacts the harness's built-in provider, so Kyber neither asks for that
credential at creation nor mints an `<agent>-openrouter` Secret for it.

The Secret must already exist in the agent's namespace; Kyber does not create
it. Its value is injected into the agent's pod as an environment variable and
never appears in the Agent resource, an API response, or a log line — the API
returns only the Secret name and key, so an operator can find what to rotate.

`baseURL` must be HTTPS unless the host is unambiguously cluster-internal (a
bare Service name, `*.svc`, `*.svc.cluster.local`, or localhost), because a
plaintext hop off-cluster would put the bearer token on the wire. Credentials
embedded in the URL are rejected; use the Secret.

Omitting `model` falls back to `spec.model`. Changing the model through the
normal model action keeps both in step. Changing or clearing `inference` rolls the agent's pod, because the endpoint,
the provider selection, and the credential reference are all pod environment.
Clearing it returns the agent to its harness's built-in provider and removes
the managed provider entry from its Hermes config — which then requires that
provider's own credential.

The model picker for such an agent is populated from the endpoint's own
`/v1/models`. Context windows come from OpenRouter metadata, which a
self-hosted endpoint does not publish, so those models are listed with the
context window reported as unknown rather than omitted. Usage reporting records
tokens for the endpoint and reports no cost, because a self-hosted endpoint has
no per-token price to apply.

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

Slack uses a Socket Mode sidecar and supports threaded replies, edits,
reactions, file transfer, and Block Kit buttons through `kyber-slack`.
Configure it through Comms; changes converge when the agent is idle. See
[channel configuration](agents-comms.md#slack).

## Runtime implementation

Runtime adapters live under `pkg/runtimes/` and register themselves with the
control-plane runtime registry. Runtime-specific image and boot logic live under
`images/<runtime>/`; shared pod lifecycle, persistence, transcript, inbound, and
status behavior stays in the controller and sidecars.

Integration authors should start with the [Kyber Agent Harness Contract](architecture/agent-harness-contract.md)
and its [conformance/onboarding guide](architecture/agent-harness-conformance.md).
The v1 contract distinguishes current behavior from migration requirements.
