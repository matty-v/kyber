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
- [ ] Add focused protocol, translation, authorization, concurrency, streaming,
  and contract tests plus operator configuration documentation.
- [ ] Run repository validation, deploy the worktree to kyber-dev GCP, exercise
  live authenticated A2A flows, review the PR, fix findings, and merge only
  after CI is green.

Current next action: run full repository validation, address review findings,
then deploy and exercise the authenticated edge in kyber-dev GCP.
