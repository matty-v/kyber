# Kyber Product Docs — the WHAT

This is the **product source of truth** for Kyber: a maintained description of
**what the product does** — its capabilities, observable behaviors, and concepts
— from an operator's vantage point.

## Scope: WHAT, not HOW

These pages describe **what an operator sees and can do**, and what the product's
states and concepts *mean*. They deliberately contain **no implementation
detail** — no file paths, function names, controller internals, or wiring. That
is the **HOW**, and it lives in its sibling set, the architecture docs:
[`../architecture/`](../architecture/overview.md).

The two sets are a matched pair and cross-link to each other so the boundary is
navigable from either direction:

| Question | Doc set |
|---|---|
| *What does Kyber do? What does an operator see and do?* | **`docs/product/`** (here) — the WHAT |
| *How is Kyber built? How does a subsystem work?* | [`docs/architecture/`](../architecture/overview.md) — the HOW |

If a contributor is unsure where something belongs: describe observable
**behavior and concepts** here; describe **mechanism** (components, data flow,
state machines, code) in `docs/architecture/`. When a state or concept is named
in both, the architecture set owns the authoritative names and this set links to
it rather than re-deriving them — keeping the vocabulary from drifting.

## Ownership and cadence

**Yoda (Product) maintains this doc set.** Yoda reads the relevant page before
triage / acceptance-criteria / spec work, and **refreshes these pages at ship /
release-notes time** — the same moment user-perspective release notes are
written, so the product docs and the released reality move together.

Behavior-changing PRs are expected to update the affected page (enforced
separately); QA tests product behavior against these pages and flags
product-vs-doc divergence (separate work). This directory establishes the doc
set and its convention; those wiring behaviors land via the per-agent skill
issues.

## Accuracy rule

Content must reflect **current shipped reality**, spot-checked against the
running product — not aspirational or planned behavior. Anything the author
cannot confirm against a running instance is **omitted or explicitly marked
`_Unverified_`**, never asserted. Bringing up a real instance to spot-check is
what the one-command dev/test environment (`scripts/devenv/`, kyber#399) is for.

## Convention

One page per major capability area. To add a page, copy
[`_TEMPLATE.md`](_TEMPLATE.md) (sections: **Concept / Observable behavior /
States / Operator actions**) and add it to the index below. New areas slot in
without re-litigating structure.

## Index

| Page | Capability area |
|---|---|
| [`overview.md`](overview.md) | What Kyber does, from an operator's vantage — the entry point |
| [`agent-lifecycle.md`](agent-lifecycle.md) | The states an agent moves through and what an operator can do at each |
| [`pwa-holocron.md`](pwa-holocron.md) | The PWA (Fleet Command Console) and Holocron multi-cluster surfaces |
| [`telegram.md`](telegram.md) | Telegram channel setup states and observable messaging behavior |

**v1 set** is the three pages above plus this README and the template. Remaining
capability areas — machine / capacity, inbound prompts, authentication, token /
context budget — accrue as follow-up pages using the same convention.
