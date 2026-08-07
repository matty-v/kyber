# Kyber Architecture Docs — the HOW source of truth

This directory is kyber's **architecture source of truth**: the maintained,
living description of **how kyber is built** — its components, control flow,
state machines, data flow, key invariants, and cross-component contracts.

Start at [`overview.md`](overview.md) for the whole-platform picture, then
follow it into the focused subsystem deep-dives indexed below.

---

## Scope: HOW, not WHAT

This set documents the **HOW** — internal architecture. It deliberately does
**not** cover the **WHAT** — product capabilities, user-facing behaviors, and
concepts. That is a separate, sibling source of truth:

| Question | Lives in |
|---|---|
| *How is kyber built?* (components, control flow, contracts, invariants) | **`docs/architecture/`** (this set) |
| *What can kyber do?* (capabilities, behaviors, concepts) | **`docs/product/`** (see [kyber#397](https://github.com/matty-v/kyber/issues/397)) |

> The boundary, stated so a contributor knows where a new page belongs: if it
> describes a module, a control path, a state transition, an invariant the code
> enforces, or a contract one subsystem relies on from another → it belongs
> **here**. If it describes a feature a user invokes, an externally observable
> behavior, or a domain concept → it belongs in **`docs/product/`**.

Kyber relates to two sibling repos: the private deploy repo (ArgoCD
Applications + per-environment values) and the optional Holocron hub (a
multi-install PWA host). This set answers "how is kyber itself built."

---

## Ownership & maintenance

**Obi-wan (Architect) owns and maintains this doc set.**

- **Read before designing.** Obi-wan reads the relevant architecture docs —
  alongside the real code — before authoring any design.
- **Update on architecture change.** When a design changes the architecture
  (a new component, a changed control path, a new/removed phase or transition,
  a changed cross-component contract or invariant), updating the affected
  page(s) here is **part of that design's deliverable** — not a follow-up.
- **Builders read before coding.** Anyone implementing against a subsystem
  reads its deep-dive first (see [`AGENTS.md`](../../AGENTS.md)).
- **The code wins on conflict.** Every page names its authoritative source
  files. If a doc and the code disagree, the code is right and the doc is
  stale — fix the doc (and, ideally, file the drift).

The read/enforce wiring (Obi-wan reads before designing; builders read before
coding; Chewie enforces that architecture-changing PRs update these docs) is
tracked in the per-agent skill issues, not here.

---

## Index

| Page | Covers | Status |
|---|---|---|
| [`overview.md`](overview.md) | whole-platform hub: runtime components, CRDs, control-plane module map, lifecycle, inbound, build/release | entry point |
| [`agent-lifecycle.md`](agent-lifecycle.md) | the agent state machine: phases, events, actions, the full transition table, invariants | deep-dive |
| [`status-pipeline.md`](status-pipeline.md) | in-pod signal source → status sidecar → `Agent.status.activity` | deep-dive |
| [`telegram-sidecar.md`](telegram-sidecar.md) | Telegram polling, signed inbound delivery, MCP replies, capability bounds, and migration | deep-dive |
| [`metrics-data-flow.md`](metrics-data-flow.md) | metrics emission and aggregation flow | deep-dive |
| [`model-onboarding.md`](model-onboarding.md) | data-driven model onboarding: context-window auto-detect (Anthropic Models API) + feed-derived pricing (build-time vendored LiteLLM — **not** a runtime proxy) | deep-dive |
| [`metricsstore.md`](metricsstore.md) | the metrics store backing model | deep-dive |
| [`pwa-views-publish-boundary.md`](pwa-views-publish-boundary.md) | PWA views publish boundary | deep-dive |
| [`log-retention.md`](log-retention.md) | durable off-cluster agent log retention: Vector shipper → GCS → `source=archive` read path | deep-dive |
| [`internal-api-auth.md`](internal-api-auth.md) | per-agent identity + authz on the internal `:8082` API (pod-tokens, act-on-self-only, the `:8082`-scoped NetworkPolicy) | deep-dive |
| [`_TEMPLATE.md`](_TEMPLATE.md) | the per-subsystem deep-dive template — copy this to start a new page | template |

Candidate subsystems still awaiting a deep-dive (accrue as follow-ups, not all
in one pass): machine/capacity, API surface, runtime/pod build, inbound
(currently summarized in `overview.md` § 6), and telemetry (partly covered by
`metrics-data-flow.md` / `metricsstore.md`).

---

## Adding a deep-dive

1. Copy [`_TEMPLATE.md`](_TEMPLATE.md) to `docs/architecture/<subsystem>.md`.
2. Fill in every section. Keep it to architecture — components, control/data
   flow, contracts, invariants, failure modes — **not** line-level detail.
3. Name the authoritative **Source of truth** files so readers know the doc
   tracks the code.
4. Add a row to the **Index** table above and link the page from
   [`overview.md`](overview.md) § 8.
5. Cross-link the sibling `docs/product/` page if one exists, so the WHAT/HOW
   boundary stays navigable from both sides.
