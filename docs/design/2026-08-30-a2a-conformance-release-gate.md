# Evidence-backed A2A conformance release gate

**Status:** Proposed
**Date:** 2026-08-30
**Tracker:** [MAT-27](https://linear.app/matty-v/issue/MAT-27/designrelease-evidence-backed-a2a-conformance-gate)
**Gates:** [MAT-26](2026-08-30-a2a-http-json-edge-adapter.md)
**Origin:** [MAT-6](https://linear.app/matty-v/issue/MAT-6/spikeplatform-what-formal-a2a-protocol-support-would-require-for-kyber), [A2A gap study](2026-08-30-a2a-protocol-support.md), and gap-study PR #183

## 1. Decision

Kyber may claim evidence-backed A2A 1.0 HTTP+JSON support only when a
checked-in applicability ledger and layered automated test suite pass against
the deployed MAT-26 adapter. The evidence pins the specification, SDK, TCK,
Kyber build, declared features, and test environment.

By Matt's decision on 2026-08-30, the deployed official TCK is a required check
for every adapter-affecting pull request, not merely a release-time check. All
applicable normative MUST requirements, Kyber security/restart/retention
extensions, and an independent-client interoperability smoke must pass before
merge. SHOULD deviations require explicit, time-bounded review.

The official A2A TCK is a compatibility tool, not a certification authority.
The TCK repository is evolving and currently has no stable release tag that
Kyber can float against safely. Kyber therefore pins an exact commit and says
“tested” or “self-attested conformance,” never “certified,” unless a recognized
certification program later verifies a release.

## 2. Verified ground truth

The official [A2A TCK](https://github.com/a2aproject/a2a-tck) supports gRPC,
JSON-RPC, and HTTP+JSON transports. It discovers declared interfaces from the
Agent Card, groups coverage by RFC 2119 MUST/SHOULD/MAY level, and emits
machine-readable compatibility JSON plus HTML and JUnit reports. Kyber runs
only the declared HTTP+JSON transport while ensuring undeclared transports are
not accidentally advertised.

The evidence baseline is pinned as one set:

- A2A specification artifact: `1.0.0` and its release commit/digest;
- wire protocol version: `1.0`;
- official Go SDK: `github.com/a2aproject/a2a-go/v2` v2.4.0, resolved commit and
  module checksum;
- official TCK: exact commit SHA plus `uv.lock` digest;
- independent client: exact official Python or JavaScript SDK version/commit,
  deliberately different from the server's Go SDK; and
- Kyber source commit, image digests, chart version, feature flags, and Agent
  Card digest.

Updating any input creates a new evidence baseline. No job checks out upstream
main or installs `latest`.

## 3. Claim boundary

The public claim is narrow:

> Kyber `<version>` is self-tested for A2A Protocol 1.0 over HTTP+JSON with
> Bearer authentication and streaming, for the capabilities and limits in the
> linked support matrix. Evidence was produced by the pinned official TCK and
> Kyber's supplemental suites at `<evidence digest>`.

Do not say:

- certified, officially approved, fully A2A-compliant, all-transports, or all
  optional features;
- that an SDK or TCK pass proves security, reliability, model quality, or
  downstream tool behavior; or
- that deferred push notifications, OAuth2/OIDC, JSON-RPC, gRPC, extensions,
  or extended/signed Agent Cards are supported.

An Agent Card, support matrix, and release note must agree. If evidence is
missing, stale, revoked, or failed, the product may describe the adapter as
experimental/A2A-shaped but cannot make the evidence-backed support claim.

## 4. Normative applicability ledger

Check in a machine-readable ledger under a future path such as:

```text
conformance/a2a/1.0/requirements.yaml
```

Each row contains:

```yaml
- id: A2A-1.0-HTTP-MUST-042
  source:
    artifact: a2a-spec-1.0.0
    section: "3.6.2"
    quoteDigest: sha256:...
  level: MUST
  topic: version-negotiation
  applicability: applicable
  reason: MAT-26 exposes HTTP+JSON 1.0
  declaredFeature: core
  tests:
    - tck: tests/http_json/test_version.py::test_unsupported_version
    - kyber: TestA2AHTTPVersionUnsupported
  owner: platform
  lastReviewed: 2026-08-30
```

Allowed applicability values are `applicable`, `not-applicable`, and
`deferred-optional`. Every non-applicable/deferred row includes a precise
reason tied to the Agent Card/support matrix and a reviewer. “TCK does not test
it” is not a reason to mark a normative requirement inapplicable.

The ledger covers every MUST and SHOULD in the pinned common model,
operations, security guidance that applies to the declared profile, and
HTTP+JSON binding. MAY requirements are included when Kyber declares the
corresponding optional capability. A generator fails on duplicate IDs,
unresolved tests, missing source digest, unknown level/status, stale review,
or a specification requirement absent from the inventory.

## 5. Gate policy

| Requirement | Merge | Release |
| --- | --- | --- |
| Applicable MUST | all pass | all pass on release candidate |
| Declared optional feature | all normative tests pass | all pass and advertised accurately |
| SHOULD | pass or approved deviation | deviations still valid and published if user-visible |
| MAY not declared | skip with ledger reason | absent from card/support matrix |
| Kyber security/reliability | all applicable pass | full deployed suite passes |
| Independent-client smoke | pass on adapter-affecting PR | pass on release candidate |

No percentage threshold can compensate for one failing applicable MUST. A
green aggregate score with a failed MUST is red. Unsupported G8/G9 operations
are tested for their normative error behavior rather than skipped entirely.

A SHOULD deviation records requirement ID, behavior, rationale, user impact,
owner, approval, expiry, upstream issue if relevant, and planned resolution.
Expired deviations fail the gate. A MUST cannot be waived through the SHOULD
process; changing applicability requires specification evidence and security/
protocol-owner approval.

## 6. Change detection

The repository maintains an explicit manifest of adapter-affecting paths:

- MAT-26 edge, translators, SDK wrapper, routes, middleware, Agent Card and
  A2A feature configuration;
- MAT-19–25 service interfaces/types used by translation;
- authentication/authorization, task/result/event persistence and migration;
- gateway/CORS/SSE/deployment configuration;
- pinned specification/SDK/TCK/client/ledger inputs; and
- conformance workflows and fixtures.

Pull requests touching those paths require the deployed TCK check. A generated
dependency graph or checked path list is itself tested; changing the list
requires conformance-owner review. Maintainers can manually apply the gate to
any PR, but cannot remove it from a detected PR without the documented
exception process.

Pure translator/unit tests run on every PR regardless of paths because they
are fast. The deployed gate uses a reusable workflow that becomes a required
GitHub status check on adapter-affecting branches.

## 7. Test layers

### Layer 1: static and pure translation

Run on every PR:

- applicability ledger schema/completeness and support-matrix generation;
- SDK import boundary (no A2A dependency in native core packages);
- golden Agent Card, Task, Message, Part, Artifact, state, event, error, and
  pagination translations;
- official examples plus independently encoded fixtures;
- fuzz/property tests for JSON, unknown enums, limits, cursors, and round trips;
- forbidden-field scans for prompts, reasoning, paths, secrets, internal URLs,
  and runtime metadata; and
- OpenAPI/CRD/product/release guards already used by Kyber.

### Layer 2: native service integration

Run on every adapter-affecting PR:

- real PostgreSQL task/event stores and migrations;
- create/get/list/continue/cancel/results/SSE transaction and idempotency
  behavior;
- owner/tenant/agent authorization and non-enumeration;
- restart, unknown commit, event replay/expiry, retention, and revocation;
- bounded multimodal/file results and download reauthorization; and
- both Claude Code and Codex adapter envelopes with fake/deterministic harness
  endpoints where model behavior is irrelevant.

### Layer 3: deployed official TCK

Create an ephemeral namespace/installation from the PR's exact images, apply a
minimal MAT-24 capability declaration, mint a least-privilege MAT-23 service
principal, and run the pinned official TCK with `--transport http_json`.

Run all applicable levels, not only `--level must`, so SHOULD results remain
visible. The gate parses compatibility JSON and JUnit against the ledger rather
than relying only on the TCK process exit code. The Agent Card must declare
only HTTP+JSON so the TCK cannot silently test or excuse an unintended binding.

### Layer 4: Kyber adversarial and lifecycle suite

The official TCK does not prove Kyber's platform guarantees. Add deployed
tests for:

- guessed/cross-owner/cross-tenant/cross-agent IDs and page/cursor swapping;
- malformed/oversized/recursive Parts, MIME confusion, SSRF, unsafe files, and
  credential/header/log leakage;
- API/control-plane/harness/sidecar restart at dispatch, completion,
  cancellation, interaction, and stream boundaries;
- PostgreSQL failover, Redis loss, missed notifications, stale attempts,
  expired events/results/tasks, and key/session revocation;
- concurrent idempotent sends/cancels/continuations and terminal races;
- first stream snapshot, ordered events, reconnect, slow consumer, and
  terminal closure; and
- unsupported push/OIDC/JSON-RPC/gRPC/extensions returning or advertising the
  exact intended behavior.

### Layer 5: independent-client interoperability

Use a pinned official Python SDK client (or another non-Go official client) to
discover the card, authenticate, create a task, poll/list it, consume SSE,
continue input when requested, read typed artifacts/files, cancel a disposable
task, and verify unsupported features. The client constructs and parses its
own wire types; it does not share MAT-26 Go SDK code or golden JSON.

At least one smoke uses the public gateway path and TLS, not an in-cluster
service URL. It proves proxy/header/SSE behavior that an in-process TCK misses.

## 8. CI topology and lifecycle

The adapter-affecting PR workflow:

1. builds immutable control-plane/runtime images and records digests;
2. creates a unique ephemeral namespace and isolated database schema;
3. installs Kyber with only MAT-26's declared A2A feature set;
4. creates deterministic disposable Claude Code and Codex test agents or
   controlled harness fixtures according to the test layer;
5. waits for explicit readiness, capability, and authentication conditions;
6. runs Layers 2–5 with per-test and total deadlines;
7. collects evidence before cleanup; and
8. explicitly deletes agents, namespace, database objects, credentials, and
   object-store artifacts even after failure.

Use one workflow concurrency key per PR and cancel superseded commits. Cleanup
is a separate `always()` job with a periodic orphan sweeper as defense in
depth. Names include repository/PR/run attempt but no branch content that could
escape naming rules.

Nightly runs exercise current main with a wider restart/load matrix. Release
candidates rerun all layers from the exact signed candidate images and publish
the immutable evidence bundle. A nightly pass cannot substitute for a required
PR or release-candidate run.

## 9. Hermeticity and secrets

Pin every action by commit SHA, container by digest, module/client/TCK by
version+commit/checksum, Python environment by `uv.lock`, and OS/toolchain
image by digest. Vendor or cache the pinned TCK dependency for availability,
while retaining upstream origin/license/digest metadata. Network access during
the test is denied except to the SUT and explicitly required package setup that
cannot be vendored.

Fixtures use deterministic clocks/IDs where protocol permits and bounded test
content. They do not depend on an external model producing exact prose.
Runtime behavioral smokes assert envelopes/state transitions and use dedicated
test credentials with minimal scope, short lifetime, and isolated resources.

Secrets are provided through the CI secret boundary, never command arguments,
test reports, TCK HTML, Agent Cards, logs, traces, screenshots, or retained
task payloads. Redact Authorization/cookies/tokens before artifact upload and
scan the bundle for credential patterns. Fork/untrusted PRs cannot receive
secrets or deploy to shared environments; they run safe static layers until a
trusted maintainer-approved workflow executes the deployed gate.

## 10. Flakes, retries, and quarantine

A failing required check fails the PR. Automatic retry may occur once only for
a classified infrastructure setup/teardown failure before any assertion runs.
An assertion failure is never auto-rerun into green. The first failing evidence
is retained even if a manually triggered rerun passes.

To call a test flaky requires at least two contradictory results on the same
immutable inputs plus an issue and owner. Required MUST/security tests cannot
be quarantined. A non-MUST supplemental test may be quarantined only with
protocol+security approval, a narrow ledger entry, expiry no longer than 14
days, and an equivalent temporary guard where feasible. Expiry fails CI.

Track retry, quarantine, duration, and failure rates. Chronic infrastructure
failure is a release reliability defect; the team does not normalize reruns as
the workflow.

## 11. Upstream TCK defects

When evidence shows the pinned TCK conflicts with the pinned specification:

1. capture the smallest reproduction and exact source sections;
2. open/link an upstream issue;
3. add a local regression that asserts the specification-correct behavior;
4. record a narrow TCK patch or expected failure keyed to exact test ID and
   pinned commit; and
5. require protocol-owner approval and an expiry.

Never mark the underlying normative requirement inapplicable merely because
the TCK is wrong. Store patches in the repository, apply them with digest
verification, and show them prominently in the evidence/support matrix. Remove
the patch when upgrading to a fixed commit.

## 12. Evidence bundle

Every adapter-affecting PR run retains a signed/checksummed bundle containing:

```text
manifest.json                 pinned inputs, Kyber/images/config, timestamps
requirements.json             resolved applicability and results
support-matrix.json           generated public claim boundary
tck/compatibility.json
tck/compatibility.html
tck/junit.xml
kyber/junit.xml               integration/security/lifecycle/client suites
logs/                         redacted bounded diagnostic logs
attestation.intoto.jsonl      workflow/provenance attestation when available
SHA256SUMS
```

The manifest includes workflow repository+commit, run URL/ID, environment
class, Agent Card digest, test counts by level/outcome, deviations, TCK patches,
and cleanup result. Evidence is immutable after publication; corrections
produce a new bundle linked to the superseded one.

Retain PR evidence for at least 30 days, main/nightly evidence for 90 days, and
release evidence for the supported lifetime of the release plus one year.
Publish release bundle digests and support-matrix links in release notes. Store
raw task content only when it is synthetic and classified safe.

## 13. Generated support matrix

Generate, do not hand-maintain, a machine-readable and human-readable matrix:

```json
{
  "claim": "self-tested-conformance",
  "specification": "1.0.0",
  "protocolVersion": "1.0",
  "bindings": ["HTTP+JSON"],
  "securitySchemes": ["Bearer"],
  "capabilities": {"streaming": true, "pushNotifications": false},
  "unsupported": ["OAuth2/OIDC", "JSON-RPC", "gRPC", "extensions", "extendedAgentCard"],
  "limitsRef": "...",
  "deviations": [],
  "sdk": {"module": "github.com/a2aproject/a2a-go/v2", "version": "v2.4.0", "commit": "..."},
  "tck": {"commit": "...", "patches": []},
  "evidenceDigest": "sha256:..."
}
```

Fail generation when the MAT-26 card, feature flags, ledger, and expected matrix
disagree. The published matrix never includes credentials, internal routes,
test principal IDs, task content, cluster identifiers, or private MAT-24
evidence.

## 14. Release workflow

A release claiming A2A support requires:

- all required branch protections green on the final source commit;
- full release-candidate Layers 1–5 against exact candidate image digests;
- zero failed applicable MUST/security tests and no expired deviations;
- reviewed generated matrix/card/deviation diff from the prior release;
- successful cleanup and secret scan;
- signed evidence manifest/checksums and retained bundle;
- release notes with claim language, limits, known deviations, pinned inputs,
  evidence link/digest, and upgrade/rollback notes; and
- named protocol and security approvers.

If a post-release defect invalidates evidence, publish an advisory, mark the
evidence/support claim withdrawn or superseded, disable affected capability
advertisement where possible, and cut a fix/rollback. Never silently replace
the old evidence artifact.

## 15. Dependency and protocol upgrades

Specification, SDK, TCK, and independent-client upgrades are separate,
reviewable PRs where possible. Each upgrade:

1. updates one pinned input and records upstream release/commit diff;
2. regenerates the normative inventory and shows added/removed/changed rows;
3. reviews SDK API/wire/default/security changes against MAT-26 boundaries;
4. runs both the old and new evidence baselines where compatible;
5. updates mappings, tests, support matrix, known deviations, and migration
   notes explicitly; and
6. retains the old pin for immediate rollback.

No dependency bot auto-merges these updates. A TCK change that adds tests does
not redefine the spec automatically; a spec change does not become supported
until MAT-26 version negotiation/card declarations and the ledger are updated.

## 16. Ownership and operations

The platform/A2A adapter owner maintains the ledger, translators, TCK pin,
client smoke, and support matrix. Security owns approval of auth,
non-enumeration, SSRF, secrets, and required-test exceptions. Release
engineering owns required checks, evidence retention/attestation, and release
gates. Every row/deviation/quarantine has a named owner, not only a team label.

Dashboards track gate duration, pass/fail by layer, MUST/SHOULD coverage,
deviations/quarantines nearing expiry, TCK age vs upstream, flakes/retries,
cleanup leaks, evidence upload/secret-scan failures, and last successful
release-evidence digest. Alert on required-check bypass or a published support
matrix without matching retained evidence.

## 17. Rollout

1. Check in pinned-input manifest, initial 1.0 applicability ledger, generators,
   and static translator tests before enabling the public adapter.
2. Add hermetic ephemeral deployment and official HTTP+JSON TCK; run in
   advisory mode until infrastructure reliability and ledger mapping are
   reviewed.
3. Add security/lifecycle and independent-client layers, evidence bundle, and
   support matrix.
4. Make deployed TCK plus supplemental suites required for adapter-affecting
   PRs, as selected; do not claim formal support yet.
5. Run the first release-candidate evidence suite, review deviations, publish
   the bundle/matrix, and only then enable evidence-backed claim language.
6. Add nightly wider matrices and deliberate upgrade automation without
   weakening pins or approval.

Rollback disables the claim and adapter/card advertisement independently of
native tasks. Existing tasks remain governed by MAT-19–25. Evidence remains
immutable and records why the claim was withdrawn.

## 18. Estimate and cost

Estimated implementation is **3–5 engineer-weeks** after MAT-26: ledger and
generators (0.5–1 week), hermetic deployed TCK workflow (1–1.5 weeks),
security/lifecycle/independent client suites (1–1.5 weeks), and evidence/release
integration (0.5–1 week).

Expected CI cost is controlled by change detection, one ephemeral environment
per adapter-affecting PR head, cancellation of superseded runs, and a small
deterministic agent set. Target required-gate duration is under 20 minutes;
nightly load/failover matrices may take longer and cannot replace the gate.

## 19. Out of scope

- a certification authority or general conformance SaaS;
- implementing MAT-26 or deferred G8/G9 features merely to satisfy tests;
- testing undeclared JSON-RPC/gRPC as supported transports;
- proving model output quality or third-party connector behavior;
- floating dependency/TCK upgrades; and
- weakening native architecture solely to match a faulty fixture.

## 20. Acceptance criteria

- A complete pinned A2A 1.0 applicability ledger maps every relevant MUST,
  SHOULD, and declared option to tests or reviewed applicability.
- Adapter-affecting PRs cannot merge when the deployed HTTP+JSON TCK or required
  Kyber supplemental/independent-client suites fail.
- Applicable MUSTs and security tests have no waiver path; SHOULD deviations
  and narrow upstream-TCK patches are explicit, owned, reviewed, and expiring.
- Test environments are hermetic, least-privilege, cleaned explicitly, and
  produce secret-scanned immutable evidence.
- Release evidence identifies exact spec/SDK/TCK/client/Kyber/image/card inputs
  and generates the public support matrix.
- Claim language says tested/self-attested, never certified, and advertises
  only MAT-26's actual HTTP+JSON Bearer+streaming profile.
- Protocol/dependency upgrades are deliberate diffs with evidence and rollback.

