# Harness conformance — evidence and onboarding

Companion to the [normative v1 contract](agent-harness-contract.md). This is a
migration baseline, not a certification that every requirement is satisfied.

## Evidence rules

- **Source:** current implementation inspected; no execution proof.
- **Fixture:** executable adapter/script/platform tests with controlled inputs;
  does not prove upstream CLI behavior, credentials or real cluster recovery.
- **Live:** exact image digest/tag, installed CLI version, auth mode, environment,
  test case and result recorded. Image default is not the installed CLI version.
- **Gap:** absent implementation or evidence; cannot claim conformance.

Evidence must never include real credential values, prompts from real tasks,
private transcript content, or authorization codes. Auth failure/rotation tests
use disposable fixtures; live account changes require scoped test credentials.

## Conformance matrix

Baseline source: 20b21f987402204b33944af9135db53d4cbf494b. Test results and review
checkpoint are recorded in the [execution plan](../specs/2026-09-06-mat-7-harness-contract-plan.md).
Implementation checkpoints extend this baseline. Live evidence is recorded separately
from fixture execution; a source implementation alone is not certification.

| Requirement | Claude Code | Codex | Hermes 0.21.0 preview | Reusable or existing evidence |
|---|---|---|---|---|
| HC-01 registration/pod assembly | Implemented | Implemented | Live verified | `pkg/runtimes/contracttest`, adapter tests, and exact-image dev evidence |
| HC-02 lifecycle/readiness | Process probe; auth/service/sync exits classified by runtime descriptor | Process/local login probes; device marker rejected | Process + credential probes, live verified | Adapter/startup fixtures and stable two-container dev rollout; Claude taxonomy has fixture/envtest evidence, live staging validation remains pending |
| HC-03 continuity | Platform recall + optional native resume | Platform recall + optional native resume | Native SQLite resume + platform recall, fixture only | Adapter/session-saver checks plus replacement continuity regression; provider storage recovery remains staging-only |
| HC-04 prompt/session commands | Startup/task + corrected fresh restart live verified; compaction delivery fixture | Startup delivery + fresh restart live verified; compaction delivery fixture | Startup, fresh restart, `/compress`, fixture only | Shared transport/compaction matrix; upstream compaction completion remains staging-only |
| HC-05 API-key mode | Fresh native login/task/restart live verified | Fresh native login/task/restart live verified | OpenRouter creation and model roll live verified | Private per-agent Secrets, native onboarding regressions and exact-image evidence below |
| HC-05 subscription mode | PKCE login live verified; ordered local recovery and compare-and-set write-back fixtures | Device login live verified; opaque auth JSON, stale-seed and compare-and-set fixtures | Gap | Shared checks, `*credential_sync_test.go`, API CAS tests, boot/device/seed/restart fixtures; persistent-volume loss before write-back remains outside the guarantee |
| HC-06 job turn hooks | Registered start/stop hooks | Registered start/stop hooks | Gap | Hermes `pre_llm_call` is context-only and fail-open in the pinned version |
| HC-07 task receipts/completion | Session receipt, explicit task tools | Session + optional turn receipt, explicit task tools | Gap | Hermes cannot satisfy the fail-closed pre-model receipt boundary in the pinned version |
| HC-08 cancellation | notify_only | notify_only | notify_only | `pkg/taskdispatch/cancellation.go` and task control tests; exact interruption unsupported |
| HC-09 common discovery/availability | Descriptor + pod-specific observations | Descriptor + pod-specific observations | Descriptor + pod observations, live verified | Availability expiry/negative tests, API pod/runtime mismatch tests, task/session gates, and replacement-pod invalidation |
| HC-10 model/usage reporting | Reporter/catalog paths | Reporter/catalog paths, unknown catalog context windows | OpenRouter catalog + native call-summary reporter, live verified | `pkg/tokenreport`, runtime catalog tests, and dev model/context evidence; no guessed metric promises |
| HC-10 tools/channels | Configured MCP sidecars | Configured MCP sidecars | Descriptor-gated MCP sidecars; Telegram live verified | Adapter/startup and channel-auth tests; Discord and Slack live evidence outstanding |
| HC-10 in-place repair | Adapter metadata | Adapter metadata | Gap | Success/failure/timeout cleanup and durable-file preservation fixtures; real repair remains staging-only |

The small adapter fixture in `pkg/runtimes/contracttest` checks optional-command
absence, credential isolation, and a conforming adapter with no model discovery
or selection. `pkg/api/runtime_extension_test.go` proves that the same
descriptor and auth strategy can pass public discovery, credential creation,
non-model feature availability, and the task dispatch gate without provider
branches while every explicit model surface fails closed.

The bootable process fixture in `test/integration/harness_contract_test.go`
uses its own registered runtime ID, real tmux, production paste/receipt scripts,
authenticated internal HTTP and PostgreSQL. It proves receipt persistence is
separate from completion, then explicitly completes, verifies the transcript,
and flushes on shutdown. It passed in integration CI run
[34064463947](https://github.com/matty-v/kyber/actions/runs/34064463947) against
`aa26d5c`, alongside the OpenAPI and native startup fixtures. Kubernetes state is simulated. This is neither a
live-pod test nor evidence that an upstream CLI obeys its native hook contract.

## Lifecycle evidence checkpoint — MAT-78

[`test/contract/harness_lifecycle_evidence.json`](../../test/contract/harness_lifecycle_evidence.json)
is the reviewable source of truth mapping HC-01 through HC-10 to named CI tests
and explicit staging-only evidence. It covers both production runtimes and is
validated for missing requirements, broken test references, and undocumented
staging claims.

The deterministic additions cover the production compaction declarations plus
unsupported absence; repair success, failure, timeout, cleanup, and preservation
of unrelated durable files; pod/node replacement retaining the same claim and
credential boundary while invalidating old-pod capability evidence; and the
PostgreSQL transition from ambiguous delivery to terminal `delivery_unknown`
without redelivery.

Live native compaction completion, real package repair, provider node/volume
reattachment, and post-recovery runtime behavior remain staging evidence. Follow
[`docs/operator/harness-lifecycle-staging.md`](../operator/harness-lifecycle-staging.md)
for the combined MAT-76 through MAT-79 pre-release matrix and cleanup record.
Volume loss, unavailable provider state, and uncertain in-flight external side
effects must be reported honestly; no fixture or HTTP delivery response closes
those boundaries.

## Dev validation checkpoint — 2026-09-06

Environment: `datawire-dev/us-central1-a/kyber-dev`, namespace `kyber-system`.
Control plane, runtime base, both harness images and status sidecar were built
from the worktree and rolled out with tag
`worktree-20260906223108-a97298f` (Cloud Build
`d94d9cfc-0302-4b7b-8bad-9b46f66296e9`). The subsequent `aa26d5c` amendment
changes test imports/fixture formatting, not deployed production behavior.

A read-only check of the existing Claude agent `echo`, installed CLI `2.1.260`,
observed hook/task availability remain unknown during startup, then become
available after native configuration was installed and a fresh report arrived.
This verifies the live report/sidecar/API path; no behavioral prompts or account
changes were made to that agent. It does not prove task execution or auth refresh.

Codex subscription test: `sol-test-mat7-codex`, CLI `0.153.4`, same image tag.
The canonical `/auth` route progressed from startup to device-login challenge;
operator consent completed successfully. The first native turn answered
`MAT7_READY`, and all declared hook/task features became available.

- Task `task_792bb9ab4b693cf6d255aa3522124dcf` was observed dispatched with
  no completion response, then explicitly completed through the installed task
  tool with `MAT7_CODEX_TASK_OK`. PostgreSQL contains a delivered Codex receipt
  and native session identity.
- The session-restart API returned success. The new native session answered the
  startup prompt again. Task `task_bc62e955394f66f807b57f9c78a4c4da` completed
  with `MAT7_CODEX_RESTART_OK`; the two task receipts have two distinct native
  session identities, confirming fresh-session behavior rather than resume.

- Negative/recovery case: temporarily removing executable permission from the
  receipt hook changed task-receipt availability to `integration_unavailable`
  while the agent stayed Running. Task
  `task_c8d8f827dff17f9e6ecbc04e5ec4d408` remained queued; its dispatch was
  leased but had no receipt. Restoring the hook to mode 0755 restored capability
  availability, then the same task explicitly completed with
  `MAT7_CODEX_RECOVERED`. No manual redelivery was requested.

Cleanup: the disposable Codex agent was deleted after evidence capture.

Claude PKCE and API-key checks are recorded below. Fixture evidence remains
distinct from successful live provider authentication.

## Claude and browser checkpoint — 2026-09-06

Disposable `sol-test-mat7-claude`, PKCE/OAuth mode, installed Claude Code
`2.1.263`, initial worktree image `worktree-20260906223108-a97298f`:
startup answered `MAT7_READY`; task
`task_e8665a6e0d006d3d03939565bc6d24e8` explicitly completed via the native
MCP task tool with `MAT7_CLAUDE_TASK_OK`. PostgreSQL records a delivered
`claude-code` receipt with native session identity.

The initial restart created a new session and repeated the startup prompt,
but the API returned 500 after its timeout. The generated A2A skill-repair
background loop retained the exec streams and session-lock descriptor. Task
`task_68164c0d13d3cfbbe9aaae570d76103d` ended `delivery_unknown`; it was not
blindly redelivered. Commit `5cfe84a` closes the inherited lock descriptor and
redirects the repair loop's standard streams. Its focused regression and the
complete Claude integration suite pass; corrected-image live evidence follows.

Claude's missing-hook case also passed on the initial image: task
`task_dd7530266b42db5324c009647ca3ff02` remained queued with a leased dispatch
and no receipt while the hook was nonexecutable. After mode 0755 and positive
capability evidence returned, the task explicitly completed with
`MAT7_CLAUDE_RECOVERED`.

The corrected Claude image was built by Cloud Build
`1091bcf3-8dec-40ff-8445-6d889ea7cd1b` and deployed as
`worktree-20260906-5cfe84a-restart`, digest
`sha256:36fb011ac2d261d72d166631e81e8bcd02581672bba2b646a53de3dad054141c`.
The control plane/Codex/sidecar remain on the original worktree tag. A transient
GKE network-sandbox failure occurred during the rollout; the normal start/retry
path recovered the disposable agent. This is not a claim of node-recovery
certification.

On the corrected image, with Claude Code still `2.1.263`, baseline task
`task_bfe3ef468dfd781241160fcead338113` explicitly completed with
`MAT7_CLAUDE_FIXED_BASELINE`. The restart API returned success in 1.06 seconds.
Post-restart task `task_9e6606eca0a7f30d6fb01e9772074fde` explicitly completed
with `MAT7_CLAUDE_RESTART_OK`; the two delivered receipts have distinct native
session identities. The disposable Claude agent was deleted after evidence
capture. Its pending PKCE state had already been removed after exchange.

A real Chromium smoke check against the dev PWA traversed creation for both
runtime IDs, verified the descriptor-specific subscription choices and login
instructions, and switched to the correctly labelled masked API-key inputs.
Screenshots were visually inspected. No agent was created and no provider key
was entered during that UI check. This is UI evidence, not live API-key auth.

## Fresh API-key matrix — 2026-09-07

The initial clean API-key pods exposed native onboarding gaps despite receiving
the selected per-agent Secret: Claude waited for key approval, and Codex stayed
at its login menu. `4510172` corrects those integrations and passes both complete
startup suites plus all code CI gates. The pilot pods and PVCs were deleted.
The following evidence comes from recreated agents with no manual TUI input.

Cloud Build `76e59598-723d-435d-8c28-45fc5050295d` produced tag
`worktree-20260906-4510172-key-auth` for both runtimes:

- Claude image digest: `sha256:657e328d3d36804f8aa4156ca765c0eb3ca7c05f735e01ff6139d0ff8670d7f7`.
- Codex image digest: `sha256:99d4c846a92eb59d7eaaca6b70712dda272c3303d17e5689e3fdc29b96374526`.

| Case | Claude Code | Codex |
|---|---|---|
| Agent / installed CLI | `sol-test-mat7-claude-key` / `2.1.263` | `sol-test-mat7-codex-key` / `0.153.4` |
| Fresh native startup | `MAT7_KEY_READY`, API Usage Billing | `MAT7_KEY_READY`, native record matches selected API key; mode 0600 |
| Explicit task completion | `task_53edb9eec62b05f281145f0e9746355f` → `MAT7_CLAUDE_API_OK` | `task_410ad180563b7f2331c8597d475bbe44` → `MAT7_CODEX_API_OK` |
| Restart API | Success in 1.07s | Success in 0.73s |
| New session startup | Repeated `MAT7_KEY_READY` | Repeated `MAT7_KEY_READY` |
| Post-restart completion | `task_662863aea630dd456417dfb40223dede` → `MAT7_CLAUDE_API_RESTART_OK` | `task_1960124622b0c9a5867e3e6665981e94` → `MAT7_CODEX_API_RESTART_OK` |
| Receipt/session check | Two delivered receipts, two distinct native session IDs | Two delivered receipts, two distinct native session IDs |
| Cleanup | Agent and associated test resources deleted | Agent and associated test resources deleted |

Both authentication modes now have live startup/task/fresh-restart evidence for
both runtimes. The earlier subscription checks retain their exact image tags;
the API-key fixes change API-key startup branches, with subscription regression
coverage in the full startup suites. This does not certify untested refresh,
provider-failure classification, forced interruption, native compaction, repair,
or node/storage recovery behavior. Those limits remain in the matrix.

## Run contract checks

Go version comes from `go.mod`; scripts need bash, jq, curl, Python and git.
The new adapter and receipt tests participate in the existing `go test ./...`
CI gate; existing startup suites require the integration tag. No new job or
branch-protection permission is needed.

```sh
go test ./pkg/runtimes/...
go test ./images/agent-base -run TestHarnessContract -count=1
go test ./pkg/tokenreport ./pkg/taskdispatch ./images/agent-base
go test -tags=integration ./images/codex ./images/claude-code ./images/hermes
# Requires PostgreSQL, Redis, tmux, Python, jq and curl:
go test -tags=integration ./test/integration -run TestHarnessContractBootReceiptAndExplicitCompletion
```

`pkg/runtimes/contracttest.CheckAdapter` takes the trusted descriptor plus
provider-owned auth fixtures and never switches on a runtime name. Add each
supported mode, its selected Secret suffix/keys, forbidden alternate-mode
variables, and two Agent names. A descriptor with `model-catalog` must expose a
nonempty `ModelEnvVar` and pass the selected value; a descriptor without it must
return an empty key and inject no model override. The no-model fixture exercises
that baseline beside Claude Code, Codex, and Hermes. Do not turn this test-only
fixture format into a second production capability schema.

Receipt tests execute the production hook against a local HTTP server for both
current runtime IDs and the fixture ID: acceptance, connection loss after POST followed by exact
GET, unavailable service, mismatched identity, ordinary prompts, wrong hook event,
missing session identity, and conflicting local evidence. They check exit status,
request correlation, private file permissions, and absence of prompt persistence.
They do not prove the real harness obeys a hook's exit code or runs a model turn.

Declared/observed gates, unknown/stale reports and the bootable process fixture
now have automated coverage. Both real harnesses/auth modes also have versioned
dev evidence above. The matrix retains unverified behaviors and documented
exceptions rather than inferring complete conformance from those checks.

## Minimal profile without model selection — MAT-79

The tested `extension-fixture` is a third runtime descriptor with ordinary
API-key authentication plus job/task declarations, but no `model-catalog`.
Its adapter returns `""` from `ModelEnvVar` and never injects a model. This is
the complete optional-capability path:

1. Implement the standard `Runtime`, `Adapter`, `Probe`, `DescribedRuntime`,
   and `Authentication` interfaces. Do not copy a production adapter.
2. List only implemented optional features in `Descriptor.Features`. Omit
   `model-catalog` when Kyber cannot authenticate a catalog and select a model.
3. Return an empty `ModelEnvVar`; create Agents without `spec.model`. Kyber
   then rejects explicit create/patch/set-model requests and catalog reads with
   the shared unsupported reason, and the PWA omits the controls.
4. Declare task receipts, task tools, or job hooks independently when they are
   implemented. Their current-pod evidence and delivery gates do not depend on
   model discovery.
5. Supply the auth cases and descriptor to `contracttest.CheckAdapter`, add the
   public creation/negative-operation fixture, and run the commands below.

The fixture adds no `claude-code` or `codex` conditional to shared feature
logic. Production runtime names remain only in compatibility projections such
as historical fleet-default/catalog keys and provider-owned integrations.
This is deterministic fixture evidence, not a live third-harness certification.

## Onboarding checklist

1. Choose the v1 interactive profile and stable runtime ID. Map each HC requirement
   to native, adapter, or platform behavior, auth modes and version limits.
2. Add `pkg/runtimes/<id>/` with Runtime/Adapter/Probe and path constants. Register
   via init and add the binary imports that enable it. Reuse the shared checker.
3. Supply image and shared entrypoint integration, `KYBER_START_CMD`,
   `KYBER_RUNTIME_DEFAULT_VERSION`, installed/requested-version reporting,
   launch/watchdog and termination handling. Configure chart image defaults,
   build/release wiring, image lookup and installed-version discovery.
4. Implement the descriptor and selected-mode credential strategy. Reuse the
   discovery-driven login UI and canonical `/auth` route for supported flows;
   new flow types require an explicit extension. Own native refresh/write-back,
   stale-seed protection and failure classification in the runtime integration.
   If model selection is unsupported, omit `model-catalog`, return an empty
   `ModelEnvVar`, and leave `spec.model` empty; do not invent a dummy model.
5. Integrate persistent home/workspace, startup identity/instructions, platform
   recall and transcript adapters. Distinguish native resume from fresh restart.
6. Register configured platform MCP services in native configuration; provide
   required hook pairs for claimed jobs/task features and explicit completion.
   Keep channel credentials in their platform service boundary.
7. Provide sidecar-compatible bounded health/activity/model/usage observations;
   unknown values remain unknown. Optional repair metadata must use owned paths.
8. Run reusable + feature-specific tests, add negative/missing-dependency cases,
   then run version-pinned dev checks with disposable agents and scoped auth.
9. Update this matrix and the normative version history with evidence. Confirm
   the registered capability declaration, observed native wiring and operator
   behavior agree. Unsupported combinations must be visible.
10. Obtain review, publish through existing repository release processes, and
    rerun version-sensitive checks on CLI/image upgrades. Do not auto-promote
    harness features into public service manifests.

This checklist identifies the current extra work; it does not claim third-party
onboarding is already a four-file exercise. The registered descriptors, credential strategies and provider-owned scripts
make those extension points explicit; release review must preserve their tests.

## PR review corrections — 2026-09-07

The review found and corrected six gaps: public task availability now schedules
runtime-evidence expiry independently of skill reports; advanced jobs reject a
missing/non-executable native probe; receipt probes require the exact
runtime-specific command; undeclared transcript roots suppress the pruner; and
authentication exit codes are scoped to the runtime that actually launched;
and the creation wizard selects a supported mode when discovery restricts the
initial runtime to API-key authentication.
Regression checks include API-server-backed public availability persistence,
job delivery rejection and native-config probes. These are deterministic
checks; the earlier native live matrix remains pinned to its recorded images.

## MAT-76 authentication failure taxonomy — fixture checkpoint

PR #249 adds deterministic Claude startup fixtures for missing and
`invalid_grant` credentials, HTTP 429/5xx, malformed success responses,
transport timeout, and failed credential write-back. The fixtures assert exit
categories `2`, `44`, and `45` and reject credential/raw-response leakage.
Descriptor and controller tests cover code uniqueness, runtime-label scoping,
current-agent-container-only classification, lifecycle transitions, bounded
restart counting, category-specific status, and truthful retry-limit guidance.
A Kubernetes 1.31 envtest exercises confirmed-auth, provider/service, and
credential-sync failures through the real CRD status subresource. This is
fixture evidence only; the combined MAT-7 staging matrix remains the release
gate after MAT-76 through MAT-79 are merged.

## MAT-77 credential synchronization recovery — fixture checkpoint

MAT-77 makes the Kubernetes Secret write-back idempotent and ordered for both
supported subscription harnesses. The private write-back endpoints accept the
hash of the bootstrap credential as a compare-and-set precondition, treat an
already-applied credential as success, and reject a stale writer with `409`
without returning credential data. Compatibility requests from an older image
may omit the precondition during rollout; the new Claude and Codex images always
send it.

Claude startup fixtures prove that a rotated credential is atomically written
and flushed to the persistent native file before a failed write-back exits with
the credential-sync category. A second boot with the same disk and stale Secret
recovers that exact local credential, skips replay of the consumed refresh
token, and advances the seed marker only after write-back succeeds. Separate
fixtures cover changed-Secret reauthorization and the first-upgrade local-first
adoption rule. Codex fixtures cover cold seed, local rotation preservation,
immediate pending write-back, operator reauthorization, first-upgrade adoption,
device login, and corrupt/empty local files.

Both reporter suites prove complete-credential deduplication, fsnotify and poll
fallback, immediate exponential retry independent of the five-minute polling
interval, and terminal handling of a superseded snapshot. API tests cover
compare-and-set success, response-loss replay, stale-writer rejection, input
bounds, and atomic full-field persistence. Fixture values are disposable and
diagnostics/API errors are checked for credential redaction. These are
deterministic fixture guarantees; final session restart and pod-replacement
validation remains part of the combined MAT-7 staging pass before release.
