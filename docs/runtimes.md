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

### Goals on Hermes

A `pre_llm_call` hook opens a goal revision at the start of each user turn, so
the agents list shows `Working on a new request` rather than nothing, and the
agent refines it by calling `get_goal` then `set_goal`.

The nudge works the same way it does on Claude Code: a `pre_llm_call` hook that
prints `{"context": "..."}` has that string injected into the model's context,
so the agent is told the revision rather than having to discover it.

The one real difference: **`pre_llm_call` fires on every model call, not once
per user turn.** The hook deduplicates on `turn_id` (keyed with `session_id`,
since a restarted session reuses turn ids) under a lock. Without that it would
re-open the revision mid-turn — resetting the goal to the placeholder and
invalidating the agent's revision, so its next `set_goal` gets a 409 — and
re-inject the nudge on every call.

Refinement is best-effort, exactly as on Claude Code: an agent that ignores the
instruction leaves the platform placeholder in place.

Hermes does not advertise durable tasks, job turn hooks, subscription login,
or in-place runtime repair. Its pinned pre-model hook cannot enforce Kyber's
fail-closed task receipt boundary, so durable task dispatch remains disabled
for this runtime.

## A model endpoint Kyber does not host

`spec.inference` points an agent at an inference endpoint you run. **Hermes and
Codex** read it; a runtime that does not declare the
`custom-inference-endpoint` feature rejects the field rather than storing a
setting it would ignore.

In the create-agent wizard this is four fields on the Auth step: tick **Use a
custom inference endpoint**, then the URL, the model, and the endpoint's API
key. Kyber stores the key in a managed `<agent>-inference` Secret, exactly as
it does an OpenRouter or Anthropic key — there is no Secret to create by hand.

Through the API, supply the key as a value:

```json
{
  "inference": {
    "baseURL": "https://llm.example.com/v1",
    "api": "openai",
    "model": "qwen3.6-35b-a3b",
    "apiKey": "<the endpoint's key>"
  }
}
```

An operator who would rather own the Secret can pass `credential` instead of
`apiKey` — exactly one of the two. What lands on the Agent resource is always
a reference, never the value:

```yaml
spec:
  inference:
    baseURL: https://llm.example.com/v1
    api: openai
    model: qwen3.6-35b-a3b
    credential:
      existingSecret: <agent>-inference
      key: token
```

`api` names the **wire protocol, not a vendor**, so any server speaking it
qualifies — llama.cpp's `llama-server`, vLLM, SGLang, Ollama, or a hosted
provider Kyber has never heard of. `openai` is the only protocol implemented
today.

### What an agent needs no longer includes its harness's own provider key

A Codex agent on an endpoint needs no OpenAI key, no ChatGPT login, and never
contacts OpenAI; a Hermes agent on one needs no OpenRouter key. Kyber neither
asks for that credential at creation nor mints a Secret for it.

If such an agent lands in `NeedsAuth`, the fix is to **rotate the endpoint's
Secret** — that is the credential the recovery gate watches. The re-authorize
control still offers the harness's own login flow (ChatGPT for Codex), which is
the wrong lever for these agents and will not clear the phase.

### Codex against a custom endpoint

Codex resolves a model through a named provider, so Kyber renders one into
`/etc/codex/managed_config.toml` at boot:

```toml
model_provider = "kyber-endpoint"
model = "qwen3.6-35b-a3b"

[model_providers.kyber-endpoint]
name = "Kyber inference endpoint"
base_url = "https://llm.example.com/v1"
wire_api = "responses"
env_key = "OPENAI_API_KEY"
```

`wire_api = "responses"` because Codex speaks **only** the Responses API:
`wire_api = "chat"` was removed upstream in February 2026 and now hard-errors.

**An endpoint serving only `/v1/chat/completions` cannot be used with Codex**,
and plenty do — including some of the servers listed above as OpenAI-compatible.
`api: "openai"` does not distinguish the two, so the runtime probes
`POST /responses` once at boot and exits with a clear reason on a 404 or 405
rather than reporting healthy and failing every turn. llama.cpp's
`llama-server` serves it; check before pointing Codex at anything else. `env_key` names the variable Codex reads the bearer token from; despite
the conventional name it is **not** an OpenAI credential, and such an agent
needs no OpenAI key, no ChatGPT login, and never contacts OpenAI.

Ordering in that file is load-bearing: `model_provider` and `model` are
top-level keys and must precede `[model_providers.*]`. A top-level key written
after any table header is parsed as a member of that table, and Codex would
silently never see the provider selection.

### Rules that apply to every runtime

The following hold for Hermes and Codex alike.

Such an agent needs no OpenRouter key: it authenticates to its own endpoint and
never contacts the harness's built-in provider, so Kyber neither asks for that
credential at creation nor mints an `<agent>-openrouter` Secret for it.

When you pass `credential` instead, that Secret must already exist in the
agent's namespace; Kyber does not create it. Either way the value is injected into the agent's pod as an environment variable and
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
