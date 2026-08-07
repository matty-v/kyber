<!--
  Per-subsystem deep-dive template for docs/architecture/.
  Copy this file to docs/architecture/<subsystem>.md and fill in every section.
  Delete this comment and any section guidance you don't need.

  Scope reminder: this set documents the HOW (architecture) — components,
  control/data flow, contracts, invariants, failure modes. NOT the WHAT
  (product capabilities/behaviors) — that lives in docs/product/ (kyber#397),
  and NOT line-level code detail. See README.md for the boundary.
-->

# <Subsystem name> — <one-line what-it-is>

> One or two sentences orienting the reader and stating when to read this page
> (e.g. "Read this before adding a new runtime" / "before changing a phase").

## 1. Purpose & scope

What this subsystem is and what it is responsible for, in one paragraph. State
explicitly what is *out* of scope for this page (and where that lives instead).

## 2. Components & responsibilities

The modules / files that make up the subsystem and what each one owns.

| Component | File(s) | Responsibility |
|---|---|---|
| <name> | `pkg/...` | <what it owns> |

## 3. Control / data flow

A diagram (mermaid or ASCII) plus a short narration of the path through the
subsystem. Show the happy path; call out the branch points.

```mermaid
%% replace with the real flow
flowchart LR
    A --> B --> C
```

## 4. Key invariants & cross-component contracts

What must **always** hold for this subsystem to be correct, and what other
subsystems rely on from it. These are the things a future change must not break
silently. State each as a checkable assertion.

- **<invariant>** — <why it holds / what enforces it>.
- **<contract>** — <who depends on it>.

## 5. Failure modes

What breaks, and how the system responds. One row per failure of interest.

| Failure | Detected by | System response |
|---|---|---|
| <failure> | <signal> | <response> |

## 6. Source of truth

The code files that are **authoritative** for this subsystem. This doc tracks
them; on any conflict, the code wins and the doc is stale.

- [`pkg/.../<file>.go`](../../pkg/.../<file>.go) — <what it defines>

## 7. Cross-references

- Sibling deep-dives: [`<other>.md`](<other>.md)
- Product / WHAT mirror (if any): `docs/product/<page>.md` (kyber#397)
- Related ADRs: [`../adr/`](../adr/)
