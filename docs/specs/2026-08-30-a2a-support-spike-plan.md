# A2A protocol support spike — execution plan

**Status:** In progress
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
- [ ] Verify every material code/spec claim and run the repository's docs-only
  checks.
- [ ] Commit and push the focused branch and open a pull request for review.

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
