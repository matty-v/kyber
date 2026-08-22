# Runtimes

Kyber supports two agent runtimes: Claude Code and Codex. Both run inside the standard Kyber agent pod, use whole-disk persistence, receive inbound work through Kyber's signed dispatch path, and expose the same lifecycle actions. You pick the runtime per agent.

## Use the subscription you already have

Claude Code agents sign in with your Claude subscription, and Codex agents with your ChatGPT subscription, instead of paying per token. API keys work too.

For Codex, subscription login is the default. After you create a Codex agent, its pod starts a device login and the agent detail page shows the resulting URL and device code. You complete the login with your ChatGPT account. The refreshed credential is kept alive across pod replacements, so the login stays active for as long as Codex and OpenAI allow. If credentials ever become invalid, the agent enters `NeedsAuth` and a **Start device login** action on the agent detail page runs the same flow again.

Codex also supports an explicit OpenAI API key mode, chosen at creation time.

## Adopting new versions and models

The control plane runs a detection poller that periodically queries the npm registry for newly released Claude Code and Codex versions, and the Anthropic Models API for new Claude models. The console's harness version pickers read from that feed, so you can adopt a new version with no Kyber code change and no rebuild. Model choices work differently: the change-model picker on an agent's page reads the model catalog that agent's own authenticated runtime reports, for both runtimes, so it shows what your subscription actually offers; a freshly created agent's list fills in once its runtime has reported. Detection failures are handled softly: the last known list keeps serving, and agents are never disrupted by a detection outage.

The same detection feed reports each Claude model's real context window, which is what keeps the console's token-budget gauges honest for brand-new models.

Version pinning is deliberate. Codex's startup self-update check is disabled because Kyber centrally manages the pinned harness; a **Set harness version** action upgrades or downgrades explicitly, and the Settings surface sets the fleet-wide defaults new agents inherit. Agents keep their identity, memory, and disk across a runtime change, because those live in [whole-pod persistence](agents-and-persistence.md) and [identity repos](memory-and-identity.md).

## Learn more

- [Agent runtimes](../../runtimes.md): the Codex credential model and runtime behavior in detail.
- [Runtime detection](../../runtime-detection.md): how new versions and models are discovered.
- [Chat channels](chat-channels.md): both runtimes use the same channel bridges.
