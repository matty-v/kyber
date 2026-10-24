# Required audit coverage for lifecycle and archives

Before an authorized lifecycle/delete or archive operation can change state or
release disk contents, persist its attributable authorization event. If history
is unavailable, return 503 before effects. Keep authorization independent of the
old observe-only lifecycle logger and preserve current exact Agent/resource
checks and restricted archive grants. Do not require unrelated terminal scopes.

Extend the existing required-audit wrapper rather than adding a second ledger.
Record actor identity/credential generation and authority, method, typed action,
Agent/Machine/installation target, request/operation IDs, and a separate outcome.
Never record request bodies, model/provider keys, archive tokens/URLs, filenames,
commands, import payloads or free-form backend errors.

Cover Agent and Machine create/delete, desired phase, model/runtime/resources/disk changes and
session/repair actions; treat capability-only edits separately from lifecycle.
Cover export start/cancel/delete, download-link issuance, public downloads,
archive uploads/import start/cancel/delete. Register the actual permission and
resource predicate for ongoing access; installation-wide archive grants may be
restricted keys and must not accidentally become administrator-only.

Read/validate non-secret metadata first where needed. Admission must precede the
first mutation or content read. Mark an attempt unknown before its first remote
write and successful only after confirmed acknowledgement; preserve uncertainty
on interruption/partial writes. Accepted asynchronous lifecycle/archive intent
is distinct from its later controller/job success. Invalid requests before an
attempt remain failed, not committed.

The internal import-to-create call shares trusted server context. Reuse its
verified import admission only for the same target, rather than overwrite it or
force an unrelated lifecycle grant; unexpected nested admission fails closed.
Public download admission uses its already verified narrow capability resolver,
not a general API bearer key. Persist exact denial targets without URL data.

Verification: admission outage causes no effects; successful writes have paired
records; backend commits followed by lost acknowledgement remain unknown; known
commits preserve success on response loss; import nesting retains attribution;
restricted archive upload/download remains valid; live revocation still closes
streams. Exercise the full API suite with working audit fixtures, meaningful
store/HTTP failure cases, contract/vet/race checks, and GCP dev verification.
Keys missing stable attribution must receive a specific identification error;
never invent a human identity or change existing task ownership. Shared test
fixtures supply an available audit store; dedicated outage cases keep it absent.

Credential CRUD/adoption outbox reconciliation and usage projection remain their
own following slices, using the same required admission/outcome conventions.

## Review findings addressed

- Handler deadline extensions are serialized with cancellation and clamped after
  changed access/authority failure. Both an already cancelled reset and an
  in-flight healthy deadline update have regressions; a late timeout must not
  revive an upload/download after cancellation.
- Import-to-create reuses only a private, matching actor and target admission;
  nested Agent creation cannot replace its parent's outcome or acquire another
  grant requirement.
- Known archive validation/capacity refusals remain failures; ambiguous remote
  writes retain unknown outcomes even if the request context itself stays alive.
- Verified obsolete archive capabilities may provide issuer attribution only to
  denial records. Invalid links remain anonymous and authority outages never
  become false permission denials. No capability claims enter caller context.
- History labels describe completed requests; asynchronous controller/job work
  retains its own outcome and progress.
