// Default template repo for identity-repo auto-create mode (V1 hard-coded).
// Operators who fork Kyber can update this constant — no server-side config needed.
export const DEFAULT_IDENTITY_TEMPLATE = 'matty-v/kyber-agent-template'

// Valid "owner/repo" slug: lowercase owner, mixed-case repo allowed by GitHub.
// Each segment must start with an alphanumeric (no leading-dash names like
// "-foo/bar" or "foo/-bar", which GitHub rejects).
export const IDENTITY_REPO_SLUG_RE = /^[a-z0-9][a-z0-9-]*\/[a-zA-Z0-9_.][a-zA-Z0-9_.-]*$/
