# Verify loaded credential access before adoption

This private read-only gate is not runtime activation. The supervised adoption
worker must discover every serving/terminating control-plane replica, pin its
release/image ownership and Kubernetes identity, and recheck that complete set
before effects. A terminating or unready replica blocks this probe; wait for
verified quiescence rather than excluding a possible old writer by age.

For each pinned ready Pod, read its actual Pod IP directly, never a load-balanced
Service. Disable environment proxies, redirects and connection reuse; bound each
request and the whole probe. Verify the current endpoint reports the expected
Pod UID, namespace, configuration authority and exact current identity,
provenance, generation, role, scopes and target resources for every distinct
source value. Use the existing authenticator's actual final-definition/root
precedence. Do not synthesize missing identities during this comparison.

A metadata inventory alone cannot detect two keys whose identity mappings were
swapped. Individual material probes catch that; the administrator inventory must
also match every expected accepted/shadowed definition, so an extra accepted
value or loaded definition blocks the handoff. Usage/configuration observations
are not credential authority and are excluded from this equality comparison.

Recheck the complete frozen source graph before and after the network reads.
All failures are static and contain no request/header/body/transport details.
Values remain private and are sent only as the existing Bearer credential to the
verified replica. No value, material hash or response body is persisted.

This check is an observation, not cross-resource atomicity or a write permit.
It does not establish source ownership, discover replicas, validate published
compatibility, stop old upgrade Jobs or serialize rotation. The complete worker
must enforce those gates, durable admission, maintenance fencing and explicit
administrator ownership confirmation before creating or selecting a registry.
Runtime protocol stays zero until that complete flow passes acceptance.

An investigator in another GKE cluster may lack a route to dev Pod IPs. The
opt-in integration test can use an authenticated Kubernetes port-forward pinned
to the exact single dev Pod UID; its destination guard still checks that Pod IP.
This proves the real replica's material-to-access mapping, not the production
transport's in-cluster reachability. There is no runtime Service/proxy fallback.
