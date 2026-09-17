# MAT-79 optional model conformance plan

Status: implemented; awaiting review and CI. Tracker: MAT-79. Parent: MAT-7.

## Goal

Prove that a v1 interactive harness can satisfy the required baseline without
model discovery or explicit model selection. The shared checker, API, PWA, and
dispatch gates must derive that behavior from the runtime descriptor's
`model-catalog` declaration, never from a runtime ID.

## Current gap

- `contracttest.CheckAdapter` unconditionally requires `ModelEnvVar()` and a
  matching model environment entry, although HC-04 requires model overrides
  only when the integration supports them.
- The test extension runtime declares no optional features, but its adapter
  still exposes `FIXTURE_MODEL`, so it does not prove the advertised profile.
- agent model-catalog and set-model routes do not reject a runtime that omits
  `model-catalog`; creation and patch paths can also accept an explicit model.
- Agent Detail always advertises a Model action and the creation review always
  describes a fleet model default, regardless of the discovered descriptor.
- the onboarding guide identifies this work as future rather than documenting
  a complete tested no-model path.

## Implementation

### 1. Make the reusable checker capability-driven

- Pass the runtime's trusted `Descriptor` into `CheckAdapter`.
- When `model-catalog` is declared, require a nonempty model env-var name and
  verify the requested model reaches that exact environment entry.
- When it is absent, require `ModelEnvVar()` to be empty and prove the adapter
  does not inject a model entry.
- Convert the minimal third-runtime fixture to a real no-model-selection
  profile and add negative checker cases for declaration/adapter mismatch.
- Keep Claude Code, Codex, and Hermes on the same checker path; do not add a
  runtime-name switch or a production fixture runtime.

### 2. Fail closed at public model operations

- Centralize the descriptor check for explicit model operations.
- Reject an explicit model during create/patch for a runtime without
  `model-catalog`, with an actionable capability-based error.
- Return a visible unsupported response from per-agent model discovery and
  `set-model` for that profile before reading catalogs or mutating the Agent.
- Retain empty-model creation as the valid baseline: the harness owns whatever
  fixed/default behavior exists internally, and Kyber does not advertise a
  selection control.
- Exercise successful credential-backed creation plus all rejected model
  surfaces with the descriptor-driven extension fixture.

### 3. Prove model independence in availability and dispatch

- Give the extension fixture selected non-model features and current pod
  evidence, then prove their availability is derived independently from
  `model-catalog`.
- Exercise the shared durable-task capability gate with receipt/tool evidence
  for that fixture, demonstrating that task delivery neither requires nor
  invokes model discovery/selection.
- Keep unsupported and stale feature behavior explicit through the existing
  availability vocabulary.

### 4. Remove unavailable controls from the PWA

- Add one descriptor helper for model-selection support based on the
  `model-catalog` feature.
- Hide the Agent Detail Model button/dialog and avoid the authenticated catalog
  query when the runtime omits the feature. Render runtime/harness/resources
  without presenting model selection as an operation.
- Omit the creation Review model row for the same profile; continue omitting
  `model` from the create request.
- Add component tests for both the no-model fixture and existing production
  descriptors, then bump `@matty-v/kyber-pwa-views` and its changelog as
  required by the publish boundary.

### 5. Publish reusable onboarding evidence

- Define `model-catalog` as the declaration governing authenticated discovery
  and explicit selection surfaces; omission is conforming and fail-closed.
- Replace the conformance guide's future-work note with the tested minimal
  profile, exact extension points, expected unsupported behavior, and commands.
- Record the fixture/API/PWA/dispatch evidence in the support matrix and note
  that no shared feature path gained a Claude Code or Codex conditional.

## Validation

- `go test ./pkg/runtimes/... ./pkg/api/...`
- focused contract, extension-route, availability, and dispatch-gate tests
- `go build ./...`
- `go vet ./...`
- `go test ./...`
- pwa-views build, type-check, and Vitest suite
- embedded PWA build/type-check/test
- `git diff --check`
- required GitHub CI, including integration, contract/TCK, security, and image
  builds, before merge

## Boundaries

- This adds no production runtime, provider credential, dependency, CRD field,
  or new public response shape.
- `model-catalog` remains the stable wire value; this change documents and
  enforces its selection semantics rather than adding overlapping vocabulary.
- A harness without model selection may still report ordinary runtime health
  and execute work. Kyber simply cannot discover, validate, or change its
  upstream model through model-specific surfaces.
