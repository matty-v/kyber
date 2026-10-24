# Live operator stream access

The request's original permission snapshot must not keep a terminal, log read or
task stream open after revocation, rotation, expiry or a relevant grant change.
This slice adds a watcher inside authentication for protected requests. It does
not activate registry adoption or change Agent specifications, disks or lifecycle intent.

Every three seconds the watcher authenticates the original request again with a
two-second deadline, requires the same principal/tenant/credential/generation,
and checks current route access. Sensitive handlers register exact action and
Agent/Machine target requirements. Task event streams register their read scope
and Agent target independently of their twenty-second heartbeat and check
cancellation while replaying pages. A rename or unrelated key rotation is not a
reason to close an authorized stream.

A failed authority read cancels access without pretending that the key is wrong
or definitively revoked. Known access changes cancel separately. Writers stop
accepting new bytes; read/write deadlines interrupt stalled uploads and network
backpressure. Hijacked sockets close independently after a short grace period.
Terminal handlers attempt policy-change (1008) or temporary-unavailability
(1013) close frames. Writes have a three-second stall bound; normal completion
refreshes the deadline for net/http's final buffered bytes. The target is closure
within ten seconds, using the supported direct authority reader and server
transport. Closing a terminal cannot undo commands it already ran.

Sensitive denials retain the original attributable operation and exact target;
audit collection never delays cancellation. An unavailable authority does not
produce a false denial. Completion collection preserves explicitly confirmed
effects even if the response is lost, and otherwise distinguishes failed access
from unknown authorization. A mutation with no confirmed acknowledgement remains
unknown after cancellation, since the remote write may have committed. Task replay captures its cursor signing key once;
live checks enforce current access without two cluster API reads per event.

Public archive download links are a separate capability surface and remain a
follow-up, as do remaining adoption/upgrade gates, complete mutation auditing,
management UI/API and usage projections. This change does not claim to erase data
already received by a client or activate production registry authentication.
