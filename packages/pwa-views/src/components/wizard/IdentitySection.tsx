import { useEffect, useMemo, useState } from 'react'
import { useComputeConfig, useGitHubRepoExists, useGitHubRepos } from '../../hooks/useAPI'
import { DEFAULT_IDENTITY_TEMPLATE, IDENTITY_REPO_SLUG_RE } from './identity-utils'
import { inputClass, labelClass } from './styles'
import type { IdentityRepoMode, WizardSetter, WizardState } from './types'

export interface IdentitySectionProps {
  state: WizardState
  set: WizardSetter
}

// Sentinel value the dropdown uses to mean "I want to type a repo that
// isn't in the list" — needed because <select> can't carry a typed-text
// state directly.
const FREEFORM = '__freeform__'

export function IdentitySection({ state, set }: IdentitySectionProps) {
  const config = useComputeConfig()
  const repoOwner = config.data?.identity?.repoOwner ?? ''

  // Lazy-load the repo list — it only matters in 'existing' mode, but
  // we also use the templates count to gate a future enhancement; for
  // now fetch only when needed.
  const reposQuery = useGitHubRepos(state.identityRepoMode === 'existing')

  // Whether the user has clicked "use a different repo" — toggles the
  // existing-mode UI from dropdown to free-text input. Sticky for the
  // lifetime of the component so a stale fetch error doesn't kick the
  // user back into the picker mid-typing.
  const [freeform, setFreeform] = useState(false)

  const showDropdown =
    state.identityRepoMode === 'existing' &&
    !freeform &&
    reposQuery.isSuccess &&
    reposQuery.data.repos.length > 0

  const showFreeformInput =
    state.identityRepoMode === 'existing' && (freeform || !showDropdown)

  return (
    <section className="space-y-5">
      <div>
        <label htmlFor="agent-identity-mode" className={labelClass}>
          Identity Repo
        </label>
        <select
          id="agent-identity-mode"
          value={state.identityRepoMode}
          onChange={(e) =>
            set('identityRepoMode', e.target.value as IdentityRepoMode)
          }
          className={inputClass}
        >
          <option value="template">Create new from template</option>
          <option value="existing">Link existing repo</option>
          <option value="none">None</option>
        </select>
      </div>

      {state.identityRepoMode === 'template' && (
        <TemplateModeBadge state={state} repoOwner={repoOwner} set={set} />
      )}

      {showDropdown && (
        <ExistingRepoDropdown
          state={state}
          set={set}
          repos={reposQuery.data.repos.map((r) => r.fullName)}
          onPickFreeform={() => setFreeform(true)}
        />
      )}

      {showFreeformInput && (
        <ExistingRepoFreeform
          state={state}
          set={set}
          loading={reposQuery.isLoading}
          error={reposQuery.error}
          canSwitchBack={reposQuery.isSuccess && reposQuery.data.repos.length > 0}
          onSwitchBack={() => setFreeform(false)}
        />
      )}
    </section>
  )
}

interface TemplateModeBadgeProps {
  state: WizardState
  repoOwner: string
  set: WizardSetter
}

function TemplateModeBadge({ state, repoOwner, set }: TemplateModeBadgeProps) {
  // The full target repo name the controller would create.
  const targetName = state.name ? `${state.name}-agent` : ''
  const targetFullName =
    targetName && repoOwner ? `${repoOwner}/${targetName}` : ''

  const exists = useGitHubRepoExists(repoOwner, targetName)

  // Mirror the collision result into wizard state so the step gate can
  // see it. Cleared whenever the conditions to call /exists aren't met.
  useEffect(() => {
    const colliding = exists.isSuccess && exists.data.exists === true
    if (state.identityRepoCollision !== colliding) {
      set('identityRepoCollision', colliding)
    }
  }, [exists.isSuccess, exists.data?.exists, state.identityRepoCollision, set])

  return (
    <div>
      <p className="text-xs text-text-muted">
        Will create{' '}
        <code>{targetFullName || `${repoOwner || '<owner>'}/<agent-name>-agent`}</code>{' '}
        from <code>{DEFAULT_IDENTITY_TEMPLATE}</code>.
      </p>
      <CollisionBadge
        owner={repoOwner}
        name={targetName}
        loading={exists.isFetching}
        ready={exists.isSuccess}
        exists={exists.data?.exists}
      />
    </div>
  )
}

interface CollisionBadgeProps {
  owner: string
  name: string
  loading: boolean
  ready: boolean
  exists: boolean | undefined
}

function CollisionBadge({ owner, name, loading, ready, exists }: CollisionBadgeProps) {
  // Don't render anything until we have both an owner and a name to check.
  if (!owner || !name) return null
  if (loading) {
    return (
      <p className="mt-1.5 text-xs text-text-muted" data-testid="identity-collision-loading">
        Checking availability…
      </p>
    )
  }
  if (!ready) return null
  if (exists) {
    return (
      <p
        className="mt-1.5 text-xs text-danger"
        role="status"
        data-testid="identity-collision-taken"
      >
        <span aria-hidden="true">✕</span> A repository named <code>{name}</code>{' '}
        already exists under <code>{owner}</code>. Pick a different agent name.
      </p>
    )
  }
  return (
    <p
      className="mt-1.5 text-xs text-success"
      role="status"
      data-testid="identity-collision-available"
    >
      <span aria-hidden="true">✓</span> Available — no collision under <code>{owner}</code>.
    </p>
  )
}

interface ExistingRepoDropdownProps {
  state: WizardState
  set: WizardSetter
  repos: string[]
  onPickFreeform: () => void
}

function ExistingRepoDropdown({
  state,
  set,
  repos,
  onPickFreeform,
}: ExistingRepoDropdownProps) {
  // The dropdown's value is either one of the App-installed repos, or the
  // FREEFORM sentinel when the user wants to type one not in the list.
  // We compute the displayed value from state.identityRepoExisting so the
  // dropdown stays consistent if the user switches step away and back.
  const dropdownValue = useMemo(() => {
    if (state.identityRepoExisting && repos.includes(state.identityRepoExisting)) {
      return state.identityRepoExisting
    }
    return ''
  }, [state.identityRepoExisting, repos])

  return (
    <div>
      <label htmlFor="agent-identity-repo" className={labelClass}>
        Repository
      </label>
      <select
        id="agent-identity-repo"
        value={dropdownValue}
        onChange={(e) => {
          const v = e.target.value
          if (v === FREEFORM) {
            onPickFreeform()
            return
          }
          set('identityRepoExisting', v)
        }}
        className={inputClass}
      >
        <option value="" disabled>
          Pick a repository…
        </option>
        {repos.map((full) => (
          <option key={full} value={full}>
            {full}
          </option>
        ))}
        <option value={FREEFORM}>Other (type repo name)…</option>
      </select>
    </div>
  )
}

interface ExistingRepoFreeformProps {
  state: WizardState
  set: WizardSetter
  loading: boolean
  error: unknown
  canSwitchBack: boolean
  onSwitchBack: () => void
}

function ExistingRepoFreeform({
  state,
  set,
  loading,
  error,
  canSwitchBack,
  onSwitchBack,
}: ExistingRepoFreeformProps) {
  return (
    <div>
      <label htmlFor="agent-identity-repo" className={labelClass}>
        Repository
      </label>
      <input
        id="agent-identity-repo"
        type="text"
        required
        value={state.identityRepoExisting}
        onChange={(e) => set('identityRepoExisting', e.target.value)}
        placeholder="owner/repo"
        className={inputClass}
      />
      {state.identityRepoExisting &&
        !IDENTITY_REPO_SLUG_RE.test(state.identityRepoExisting) && (
          <p className="mt-1.5 text-xs text-danger">
            Format: <code>owner/repo</code> (lowercase owner; mixed-case repo allowed).
          </p>
        )}
      {loading && (
        <p className="mt-1.5 text-xs text-text-muted">Loading available repos…</p>
      )}
      {!loading && error != null && (
        <p className="mt-1.5 text-xs text-text-muted">
          Couldn't load installed repositories — type the repo name above.
        </p>
      )}
      {canSwitchBack && (
        <button
          type="button"
          className="mt-1.5 text-xs text-accent underline"
          onClick={onSwitchBack}
        >
          Back to the list
        </button>
      )}
    </div>
  )
}
