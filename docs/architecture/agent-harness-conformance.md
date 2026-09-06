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

## Initial matrix

Baseline source: 20b21f987402204b33944af9135db53d4cbf494b. Test results and review
checkpoint are recorded in the [execution plan](../specs/2026-09-06-mat-7-harness-contract-plan.md).
No new live evidence is claimed by this docs/tests checkpoint.

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
| HC-09 common discovery/availability | Gap | Gap | Nil-command and sentinel behavior exists; shared capability schema/gates require approved refactor |
| HC-10 model/usage reporting | Reporter/catalog paths | Reporter/catalog paths, unknown catalog context windows | `pkg/tokenreport`, runtime catalog tests; no guessed metric promises |
| HC-10 tools/channels | Configured MCP sidecars | Configured MCP sidecars | Adapter/startup tests; Telegram + API-key rejected by current API |
| HC-10 in-place repair | Adapter metadata | Adapter metadata | Existing adapter/installer/repair tests; real repair requires separate live evidence |

The minimal fixture in `pkg/runtimes/contracttest` registers through the real
registry and passes the same adapter checker with optional commands absent.
Negative fixtures prove cross-agent credential references and empty optional
argv are detected. **It is not a fully bootable third harness.** In particular,
the shared receipt script currently rejects its runtime ID. Removing that
whitelist belongs to the architectural migration, followed by an end-to-end
fake harness proving the resulting extension boundary.

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
```

`pkg/runtimes/contracttest.CheckAdapter` takes provider-owned auth fixtures and
never switches on a runtime name. Add each supported mode, its selected Secret
suffix/keys, forbidden alternate-mode variables, and two Agent names. Current
fixtures exercise explicit model selection. A future integration without that
optional feature needs a fixture/checker extension that does not assert model
support. Do not turn this test-only fixture format into the production capability
schema without design review.

Receipt tests execute the production hook against a local HTTP server for both
current runtime IDs: acceptance, connection loss after POST followed by exact
GET, unavailable service, mismatched identity, ordinary prompts, wrong hook event,
missing session identity, and conflicting local evidence. They check exit status,
request correlation, private file permissions, and absence of prompt persistence.
They do not prove the real harness obeys a hook's exit code or runs a model turn.

Before claiming final conformance, extend coverage for declared/observed
capability gates, unknown/stale reports, a complete bootable fake harness, and
both real harnesses in dev. The matrix is deliberately incomplete while the
production contract implementation remains incomplete.

## Onboarding checklist

1. Choose the v1 interactive profile and stable runtime ID. Map each HC requirement
   to native, adapter, or platform behavior, auth modes and version limits.
2. Add `pkg/runtimes/<id>/` with Runtime/Adapter/Probe and path constants. Register
   via init and add the binary imports that enable it. Reuse the shared checker.
3. Supply image and shared entrypoint integration, `KYBER_START_CMD`,
   `KYBER_RUNTIME_DEFAULT_VERSION`, installed/requested-version reporting,
   launch/watchdog and termination handling. Configure chart image defaults,
   build/release wiring, image lookup and installed-version discovery.
4. Define selected-mode credential references, operator login/reauth UI/API,
   refresh/write-back, stale-seed protection and failure classification. Today
   these require additional extension work outside the Go adapter; see review.
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
   the production capability declaration and operator behavior agree once that
   mechanism exists. Unsupported combinations must be visible.
10. Obtain review, publish through existing repository release processes, and
    rerun version-sensitive checks on CLI/image upgrades. Do not auto-promote
    harness features into public service manifests.

This checklist identifies the current extra work; it does not claim third-party
onboarding is already a four-file exercise. The approved architecture migration
must make those extension points explicit before final v1 publication.
