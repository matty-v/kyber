# Review shared changes across every harness

Before implementing shared Agent behavior, enumerate the production runtime
registry (`pkg/runtimes`, including built-in registration and each descriptor).
Do not infer the inventory from the agent that reproduced a bug. The current
inventory includes Claude Code, Codex and Hermes; the registry is authoritative
when another harness is added.

Put a coverage matrix in the PR description. Include every registered harness,
even when the conclusion is that its native client does not offer the feature.
For each row record:

- The native control or behavior, installed version and relevant model/provider
  routes. Use current native metadata or primary upstream documentation/source.
- The adapter, launch and resume paths, API/controller validation, capability
  reporting, shared UI and runtime images affected by the change.
- What was tested, the result, and any action or evidence still required.

An unexamined harness is a gap, not an unsupported feature. Missing credentials,
an unavailable test environment, stale metadata and a missing Kyber adapter
implementation do not establish that the upstream harness lacks a capability.
If native support exists, include its integration and tests in the shared feature
or explicitly agree a narrower scope with the operator before claiming delivery.
If support is unavailable, give the native reason and evidence; never silently
accept an ignored setting or invent a mapping between providers' controls.

For configuration changes, cover default/reset behavior, explicit and moving
version selections such as `latest`, supported and unsupported models/routes,
authentication recovery, real application of the value, restart/resume behavior
and preservation of agent data. Preserve the same operator intent across
harnesses while using each adapter's native semantics. Test desktop/mobile and
both embedded Kyber and Holocron when shared controls change. Verify the relevant
runtime images and independently published UI packages actually shipped.

Before merge, revisit the matrix against the final diff and runtime inventory.
Add registry-enumerating regression coverage where shared code branches by
harness so a newly registered runtime cannot silently fall through. Do not mark
work complete while a required row is untested or blocked; report the exact
remaining action. Follow the normal review, CI, deployment and cleanup rules.
