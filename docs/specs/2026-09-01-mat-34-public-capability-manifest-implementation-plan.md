# MAT-34 implementation plan: curated public capability manifests

**Issue:** [MAT-34](https://linear.app/matty-v/issue/MAT-34/a2a-69-implement-curated-public-capability-manifests)  
**Design:** [MAT-24 accepted design](../design/2026-08-30-public-agent-capability-manifest.md)  
**Origin:** [MAT-6 spike and gap study](../design/2026-08-30-a2a-protocol-support.md)

- [x] Add the empty-by-default `Agent.spec.publicCapabilities` declaration and
  bounded `Agent.status.publicCapabilities` availability model.
- [x] Validate and normalize the v1alpha1 public allowlist, stable IDs, MIME
  modes, task features, URLs, text, private evidence, limits, and digest.
- [x] Reconcile Claude Code/Codex adapter support, task gates, skill freshness
  and health, configured connectors, platform features, lifecycle, drift, and
  recovery without ever promoting observations into declarations.
- [x] Add `capabilities:read`/`capabilities:write`, exact agent-resource checks,
  audit records, privacy contract tests, native GET with ETag, and PATCH
  publish/update/unpublish behavior.
- [x] Add the structured PWA editor, private evidence suggestions, exact public
  preview, drift visibility, and explicit publication controls.
- [x] Validate the generated CRD, API, controller, PWA, OpenAPI contract, dev
  deployment, and live Claude Code/Codex observation paths before merge.

Validation evidence: focused Go/API/controller/contract suites and all 755 PWA
tests passed; the full controller package reached its existing 10-minute local
envtest timeout after executing hundreds of tests, while the MAT-34-focused
controller tests passed. Dev image `worktree-20260902014623-33e8998` proved
empty-by-default 404, available publication, private-evidence omission, ETag
304, fail-closed required-skill drift, and clean unpublication on the live
Claude Code agent. Unit coverage exercises the same matrix for Codex.
