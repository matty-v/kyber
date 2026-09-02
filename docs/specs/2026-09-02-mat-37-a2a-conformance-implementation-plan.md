# MAT-37 implementation plan: evidence-backed A2A conformance gate

**Issue:** [MAT-37](https://linear.app/matty-v/issue/MAT-37/a2a-99-implement-evidence-backed-conformance-gate)

**Design:** [MAT-27 accepted design](../design/2026-08-30-a2a-conformance-release-gate.md)

**Adapter:** [MAT-26 accepted design](../design/2026-08-30-a2a-http-json-edge-adapter.md)

- [x] Re-verify the exact specification, Go SDK, official TCK, and independent
  Python SDK inputs and record immutable digests.
- [x] Check in and validate the complete A2A 1.0 applicability ledger,
  deviation/patch policy, adapter-affecting path manifest, and generated
  support matrix.
- [ ] Add the always-on static/pure gate and the adapter-affecting deployed gate
  with fail-safe change detection and pinned workflow dependencies.
- [ ] Run the pinned official HTTP+JSON TCK through its narrow authenticated
  patch and add independent-client plus Kyber security/lifecycle coverage.
- [ ] Produce and validate a redacted immutable evidence bundle, checksums, and
  tested/self-attested release claim boundary.
- [ ] Run repository validation, deploy to kyber-dev where applicable, review
  findings, and merge only after required CI is green.

Current next action: close the pinned TCK findings without weakening the
applicability ledger, then activate the adapter-affecting workflow.
