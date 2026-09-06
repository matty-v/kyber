# Harness conformance — evidence and onboarding

Companion to the [normative v1 draft](agent-harness-contract.md). This is a
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

| Requirement | Claude Code | Codex | Reusable or existing evidence |
|---|---|---|---|
| HC-01 registration/pod assembly | Implemented | Implemented | `pkg/runtimes/contracttest`, existing adapter tests; image boot needs live evidence |
| HC-02 lifecycle/readiness | Process probe; some exit-2 failures conflated | Process/local login probes; device marker rejected | Adapter and startup fixtures, `credential_failure_test.go`; clearer normalized failure taxonomy is a gap |
| HC-03 continuity | Platform recall + optional native resume | Platform recall + optional native resume | Adapter path checks; generated relaunch fixtures, session-saver tests; storage recovery live checks outstanding |
| HC-04 prompt/session commands | Shared tmux delivery, fresh restart, `/compact` | Same platform semantics | `job_dispatch_test.go`, `compact_session_test.go`, startup/relaunch tests |
| HC-05 API-key mode | Anthropic Secret | OpenAI Secret | Shared checks cover mode separation and two Agent names; startup fixtures; live mode checks outstanding |
| HC-05 subscription mode | PKCE OAuth; refresh-token sync | Device login or legacy auth JSON; opaque sync | Shared checks, `*credential_sync_test.go`, boot/device/seed fixtures; no universal atomic durability guarantee |
| HC-06 job turn hooks | Registered start/stop hooks | Registered start/stop hooks | Boot fixtures, cron correlation and dispatch marker tests; runtime version behavior needs live evidence |
| HC-07 task receipts/completion | Session receipt, explicit task tools | Session + optional turn receipt, explicit task tools | New `TestHarnessContractReceiptRecovery` plus task worker/store/API suites; historical MAT-28 live evidence is version-specific |
| HC-08 cancellation | notify_only | notify_only | `pkg/taskdispatch/cancellation.go` and task control tests; exact interruption unsupported |
| HC-09 common discovery/availability | Descriptor + pod-specific observations | Descriptor + pod-specific observations | Availability expiry/negative tests, API pod/runtime mismatch tests, native-config probes, task/session gates and discovery-driven UI |
| HC-10 model/usage reporting | Reporter/catalog paths | Reporter/catalog paths, unknown catalog context windows | `pkg/tokenreport`, runtime catalog tests; no guessed metric promises |
| HC-10 tools/channels | Configured MCP sidecars | Configured MCP sidecars | Adapter/startup tests; Telegram + API-key rejected by current API |
| HC-10 in-place repair | Adapter metadata | Adapter metadata | Existing adapter/installer/repair tests; real repair requires separate live evidence |

The small adapter fixture in `pkg/runtimes/contracttest` checks optional-command
absence and credential isolation. `pkg/api/runtime_extension_test.go` proves a
new descriptor and auth strategy can pass public discovery and credential
creation without provider branches.

The bootable process fixture in `test/integration/harness_contract_test.go`
uses its own registered runtime ID, real tmux, production paste/receipt scripts,
authenticated internal HTTP and PostgreSQL. It proves receipt persistence is
separate from completion, then explicitly completes, verifies the transcript,
and flushes on shutdown. It passed in integration CI run
[34064463947](https://github.com/matty-v/kyber/actions/runs/34064463947) against
`aa26d5c`, alongside the OpenAPI and native startup fixtures. Kubernetes state is simulated. This is neither a
live-pod test nor evidence that an upstream CLI obeys its native hook contract.

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

The disposable Codex agent `sol-test-mat7-codex` exposes its registered contract
and the canonical `/auth` endpoint reports `starting` while login is pending.
Both subscription test logins require operator consent. Successful model turns,
task completion and session restart remain pending; API-key fixtures are not
substituted for a successful live API-key login.

## Run contract checks

Go version comes from `go.mod`; scripts need bash, jq, curl, Python and git.
The new adapter and receipt tests participate in the existing `go test ./...`
CI gate; existing startup suites require the integration tag. No new job or
branch-protection permission is needed.

```sh
go test ./pkg/runtimes/...
go test ./images/agent-base -run TestHarnessContract -count=1
go test ./pkg/tokenreport ./pkg/taskdispatch ./images/agent-base
go test -tags=integration ./images/codex ./images/claude-code
# Requires PostgreSQL, Redis, tmux, Python, jq and curl:
go test -tags=integration ./test/integration -run TestHarnessContractBootReceiptAndExplicitCompletion
```

`pkg/runtimes/contracttest.CheckAdapter` takes provider-owned auth fixtures and
never switches on a runtime name. Add each supported mode, its selected Secret
suffix/keys, forbidden alternate-mode variables, and two Agent names. Current
fixtures exercise explicit model selection. A future integration without that
optional feature needs a fixture/checker extension that does not assert model
support. Do not turn this test-only fixture format into the production capability
schema without design review.

Receipt tests execute the production hook against a local HTTP server for both
current runtime IDs and the fixture ID: acceptance, connection loss after POST followed by exact
GET, unavailable service, mismatched identity, ordinary prompts, wrong hook event,
missing session identity, and conflicting local evidence. They check exit status,
request correlation, private file permissions, and absence of prompt persistence.
They do not prove the real harness obeys a hook's exit code or runs a model turn.

Declared/observed gates, unknown/stale reports and the bootable process fixture
now have automated coverage. Final native conformance still requires versioned
evidence for both real harnesses in dev; the matrix retains their documented
limitations rather than inferring behavior from the fake.

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
onboarding is already a four-file exercise. The approved architecture migration
must make those extension points explicit before final v1 publication.
