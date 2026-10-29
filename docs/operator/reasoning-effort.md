# Reasoning effort

Reasoning effort controls the model's native thinking level. It does not set
response verbosity, reasoning-summary detail or a token budget.

In **Create Agent**, choose Reasoning effort alongside authentication. In an
Agent's **Configuration → General → Runtime**, use **Change effort** alongside
the model and harness settings to update it. The shared controls work
on desktop and mobile. **Default (no override)** removes Kyber's setting and
leaves the harness's own configuration and defaults in control. Kyber does not
rewrite the Agent's user settings.

Only native choices supported by the selected model, inference route and exact
harness version are offered. The Kyber runtime image must also contain the launch integration. A matching
first-party source build qualifies creation; existing Agents can retain their
observed integration for the exact immutable image across control-plane updates.
A control-plane-only upgrade does not establish runtime-image support. Other
image combinations remain at Default until verified; updating the runtime images
is part of enabling this feature. For an older pinned image without a known
creation-time match, create with Default first; its current integration report
can qualify effort changes for that Agent. Mutable/custom image tags remain
unverified.

Different harnesses/models can have different
levels; Kyber does not translate or clamp them. Unknown combinations offer
Default only. This is native-client compatibility, not proof of provider
entitlement or a guarantee of reasoning quality. Custom inference routes and
other harnesses remain at Default until their effort behavior is qualified.

A real change restarts an active Agent using its existing disk and normal
session path. A no-op does not restart it. Stopped, Failed or NeedsAuth Agents
keep their existing lifecycle intent; an archive hold defers application. A
pending value is distinguished from the value passed to the current pod. Neither
is a claim about the effort a provider used on every request.

Clear or change effort before selecting an incompatible model or runtime
version. Switching harness clears the old effort alongside its model/version
overrides. A runtime install falling back to a different version cannot silently
apply the old effort. Correct the version or reset effort to Default. Unknown
support and configuration problems are separate from provider authentication.

## API

- `GET /api/v1/effort-options?runtime=codex&model=gpt-5.6-sol` previews creation
  choices using the configured fleet defaults. This is administrator-only.
- `GET /api/v1/agents/{name}/effort-options` reports choices for the Agent's
  desired model/version/route.
- `POST /api/v1/agents/{name}/set-effort` accepts `{"effort":"high"}` or
  `{"effort":""}` to reset. Omitted/null values and unknown fields are rejected.
- Create requests accept an optional `effort` field. Agent reads include
  `effort`, `effortPending` and an actionable `effortProblem` when present.

Per-Agent lookup and mutation require exact-Agent lifecycle-write access;
mutations require durable audit admission before changes. A conflicting write
returns 409 and requires a fresh review. Metadata failure never prevents resetting
an existing override. Configuration APIs do not reveal provider credentials.

The compatibility policy is deliberately bounded in
[`pkg/reasoningeffort`](../../pkg/reasoningeffort/policy.go). Model catalogs can
carry native effort evidence, but unbound or stale catalog records must not
expand this policy. A new harness version must be qualified before it gains
non-default choices. See [API authorization](../architecture/api-authorization.md)
for operator permissions.
