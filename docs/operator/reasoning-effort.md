# Reasoning effort

Reasoning effort controls the model's native thinking level. It does not set
response verbosity, reasoning-summary detail or a token budget.

In **Create Agent**, choose Reasoning effort alongside authentication. In an
Agent's **Configuration → General → Runtime**, use **Change effort** alongside
the model and harness settings to update it. The shared controls work
on desktop and mobile. **Default (no override)** removes Kyber's setting and
leaves the harness's own configuration and defaults in control. Kyber does not
rewrite the Agent's user settings.

For an existing Codex Agent, choices come from its authenticated native model
catalog, bound to that Agent and current pod and the installed executable version.
Evidence expires after two hours and is not shared across agents. `latest` is a
version selection, not an executable version: Kyber shows the actual version in
its effort-options response and checks the selected level against that model.
Explicitly empty native choices offer Default only. Missing, expired, replaced-pod
or mismatched-version evidence cannot authorize a new override. Custom inference
routes remain at Default.

Update both the control plane and Codex runtime images for this behavior. The
runtime must match the current first-party source build, including the standalone
release's source-version tag. A control-plane-only upgrade does not add the native
boot check to an older image. After the updated pod reports its model catalog,
refresh Configuration → General → Runtime. No manual version pin is required.
The launcher resolves `latest`, refuses a failed install/fallback, and rechecks
native model/effort support before every boot. It fails explicitly rather than
silently dropping or clamping an unsupported setting. User-owned configuration
remains intact; Default is always available for recovery.

Claude Code retains its exact version/model compatibility policy. Creation before
an Agent has authenticated model evidence uses the bounded legacy policy; create
with Default and then select effort after the current runtime reports its models.
This is native-client compatibility, not proof of provider entitlement or a
guarantee of reasoning quality.

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
expand this policy. Codex native choices must be reported by the current pod and checked again by
the launcher; other adapters require exact qualification. See [API authorization](../architecture/api-authorization.md)
for operator permissions.
