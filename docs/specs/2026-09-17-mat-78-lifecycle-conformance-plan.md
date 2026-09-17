# MAT-78 — harness lifecycle conformance plan

Status: implementation complete; validation in progress.
Issue: https://linear.app/matty-v/issue/MAT-78
Parent: MAT-7.
Baseline: `a18e971` (`origin/main`, including MAT-76 and MAT-77).

## Outcome and boundaries

Turn the remaining harness lifecycle claims into a reviewable evidence map and
close the deterministic gaps for compaction, repair, replacement, capability
gating, and ambiguous task delivery. Every requirement will point either to a
named automated check or to an explicit staging-only procedure with an honest
reason it cannot be certified in CI.

This issue will not add a CRD field, lifecycle phase, runtime, dependency, or
credential path. It will not claim that delivering `/compact` proves upstream
compaction completed, that a fixture proves cloud storage behavior, or that
lost persistent storage can be reconstructed. MAT-79 remains responsible for
proving that a minimal third runtime can be added without Claude/Codex branches.

## Baseline findings

- Claude Code and Codex both declare `/compact`; the API and shared delivery
  script already cover unsupported runtimes, missing commands, non-running
  agents, cooldowns, missing sessions, restart locks, and delivery failures.
  Live upstream completion is asynchronous and therefore staging-only.
- Runtime repair already has API success/failure/conflict tests, same-node PVC
  construction checks, and installer verification fixtures. It lacks explicit
  timeout cleanup and proof that unrelated durable identity, session, and
  credential files survive both successful and failed repair attempts.
- Managed-machine recovery already parks an agent without spending its retry
  budget and recreates the pod on the replacement node. The test does not yet
  bind that replacement to the same PVC and credential Secret or demonstrate
  that the old pod's capability observation cannot authorize the new pod.
- Session recall extraction is deterministic for Claude and Codex. Physical
  volume/node survival is storage-class and provider behavior, so it requires
  staging evidence. Loss of the volume remains an explicit loss boundary.
- Current-pod capability evidence already fails closed for hook-dependent
  operations. Compaction and runtime repair intentionally use direct in-pod or
  maintenance checks rather than hook evidence.
- PostgreSQL reconciliation implements the conservative task rule: an expired
  `attempting` or `receipt_pending` delivery becomes `delivery_unknown` and is
  never automatically replayed. That exact database transition currently lacks
  a dedicated integration regression.

## Design

### 1. Versioned lifecycle evidence catalog

Add a machine-readable catalog under `test/contract/` covering HC-01 through
HC-10. Each lifecycle scenario will identify its runtime
scope, guarantee, evidence kind, named automated checks, and any staging-only
step or residual loss boundary.

A standard-library validation test will fail when a required scenario or
Claude/Codex row is absent, an automated reference no longer resolves to a
test, a staging-only claim has no rationale/procedure, or the catalog attempts
to store credential material. The architecture conformance matrix will link to
this catalog rather than duplicating an unverifiable checklist.

### 2. Deterministic behavior gaps

Add focused regressions without real provider credentials:

- exercise compaction declarations and API outcomes for Claude, Codex, and a
  minimal unsupported fixture, while retaining the existing asynchronous
  delivery boundary;
- force the maintenance repair runner through success, failure, and a bounded
  context timeout, asserting cleanup of its short-lived pod;
- seed unrelated durable identity, recall, and synthetic credential-sentinel
  files around repair script fixtures and assert byte content and modes are
  unchanged after successful and failed repair;
- extend managed-machine recovery to prove the replacement pod mounts the same
  PVC, the per-agent credential Secret is unchanged, restart accounting is not
  spent, and old-pod capability evidence is cleared/fails closed;
- add a PostgreSQL integration regression proving expired ambiguous attempts
  terminate as `delivery_unknown`, emit terminal evidence, and cannot be
  reclaimed or blindly redelivered.

These checks use synthetic values only. They do not revoke, rotate, or destroy
real credentials.

### 3. Staging qualification runbook

Add an operator runbook for the combined MAT-76 through MAT-79 pre-release
matrix. It will pin image digests and runtime versions; use disposable agents
and scoped credentials; exercise Claude/Codex compaction, repair, pod
replacement, node replacement, and recoverable storage reattachment; record
task ambiguity and capability observations; and require cleanup evidence.

The runbook will distinguish delivery from observed upstream completion and
recoverable volume reattachment from destructive volume loss. Destructive
credential or persistent-volume deletion is not required. If an optional loss
drill is deliberately authorized, the expected result is an honest recovery
boundary and reauthentication—not fabricated continuity.

### 4. Contract and conformance documentation

Update the v1 contract and conformance guide with the catalog, the CI/staging
split, capability-evidence ownership by pod UID, task ambiguity behavior, and
the exact storage/credential loss boundaries. This is evidence clarification;
it does not silently broaden the normative runtime interface.

## Test matrix

| Layer | Required cases |
|---|---|
| Evidence catalog | required HC/scenario coverage, both production runtimes, valid test references, explicit staging rationale, no secret-bearing fields |
| Compaction | Claude and Codex delivery contract; unsupported/unavailable runtime; missing session, lock, delivery failure; live completion deferred explicitly |
| Runtime repair | success, install/verify failure, context timeout, maintenance-pod cleanup, unrelated durable-file preservation, no service-account token |
| Pod/node recovery | park/resume without retry spend, same PVC, unchanged credential Secret, recall path, stale capability invalidation, replacement-node affinity |
| Task ambiguity | expired attempting and receipt-pending deliveries become terminal `delivery_unknown`, emit evidence, and are not reclaimed |
| Capability gates | missing, stale, wrong-pod, negative, undeclared, and current-positive observations with actionable reasons |
| Staging | pinned Claude/Codex matrix, cleanup, evidence template, explicit provider/storage limitations |
| Regression | focused packages, integration fixtures, `make build`, `make lint`, `make test`, documentation checks |

## Checkpoints

- [x] Load MAT-78 and create a clean worktree from current `origin/main`.
- [x] Trace compaction, repair, machine recovery, session recall, capability
  gates, credential recovery, and ambiguous task delivery against MAT-78.
- [x] Record the CI/staging boundary and implementation plan.
- [x] Add and validate the lifecycle evidence catalog.
- [x] Add the missing deterministic behavior regressions.
- [x] Add the staging qualification runbook and update contract/conformance
  documentation.
- [x] Run focused suites, full build/lint/test gates, and review the complete
  diff for false claims, secret exposure, and provider-specific branching.
- [ ] Open a PR, require green CI, merge, and retain MAT-78 in testing until the
  combined MAT-76 through MAT-79 staging matrix passes before the Kyber release.

Local evidence: the catalog validator, compaction/repair API tests, repair
integration fixtures, machine-recovery envtest, all runtime packages, shell
syntax, product-documentation checks, `go build ./...`, and `go vet ./...`
pass. The PostgreSQL integration suite compiles; its database-backed execution
requires integration CI because this workspace has no Docker/PostgreSQL
service. The aggregate `go test ./...` passed every reported package except
`pkg/controllers/agent`, whose many serial envtest control planes exhausted the
package's 10-minute timeout after an API-server startup timeout. The exact
modified controller test passes independently with the same envtest assets.
