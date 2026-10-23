# Operator security history persistence

This package is the durable storage foundation for MAT-97. It is used by the
sensitive operator handlers and bounded retention worker.
Settings history and complete operator audit coverage remain subsequent work.
The credential authority field distinguishes configuration-reported identities
and generations from a future authoritative registry. Do not interpret current
shared-key/configuration generations as a complete key-material rotation ledger.

Events contain typed identity/action/resource metadata. Callers must generate
an immutable event ID and preserve the complete event when retrying a failed
append. Reusing an ID with different metadata fails. Authorization decisions
and completed effects have distinct phases/outcomes; an operation can remain
uncertain after a crash or an external effect followed by a storage failure.

PostgreSQL stores both observation and database-recorded timestamps. Reads use
a commit-ordered sequence snapshot so late/backdated events cannot appear in
later pages of an existing query. Pagination is newest recorded first, not a
claim that clocks on all replicas agree. Future API cursors must bind the
namespace, filters, snapshot and caller authorization; the internal Cursor is
not a public access token. Retention can remove rows while a query is paged.
Pruning uses recorded time and a bounded per-namespace batch. The runtime
policy also caps row count; its minute sweeps can temporarily lag a traffic
burst. Queries must disclose actual retained coverage.

Database failures are errors, never an empty successful history or a successful
append. Backend errors are redacted; context cancellation/deadlines remain
recognizable. A future API must authorize queries and persist required events
before high-risk effects or sensitive bytes. It must not expose key material,
request bodies, commands, raw URLs or backend error strings in events.

Run `go test ./pkg/securityaudit`. To verify actual PostgreSQL behavior, set
`KYBER_PG_DSN` privately and run
`go test -tags integration -count=1 ./pkg/securityaudit`. Tests create and drop
only their own random schemas; their database role needs that capability.
The CI integration workflow runs these tests against its disposable database.
Do not run the general `test/integration` suite on a live installation: other
suites deliberately truncate shared application tables.
