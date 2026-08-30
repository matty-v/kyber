# A2A protocol support spike — execution plan

**Status:** Complete
**Date:** 2026-08-30
**Tracker:** MAT-6
**Branch:** `docs/mat-6-a2a-support-spike`

## Goal

Define what honest, formal A2A 1.0 support would require for Kyber agents and
give Matt a costed recommendation that can be accepted or declined without
starting a transport implementation.

## Delivery rules

- Treat inbound A2A server support and outbound A2A client support as separate
  products unless the evidence establishes a shared Kyber boundary.
- Ground protocol claims in the current official A2A specification, schemas,
  SDKs, and conformance artifacts; record exact versions and retrieval dates.
- Ground the gap analysis in current `main`, including the existing internal
  agent request/reply path added after MAT-6 was written.
- Prefer an independently useful first phase over a partial compliance claim.
- Do not add protocol, task-store, or Agent Card implementation code.

## Investigation

- [x] Re-verify A2A 1.0 operations, bindings, version negotiation, task
  lifecycle, Agent Card, authentication, streaming, push notifications,
  idempotency, and error requirements.
- [x] Determine whether an official conformance suite, certification program,
  or normative implementation profile exists; define Kyber's support bar.
- [x] Trace Kyber's inbound webhook, internal agent request/reply, queue,
  persistence, runtime completion, authentication/authorization, and skills
  metadata boundaries with exact file references.
- [x] Compare JSON-RPC, HTTP+JSON/REST, and gRPC against Kyber's current API and
  operational model.
- [x] Decide where addressable tasks and their result artifacts would live and
  how lifecycle transitions survive control-plane or agent restarts.
- [x] Decide how an Agent Card is configured or derived without overstating an
  agent's actual capabilities.
- [x] Separate server and client scope, dependencies, security boundaries, and
  costs.

## Deliverable

- [x] Write `docs/design/2026-08-30-a2a-protocol-support.md` in the repository's
  design-doc shape.
- [x] Include a recommendation, alternatives, phased plan, independently
  shippable phase 1, rough engineering sizes, risks, and explicit `not yet`
  option.
- [x] Verify every material code/spec claim and run the repository's docs-only
  checks.
- [x] Commit and push the focused branch and open a pull request for review.

## Progress log

- 2026-08-30: Matt approved the investigation plan. Updated to `origin/main`
  at `630b7e9`; current main already contains an authenticated internal
  request/reply path, so the ticket's initial “no task model” assessment must
  be re-evaluated against that newer work.
- 2026-08-30: Verified the protocol against the A2A v1.0.1 release artifacts,
  official Go SDK v2.5.0, and official TCK commit `5996b79`. The TCK exists but
  is untagged and no certification authority or registry exists; the design
  defines a pinned, self-attested conformance bar.
- 2026-08-30: Drafted the recommendation: do not build without a named caller;
  split server and client work; use HTTP+JSON and PostgreSQL for a formal
  server; preserve MAT-9's explicit sidecar-authenticated completion boundary.
- 2026-08-30: Repository docs-only checks passed, the branch was pushed, and
  pull request #183 was opened for review.
- 2026-08-30: Matt asked to evaluate the path gap by gap based on native Kyber
  value. Reframed the recommendation as a twelve-item dependency-ordered gap
  register, replaced the monolithic delivery choice, and added the first G1
  decision brief for durable agent tasks.
- 2026-08-30: Matt chose to pursue G1 as a separate design. Created MAT-19 with
  MAT-6 and PR #183 references, recorded G1 as pursued, and prepared G2
  (task-scoped progress and typed results) for the next independent decision.
- 2026-08-30: Matt chose to pursue G2 and asked whether results include
  multimodal output. Confirmed that A2A Artifacts support text, structured data,
  inline bytes, and media URLs; created MAT-20 to design Kyber's bounded native
  subset, recorded G2 as pursued, and prepared G3 (cooperative cancellation) for
  the next independent decision.
- 2026-08-30: Matt chose to pursue G3. Created MAT-21 with MAT-6, PR #183,
  MAT-19, and MAT-20 references, recorded cooperative cancellation as pursued,
  and prepared G4 (multi-turn task continuation) for the next independent
  decision.
- 2026-08-30: Matt chose to pursue G4. Created MAT-22 with MAT-6, PR #183,
  MAT-19, and MAT-20 references, recorded multi-turn continuation as pursued,
  and prepared G5 (principal-scoped task authorization) for the next
  independent decision.
- 2026-08-30: Matt chose to pursue G5 and clarified the intended architecture:
  Kyber should be a thin adapter over Claude Code and Codex concepts, exposing
  one durable formal platform envelope rather than rebuilding an agent loop.
  Added that guardrail and a harness-capability audit to MAT-6 and MAT-19–22,
  created MAT-23, recorded G5 as pursued, and prepared G6 (machine-readable
  public capability contract) for the next decision.
- 2026-08-30: Matt chose to pursue G6. Created MAT-24 under the thin-adapter
  guardrail, recorded the curated capability manifest as pursued, and prepared
  G7 (live event subscriptions) for the next independent decision.
- 2026-08-30: Matt chose to pursue G7. Created MAT-25 for normalized, durable,
  resumable task events, recorded G7 as pursued, and prepared optional G8
  (outbound task webhooks) with a recommendation to defer it until a concrete
  disconnected consumer exists.
- 2026-08-30: Matt chose to defer G8. Recorded push notifications as unsupported
  in the initial A2A profile with a concrete-consumer revisit trigger, and
  prepared G9 (federated external identity) with a recommendation to use G5's
  static service principals initially and defer OAuth2/OIDC.
- 2026-08-30: Matt chose to defer G9. Recorded MAT-23 service principals as the
  initial A2A security profile and prepared G10 (Agent Card projection) with a
  recommendation to bundle this A2A-only projection into the eventual protocol
  adapter rather than create another native feature design.
- 2026-08-30: Matt chose to bundle G10 with the eventual A2A adapter. Recorded
  MAT-24 as the sole native capability source, opened no separate G10 issue,
  and prepared G11 (A2A HTTP+JSON binding) as the thin standards adapter that
  delivers the original interoperability goal.
