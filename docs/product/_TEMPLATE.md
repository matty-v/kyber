<!--
  Per-capability page template for docs/product/ (kyber#397).
  Copy this file to docs/product/<capability>.md and fill each section.
  Describe WHAT an operator observes and can do — never HOW it is built.
  No file paths, no function names, no controller internals: that is the
  architecture (HOW) set, docs/architecture/. If you find yourself naming a
  package, a source file, or an internal component, you are in the wrong doc.
-->

# <Capability area> — product behavior

> **Verification status:** <state how this page was confirmed against current
> shipped reality — e.g. "spot-checked against a running instance on
> YYYY-MM-DD" or "grounded in shipped behavior; items not confirmable against a
> running instance are marked _Unverified_ below">. Maintained by Yoda; see
> [README](README.md).

## Concept

What this capability *is*, in product terms — the one-paragraph answer to "what
does Kyber give an operator here, and why would they care." No mechanism.

## Observable behavior

What an operator actually sees happen. Describe it from the outside: the surface
they look at, what appears there, and what changes when the system acts. If a
behavior cannot be confirmed against a running instance, either omit it or mark
it `> _Unverified — pending spot-check (#399)_` rather than asserting it.

## States

The meaningful states a thing can be in and what each *means to an operator* —
named (using the same vocabulary the product surfaces), but not implemented.
Where the architecture (HOW) set owns the authoritative state list, name the
states here and link there for the deep-dive instead of re-deriving them.

| State | What it means to an operator | What (if anything) they should do |
|---|---|---|
| `<State>` | <plain-language meaning> | <action, or "nothing — informational"> |

## Operator actions

What an operator can *do* in this area, and the outcome they should expect.
Action → observable result. No API routes, no commands beyond what the operator
literally invokes in the product surface.

| Action | Where | Expected result |
|---|---|---|
| <action> | <surface, e.g. "Agents tab"> | <what the operator then observes> |

## See also

- The HOW for this capability: [`../architecture/`](../architecture/overview.md)
- Related product pages: <links>
