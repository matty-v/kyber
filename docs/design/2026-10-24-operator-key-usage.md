# Observe operator key use

Status: implementation design for the approved MAT-97/MAT-134 scope. Key
management and adoption remain separate from activity collection.

## Operator outcome

Admin Settings shows each key's last observed accepted request, authenticated
request count, denied HTTP request count, authentication transport and request
types. Counts describe observed API activity during a stated collection window.
They do not claim individual people behind shared credentials, lifetime totals,
terminal commands or model/provider usage. Imported creation time stays unknown.
A key with no observation says so for that window; it is never labelled unused
throughout its lifetime. Required security history remains a separate durable
record of sensitive access and effects.

## Collection and durability

Authentication records an immutable, value-free observation after a credential
is accepted. A bounded, non-blocking collector batches metadata locally. HTTP
401/403 after successful authentication contributes a denied-request observation.
Authentication failures before a current key can be established remain
installation-level counts; invalid material is never fingerprinted or assigned
to a guessed credential. Request type labels come from a fixed resource-free
catalog, never raw paths, query strings, bodies or client-supplied labels.

Normal authentication has no PostgreSQL dependency. A full queue, restart,
missing reporter or database outage produces explicit partial coverage and
freshness information. Browser sessions and bearer keys use the same stable
credential identity. Long-lived connections contribute their initial request,
with continuing sensitive actions represented by the existing security history.

Each process has a fresh observer UUID and monotonic batch sequence. PostgreSQL
commits aggregate deltas and the observer cursor together. Replaying an identical
acknowledgement-lost batch is idempotent; a different payload or skipped sequence
fails. No raw credentials, cookies, IP addresses or per-request paths are stored.
Only the last bounded batch payload is retained for replay comparison. Observer
cursor rows remain durable to prevent old first-batch replay after pruning.
Aggregate counters are returned as decimal strings, preserving integer precision.

## API and UI

Administrator-only usage reads return current metadata and collection coverage.
An unavailable store returns unavailable, not an empty successful result. Current
administrator access is rechecked before output. Metadata stays bound to its
connection and authenticated-key context, with stale/denied reads clearing the
protected view. UI polling and its API requests are genuine observed activity.
Collection is explicitly partial across historical gaps and unsupported entry
points; coverage descriptions identify the implemented entry points.

## Review and acceptance

Self-review requires durable replay tests, bounded-queue loss reporting, no
blocking database calls on authentication, no secret/raw-path persistence,
current-admin output checks, no integer truncation, HTTP stream/WebSocket
compatibility and truthful unavailable/partial UI. Real GCP dev testing uses
owned registry/schema probes, preserving live keys and zero Agents. Desktop and
mobile screenshots show the final presentation. Production rollout/credential
activation remain separate operations.
