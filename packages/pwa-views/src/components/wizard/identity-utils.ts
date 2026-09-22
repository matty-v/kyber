import type { ComputeConfig, IdentityRepoMode } from '../../lib/types'

// Default template repo for identity-repo auto-create mode (V1 hard-coded).
// Operators who fork Kyber can update this constant — no server-side config needed.
export const DEFAULT_IDENTITY_TEMPLATE = 'matty-v/kyber-agent-template'

// Valid "owner/repo" slug: lowercase owner, mixed-case repo allowed by GitHub.
// Each segment must start with an alphanumeric (no leading-dash names like
// "-foo/bar" or "foo/-bar", which GitHub rejects).
export const IDENTITY_REPO_SLUG_RE = /^[a-z0-9][a-z0-9-]*\/[a-zA-Z0-9_.][a-zA-Z0-9_.-]*$/

// What the control plane can do with identity repos, read from
// GET /api/v1/config. The server validates creates against the same
// capability, so the wizard must never offer a mode missing from here.
export interface IdentityRepoCapability {
  // False while config is loading or failed to load. Nothing but 'none' is
  // usable until it is true.
  loaded: boolean
  supportedModes: IdentityRepoMode[]
  // Why the GitHub-backed modes are unavailable, when they are.
  unavailableReason: string
}

const FALLBACK_UNAVAILABLE_REASON =
  'GitHub identity repositories need the Kyber GitHub App to be configured on this installation.'

export function identityRepoCapability(config: ComputeConfig | undefined): IdentityRepoCapability {
  if (!config) {
    return { loaded: false, supportedModes: ['none'], unavailableReason: '' }
  }
  // A control plane older than this field never reports it, and cannot say
  // whether its GitHub App works. Offer only what cannot fail.
  const modes = config.identity?.supportedModes ?? []
  const supportedModes: IdentityRepoMode[] = modes.includes('none') ? modes : [...modes, 'none']
  return {
    loaded: true,
    supportedModes,
    unavailableReason: config.identity?.unavailableReason || FALLBACK_UNAVAILABLE_REASON,
  }
}

// The identity choice a new agent starts with: a new repo from the template
// when the installation can create one, otherwise none.
export function defaultIdentityRepoMode(capability: IdentityRepoCapability): IdentityRepoMode {
  return capability.supportedModes.includes('template') ? 'template' : 'none'
}
