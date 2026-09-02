# MAT-36 implementation plan: thin A2A HTTP+JSON edge

**Issue:** [MAT-36](https://linear.app/matty-v/issue/MAT-36/a2a-89-implement-thin-a2a-10-httpjson-edge-adapter)

**Design:** [MAT-26 accepted design](../design/2026-08-30-a2a-http-json-edge-adapter.md)

**Origin:** [MAT-6 spike and gap study](../design/2026-08-30-a2a-protocol-support.md)

- [x] Re-verify the pinned A2A 1.0 specification and official Go SDK v2.4.0,
  including currently published security and interoperability issues.
- [x] Add the disabled-by-default installation gate and authenticated per-agent
  A2A route without changing native API behavior.
- [x] Generate deterministic Agent Cards from the curated, currently available
  public capability manifest.
- [x] Translate send, get, list, cancel, and subscribe/stream operations onto
  the native MAT-19 through MAT-25 task services, preserving native ownership,
  authorization, idempotency, state, result, history, and cursor semantics.
- [x] Return explicit A2A errors for unsupported push, extended-card, transport,
  content, and version requests; keep all SDK-owned execution and persistence
  components out of the adapter.
- [x] Add focused protocol, translation, authorization, concurrency, streaming,
  and contract tests plus operator configuration documentation.
- [x] Run repository validation, deploy the worktree to kyber-dev GCP, verify
  the authenticated default-off route gate, review the PR, fix findings, and
  merge only after CI is green. Enabled-path dev exercise follows normal chart
  delivery because the worktree deploy intentionally does not apply Helm values.

Validation completed against immutable dev image
`worktree-20260902110543-dce395c`; the authenticated A2A path returns 404 while
the installation gate remains disabled, as designed. Live enabled-path exercise
remains gated on applying the new Helm flag through the normal dev delivery path.
