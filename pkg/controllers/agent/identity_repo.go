package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/githubapp"
)

// identityRepoSlugRegex matches owner/name slugs that GitHub actually
// accepts. Intentionally strict: it flows into shell-constructed URLs in
// start-claude.sh, so a malformed value must surface as a controller error
// rather than as unpredictable pod behavior.
var identityRepoSlugRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

const (
	// IdentityRepoEnvVar names the env var set on the agent container when
	// spec.identityRepo.repo is configured. start-claude.sh branches on this
	// to clone the repo; git auth itself rides the generic PAT user-secret
	// ($GH_TOKEN / $USER_GITHUB_TOKEN), not an in-platform-delivered token
	// (kyber#509 — the per-agent <name>-github Secret delivery loop was removed).
	IdentityRepoEnvVar = "KYBER_IDENTITY_REPO"

	// githubTokenErrorRetry is how long to wait before retrying after a
	// scaffolding failure. Short enough to recover quickly; long enough to
	// avoid hammering GitHub during an outage.
	githubTokenErrorRetry = 1 * time.Minute
)

// GithubTokenMinter is the controller's view of pkg/githubapp.Client. Defined
// as an interface so tests can inject a fake; the real implementation is
// always *githubapp.Client.
type GithubTokenMinter interface {
	MintInstallationToken(ctx context.Context) (*githubapp.InstallationToken, error)
}

// RepoScaffolder creates a new private GitHub repo from a template and
// substitutes agent-specific placeholders in all text files. Defined as an
// interface so tests can inject a fake without standing up a real GitHub API.
// The real implementation delegates to pkg/githubapp.Client.CreateFromTemplate.
type RepoScaffolder interface {
	// CreateFromTemplate creates a private repo from templateOwner/templateRepo
	// under newOwner/newRepoName and substitutes {{ .AgentName }} and
	// {{ .Description }} in all text files. Returns the "owner/repo" full name.
	// Idempotent: safe to call multiple times (re-applies substitutions if the
	// repo already exists from a previous partial attempt).
	CreateFromTemplate(
		ctx context.Context,
		installationToken string,
		templateOwner, templateRepo string,
		newOwner, newRepoName string,
		params githubapp.ScaffoldParams,
	) (fullName string, err error)
}

// reconcileIdentityRepo dispatches identity-repo scaffolding (when a template is
// set but the repo is empty) and records the configured repo's status. Returns
// the duration after which the reconciler should re-run (0 means "nothing to
// reschedule on this account").
//
// Git authentication for the identity repo is NOT handled here. As of kyber#509
// (Stage 2 of the decouple-#508 cutover) the in-platform per-agent git-token
// mint + <name>-github Secret delivery loop was removed: the agent pod
// authenticates git with the generic PAT user-secret ($GH_TOKEN /
// $USER_GITHUB_TOKEN), which start-claude.sh installs as the git credential
// helper. The GitHub App client is retained only for scaffolding (and the
// GitHub API routes), retired later in #508 Stage 3/4.
//
// Errors are wrapped and returned — the caller should log but not fail the
// whole reconcile over them, since identity-repo failures shouldn't block the
// agent's pod lifecycle state machine.
func (r *AgentReconciler) reconcileIdentityRepo(ctx context.Context, agent *kyberv1.Agent) (time.Duration, error) {
	// Auto-create dispatch: if a template is set but Repo is still empty,
	// scaffold the repo and patch spec.identityRepo.repo with the result. On
	// success the guard (Repo == "") ensures we never re-scaffold.
	if agent.Spec.IdentityRepo.Template != "" && agent.Spec.IdentityRepo.Repo == "" {
		if err := r.scaffoldIdentityRepo(ctx, agent); err != nil {
			return githubTokenErrorRetry, err
		}
		// scaffoldIdentityRepo patched spec.identityRepo.repo; fall through to
		// record the configured repo's status.
	}

	if agent.Spec.IdentityRepo.Repo == "" {
		// Not configured — nothing to do.
		return 0, nil
	}

	if !identityRepoSlugRegex.MatchString(agent.Spec.IdentityRepo.Repo) {
		// Reject early — the slug is echoed into shell-constructed URLs in
		// start-claude.sh. Surface this in status so operators see the
		// problem in `kubectl describe` without tailing pod logs.
		msg := fmt.Sprintf("spec.identityRepo.repo %q is not a valid owner/name slug", agent.Spec.IdentityRepo.Repo)
		if err := r.setIdentityRepoStatus(ctx, agent, kyberv1.AgentIdentityRepoPhaseFailed, msg, nil, nil); err != nil {
			return 0, err
		}
		// No periodic retry: the slug won't fix itself, and the controller
		// will re-run when the spec changes.
		return 0, nil
	}

	// Configured and valid. Git auth rides the generic PAT user-secret in the
	// pod (no in-platform token to mint, deliver, or refresh). Record Ready so
	// the status surface stays accurate; schedule no token-refresh requeue.
	if err := r.setIdentityRepoStatus(ctx, agent, kyberv1.AgentIdentityRepoPhaseReady, "", nil, nil); err != nil {
		return 0, err
	}
	return 0, nil
}

// scaffoldIdentityRepo creates a new GitHub repo from spec.identityRepo.template
// and patches spec.identityRepo.repo with the resulting "owner/repo" slug.
// On success the caller's guard (Repo == "") prevents re-scaffolding. On
// failure, status is set to Failed with a descriptive message so operators can
// diagnose without reading controller logs.
func (r *AgentReconciler) scaffoldIdentityRepo(ctx context.Context, agent *kyberv1.Agent) error {
	logger := log.FromContext(ctx)

	if r.Scaffolder == nil {
		msg := "identity-repo auto-create requires GitHub App client — check kyber-github-app Secret"
		if err := r.setIdentityRepoStatus(ctx, agent, kyberv1.AgentIdentityRepoPhaseFailed, msg, nil, nil); err != nil {
			return err
		}
		return errors.New("identityRepo.template set but Scaffolder is nil")
	}

	if r.IdentityRepoOwner == "" {
		msg := "identity-repo auto-create requires KYBER_IDENTITY_REPO_OWNER to be set"
		if err := r.setIdentityRepoStatus(ctx, agent, kyberv1.AgentIdentityRepoPhaseFailed, msg, nil, nil); err != nil {
			return err
		}
		return errors.New("identityRepo.template set but IdentityRepoOwner is empty")
	}

	// Parse "owner/repo" from spec.identityRepo.template.
	templateSlug := agent.Spec.IdentityRepo.Template
	parts := strings.SplitN(templateSlug, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		msg := fmt.Sprintf("spec.identityRepo.template %q is not a valid owner/repo slug", templateSlug)
		if err := r.setIdentityRepoStatus(ctx, agent, kyberv1.AgentIdentityRepoPhaseFailed, msg, nil, nil); err != nil {
			return err
		}
		return nil // don't retry — spec won't fix itself
	}
	templateOwner, templateRepo := parts[0], parts[1]

	// The new repo is named "<agent-name>-agent" under the configured owner.
	newRepoName := agent.Name + "-agent"

	// Mark as Pending while scaffolding is in flight.
	if err := r.setIdentityRepoStatus(ctx, agent, kyberv1.AgentIdentityRepoPhasePending,
		"scaffolding identity repo from template", nil, nil); err != nil {
		return err
	}

	// We need a fresh installation token to call the GitHub API. Mint one with
	// a short timeout (scaffolding may take a few seconds + polling).
	if r.GithubTokenMinter == nil {
		msg := "identity-repo auto-create requires GitHub App client — check kyber-github-app Secret"
		if err := r.setIdentityRepoStatus(ctx, agent, kyberv1.AgentIdentityRepoPhaseFailed, msg, nil, nil); err != nil {
			return err
		}
		return errors.New("identityRepo.template set but GithubTokenMinter is nil")
	}
	mintCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tok, err := r.GithubTokenMinter.MintInstallationToken(mintCtx)
	if err != nil {
		msg := "mint installation token for scaffold: " + err.Error()
		_ = r.setIdentityRepoStatus(ctx, agent, kyberv1.AgentIdentityRepoPhaseFailed, msg, nil, nil)
		return fmt.Errorf("minting token for scaffolding: %w", err)
	}

	scaffoldCtx, cancel2 := context.WithTimeout(ctx, 60*time.Second)
	defer cancel2()

	description := agent.Spec.Identity.SoulDescription
	fullName, err := r.Scaffolder.CreateFromTemplate(
		scaffoldCtx,
		tok.Token,
		templateOwner, templateRepo,
		r.IdentityRepoOwner, newRepoName,
		githubapp.ScaffoldParams{
			AgentName:   agent.Name,
			Description: description,
		},
	)
	if err != nil {
		msg := "scaffold from template: " + err.Error()
		_ = r.setIdentityRepoStatus(ctx, agent, kyberv1.AgentIdentityRepoPhaseFailed, msg, nil, nil)
		return fmt.Errorf("scaffolding identity repo: %w", err)
	}

	logger.Info("scaffolded identity repo from template",
		"agent", agent.Name,
		"template", templateSlug,
		"repo", fullName)

	// Patch spec.identityRepo.repo with the new repo slug. Once this write
	// lands, the Repo != "" guard above prevents re-scaffolding on the next
	// reconcile, even if this reconcile fails after this point.
	specPatch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.IdentityRepo.Repo = fullName
	if err := r.Patch(ctx, agent, specPatch); err != nil {
		return fmt.Errorf("patching spec.identityRepo.repo after scaffold: %w", err)
	}

	return nil
}

// setIdentityRepoStatus patches agent.Status.IdentityRepo. Pass nil for
// expiresAt/lastMinted on failure paths to leave them unchanged from the
// previous successful mint (operators can still read the last-known-good
// values).
func (r *AgentReconciler) setIdentityRepoStatus(
	ctx context.Context,
	agent *kyberv1.Agent,
	phase kyberv1.AgentIdentityRepoPhase,
	message string,
	expiresAt *metav1.Time,
	lastMinted *metav1.Time,
) error {
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.IdentityRepo.Phase = phase
	agent.Status.IdentityRepo.Repo = agent.Spec.IdentityRepo.Repo
	agent.Status.IdentityRepo.Message = message
	if expiresAt != nil {
		agent.Status.IdentityRepo.TokenExpiresAt = expiresAt
	}
	if lastMinted != nil {
		agent.Status.IdentityRepo.LastMinted = lastMinted
	}
	if err := r.Status().Patch(ctx, agent, patch); err != nil {
		return fmt.Errorf("patching identityRepo status: %w", err)
	}
	return nil
}
