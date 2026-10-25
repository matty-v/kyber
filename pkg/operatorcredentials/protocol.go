package operatorcredentials

// RuntimeAuthorityProtocolVersion is the complete control-plane adoption and
// management protocol advertised by published charts. Zero means activation is
// unavailable. Persistence/schema support alone must never raise this value:
// ownership, audited reconciliation, current auth and supported upgrade/rollback
// integration must all be ready before the runtime advertises protocol one.
const RuntimeAuthorityProtocolVersion = 1
