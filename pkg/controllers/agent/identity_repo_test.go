package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/githubapp"
)

// fakeMinter is a test double for the githubapp.Client interface. It records
// how many times MintInstallationToken was called and returns the configured
// token (or error).
type fakeMinter struct {
	calls int
	tok   *githubapp.InstallationToken
	err   error
}

func (f *fakeMinter) MintInstallationToken(ctx context.Context) (*githubapp.InstallationToken, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.tok, nil
}

// newIdentityTestAgent returns an Agent with identityRepo configured, suitable
// for exercising the identity-repo reconciler branch.
func newIdentityTestAgent(name string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kyber-system",
		},
		Spec: kyberv1.AgentSpec{
			IdentityRepo: kyberv1.AgentIdentityRepo{
				Repo: "matty-v/chewie-agent",
			},
		},
	}
}

// newIdentityReconciler builds an AgentReconciler backed by a fake client
// with Agent status subresource enabled. The minter is injected by the caller.
func newIdentityReconciler(t *testing.T, minter GithubTokenMinter, agents ...*kyberv1.Agent) (*AgentReconciler, client.Client) {
	t.Helper()
	scheme := buildTestScheme()
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&kyberv1.Agent{})
	for _, a := range agents {
		builder = builder.WithObjects(a)
	}
	c := builder.Build()
	r := &AgentReconciler{
		Client:            c,
		Scheme:            scheme,
		Recorder:          record.NewFakeRecorder(32),
		GithubTokenMinter: minter,
	}
	return r, c
}

func TestReconcileIdentityRepo_NoOpWhenNotConfigured(t *testing.T) {
	ctx := context.Background()
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "nobody", Namespace: "kyber-system"},
		// No IdentityRepo.Repo set.
	}
	minter := &fakeMinter{}
	r, c := newIdentityReconciler(t, minter, agent)

	requeue, err := r.reconcileIdentityRepo(ctx, agent)
	if err != nil {
		t.Fatalf("reconcileIdentityRepo: %v", err)
	}
	if requeue != 0 {
		t.Errorf("requeue: got %v, want 0 when identityRepo is not configured", requeue)
	}
	if minter.calls != 0 {
		t.Errorf("minter calls: got %d, want 0", minter.calls)
	}

	// No Secret should have been created.
	sec := &corev1.Secret{}
	err = c.Get(ctx, types.NamespacedName{Name: "nobody-github", Namespace: "kyber-system"}, sec)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected NotFound, got: %v", err)
	}
}

// TestReconcileIdentityRepo_RejectsMalformedSlug covers the defense-in-depth
// regex check: slugs that would flow into shell-constructed URLs must surface
// as Failed without ever reaching the minter.
func TestReconcileIdentityRepo_RejectsMalformedSlug(t *testing.T) {
	cases := []string{
		"not-a-slug",               // no slash
		"matty-v/chewie; rm -rf /", // shell metacharacters
		"matty-v/chewie`whoami`",   // backtick
		"/leading-slash",           // empty owner
		"trailing-slash/",          // empty name
		"matty-v/chewie agent",     // space
	}
	for _, slug := range cases {
		t.Run(slug, func(t *testing.T) {
			ctx := context.Background()
			agent := newIdentityTestAgent("chewie")
			agent.Spec.IdentityRepo.Repo = slug
			minter := &fakeMinter{
				tok: &githubapp.InstallationToken{Token: "should-not-be-called", ExpiresAt: time.Now().Add(time.Hour)},
			}
			r, c := newIdentityReconciler(t, minter, agent)

			requeue, err := r.reconcileIdentityRepo(ctx, agent)
			if err != nil {
				t.Fatalf("reconcileIdentityRepo: got error %v, want nil (invalid slug should only surface in status)", err)
			}
			if requeue != 0 {
				t.Errorf("requeue: got %v, want 0 (no retry — spec won't fix itself)", requeue)
			}
			if minter.calls != 0 {
				t.Errorf("minter calls: got %d, want 0 (must not mint for invalid slug)", minter.calls)
			}

			got := &kyberv1.Agent{}
			if err := c.Get(ctx, types.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
				t.Fatalf("Get agent: %v", err)
			}
			if got.Status.IdentityRepo.Phase != kyberv1.AgentIdentityRepoPhaseFailed {
				t.Errorf("status.phase: got %q, want Failed", got.Status.IdentityRepo.Phase)
			}
		})
	}
}

// --- kyber#509 — delivery loop removed; configured repo is Ready without minting -

// TestReconcileIdentityRepo_ConfiguredRepoReadyWithoutDelivery is the core
// kyber#509 assertion: a configured identity repo no longer triggers an
// in-platform token mint or a <name>-github Secret delivery. The reconciler
// just records the repo as Ready (git auth rides the generic PAT user-secret,
// installed by start-claude.sh) and schedules no token-refresh requeue.
func TestReconcileIdentityRepo_ConfiguredRepoReadyWithoutDelivery(t *testing.T) {
	ctx := context.Background()
	agent := newIdentityTestAgent("chewie")
	minter := &fakeMinter{
		tok: &githubapp.InstallationToken{Token: "should-not-be-minted", ExpiresAt: time.Now().Add(time.Hour)},
	}
	r, c := newIdentityReconciler(t, minter, agent)

	requeue, err := r.reconcileIdentityRepo(ctx, agent)
	if err != nil {
		t.Fatalf("reconcileIdentityRepo: %v", err)
	}
	if requeue != 0 {
		t.Errorf("requeue: got %v, want 0 (no token to refresh — git auth is the PAT user-secret)", requeue)
	}
	if minter.calls != 0 {
		t.Errorf("minter calls: got %d, want 0 (in-platform git-token mint is removed)", minter.calls)
	}

	// No <name>-github Secret must be delivered.
	sec := &corev1.Secret{}
	err = c.Get(ctx, types.NamespacedName{Name: "chewie-github", Namespace: "kyber-system"}, sec)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no github-identity Secret to be created, got: %v", err)
	}

	// Status should still reflect the configured repo as Ready for observability.
	got := &kyberv1.Agent{}
	if err := c.Get(ctx, types.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get agent: %v", err)
	}
	if got.Status.IdentityRepo.Phase != kyberv1.AgentIdentityRepoPhaseReady {
		t.Errorf("status.phase: got %q, want Ready", got.Status.IdentityRepo.Phase)
	}
	if got.Status.IdentityRepo.Repo != "matty-v/chewie-agent" {
		t.Errorf("status.repo: got %q, want matty-v/chewie-agent", got.Status.IdentityRepo.Repo)
	}
}

// TestReconcileIdentityRepo_ConfiguredRepoReadyWithoutMinter proves the
// decoupling: a configured (non-template) identity repo reconciles cleanly even
// when no GithubTokenMinter is wired, because git auth no longer depends on the
// in-platform App client. (Pre-kyber#509 this surfaced a Failed status.)
func TestReconcileIdentityRepo_ConfiguredRepoReadyWithoutMinter(t *testing.T) {
	ctx := context.Background()
	agent := newIdentityTestAgent("chewie")
	r, c := newIdentityReconciler(t, nil, agent) // nil minter

	requeue, err := r.reconcileIdentityRepo(ctx, agent)
	if err != nil {
		t.Fatalf("reconcileIdentityRepo: got error %v, want nil (git auth no longer needs the minter)", err)
	}
	if requeue != 0 {
		t.Errorf("requeue: got %v, want 0", requeue)
	}

	got := &kyberv1.Agent{}
	if err := c.Get(ctx, types.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get agent: %v", err)
	}
	if got.Status.IdentityRepo.Phase != kyberv1.AgentIdentityRepoPhaseReady {
		t.Errorf("status.phase: got %q, want Ready", got.Status.IdentityRepo.Phase)
	}
}

// --- Auto-create (Phase 3) tests ---

// fakeScaffolder is a test double for RepoScaffolder.
type fakeScaffolder struct {
	calls    int
	fullName string // returned on success
	err      error
}

func (f *fakeScaffolder) CreateFromTemplate(
	ctx context.Context,
	installationToken string,
	templateOwner, templateRepo string,
	newOwner, newRepoName string,
	params githubapp.ScaffoldParams,
) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if f.fullName != "" {
		return f.fullName, nil
	}
	return newOwner + "/" + newRepoName, nil
}

// newTemplateTestAgent returns an Agent with identityRepo.template set but
// identityRepo.repo empty — simulating an auto-create request.
func newTemplateTestAgent(name string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kyber-system",
		},
		Spec: kyberv1.AgentSpec{
			IdentityRepo: kyberv1.AgentIdentityRepo{
				Template: "matty-v/kyber-agent-template",
				// Repo is intentionally empty — triggers auto-create
			},
			Identity: kyberv1.AgentIdentity{
				SoulDescription: "A test agent",
			},
		},
	}
}

// newScaffoldReconciler builds an AgentReconciler with both a minter and a
// scaffolder injected, for testing the auto-create dispatch path.
func newScaffoldReconciler(
	t *testing.T,
	minter GithubTokenMinter,
	scaffolder RepoScaffolder,
	repoOwner string,
	agents ...*kyberv1.Agent,
) (*AgentReconciler, client.Client) {
	t.Helper()
	scheme := buildTestScheme()
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&kyberv1.Agent{})
	for _, a := range agents {
		builder = builder.WithObjects(a)
	}
	c := builder.Build()
	r := &AgentReconciler{
		Client:            c,
		Scheme:            scheme,
		Recorder:          record.NewFakeRecorder(32),
		GithubTokenMinter: minter,
		Scaffolder:        scaffolder,
		IdentityRepoOwner: repoOwner,
	}
	return r, c
}

// TestReconcileIdentityRepo_AutoCreate_HappyPath verifies that when
// spec.identityRepo.template is set and spec.identityRepo.repo is empty:
//   - The scaffolder is called once (minting one token for the scaffold call)
//   - spec.identityRepo.repo is patched with the result
//   - No <name>-github Secret is delivered (kyber#509 removed that path) and the
//     configured repo is marked Ready with no token-refresh requeue.
func TestReconcileIdentityRepo_AutoCreate_HappyPath(t *testing.T) {
	ctx := context.Background()
	agent := newTemplateTestAgent("threepio")

	expires := time.Now().Add(60 * time.Minute)
	minter := &fakeMinter{
		tok: &githubapp.InstallationToken{
			Token:     "ghs_scaffoldtoken",
			ExpiresAt: expires,
		},
	}
	scaffolder := &fakeScaffolder{fullName: "matty-v/threepio-agent"}
	r, c := newScaffoldReconciler(t, minter, scaffolder, "matty-v", agent)

	requeue, err := r.reconcileIdentityRepo(ctx, agent)
	if err != nil {
		t.Fatalf("reconcileIdentityRepo: %v", err)
	}
	if scaffolder.calls != 1 {
		t.Errorf("scaffolder calls: got %d, want 1", scaffolder.calls)
	}

	// spec.identityRepo.repo must have been patched.
	got := &kyberv1.Agent{}
	if err := c.Get(ctx, types.NamespacedName{Name: "threepio", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get agent: %v", err)
	}
	if got.Spec.IdentityRepo.Repo != "matty-v/threepio-agent" {
		t.Errorf("spec.identityRepo.repo: got %q, want matty-v/threepio-agent", got.Spec.IdentityRepo.Repo)
	}

	// The minter is called exactly once — for the scaffold API call. There is
	// no longer a second mint for a delivered git token (kyber#509).
	if minter.calls != 1 {
		t.Errorf("minter calls: got %d, want 1 (scaffold only — git-token delivery removed)", minter.calls)
	}

	// No <name>-github Secret must be delivered.
	sec := &corev1.Secret{}
	err = c.Get(ctx, types.NamespacedName{Name: "threepio-github", Namespace: "kyber-system"}, sec)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no github-identity Secret after scaffold, got: %v", err)
	}

	// Configured repo recorded Ready; no token-refresh requeue.
	if got.Status.IdentityRepo.Phase != kyberv1.AgentIdentityRepoPhaseReady {
		t.Errorf("status.phase: got %q, want Ready", got.Status.IdentityRepo.Phase)
	}
	if requeue != 0 {
		t.Errorf("requeue: got %v, want 0 (no token to refresh)", requeue)
	}
}

// TestReconcileIdentityRepo_AutoCreate_ScaffolderError verifies that a
// scaffolder failure surfaces in status as Failed and the token-mint path
// does NOT run (no Secret created).
func TestReconcileIdentityRepo_AutoCreate_ScaffolderError(t *testing.T) {
	ctx := context.Background()
	agent := newTemplateTestAgent("r2d2")

	minter := &fakeMinter{
		tok: &githubapp.InstallationToken{
			Token:     "ghs_minttoken",
			ExpiresAt: time.Now().Add(60 * time.Minute),
		},
	}
	scaffolder := &fakeScaffolder{err: errors.New("GitHub 503 on generate")}
	r, c := newScaffoldReconciler(t, minter, scaffolder, "matty-v", agent)

	_, err := r.reconcileIdentityRepo(ctx, agent)
	if err == nil {
		t.Fatal("expected error from scaffolder failure, got nil")
	}

	// Status should be Failed.
	got := &kyberv1.Agent{}
	if err2 := c.Get(ctx, types.NamespacedName{Name: "r2d2", Namespace: "kyber-system"}, got); err2 != nil {
		t.Fatalf("Get agent: %v", err2)
	}
	if got.Status.IdentityRepo.Phase != kyberv1.AgentIdentityRepoPhaseFailed {
		t.Errorf("status.phase: got %q, want Failed", got.Status.IdentityRepo.Phase)
	}

	// spec.identityRepo.repo must still be empty (scaffold didn't complete).
	if got.Spec.IdentityRepo.Repo != "" {
		t.Errorf("spec.identityRepo.repo: got %q, want empty", got.Spec.IdentityRepo.Repo)
	}

	// Secret must not have been created.
	sec := &corev1.Secret{}
	err2 := c.Get(ctx, types.NamespacedName{Name: "r2d2-github", Namespace: "kyber-system"}, sec)
	if !apierrors.IsNotFound(err2) {
		t.Errorf("expected Secret to not exist, got: %v", err2)
	}
}

// TestReconcileIdentityRepo_AutoCreate_NilScaffolder verifies that when
// Scaffolder is nil, the status surfaces a clear failure message.
func TestReconcileIdentityRepo_AutoCreate_NilScaffolder(t *testing.T) {
	ctx := context.Background()
	agent := newTemplateTestAgent("bb8")
	minter := &fakeMinter{
		tok: &githubapp.InstallationToken{
			Token:     "ghs_minttoken",
			ExpiresAt: time.Now().Add(60 * time.Minute),
		},
	}
	// Scaffolder is nil but minter is set.
	r, c := newScaffoldReconciler(t, minter, nil, "matty-v", agent)

	_, err := r.reconcileIdentityRepo(ctx, agent)
	if err == nil {
		t.Fatal("expected error when Scaffolder is nil, got nil")
	}

	got := &kyberv1.Agent{}
	if err2 := c.Get(ctx, types.NamespacedName{Name: "bb8", Namespace: "kyber-system"}, got); err2 != nil {
		t.Fatalf("Get agent: %v", err2)
	}
	if got.Status.IdentityRepo.Phase != kyberv1.AgentIdentityRepoPhaseFailed {
		t.Errorf("status.phase: got %q, want Failed", got.Status.IdentityRepo.Phase)
	}
	if got.Status.IdentityRepo.Message == "" {
		t.Error("status.message: expected non-empty error message")
	}
}

// TestReconcileIdentityRepo_AutoCreate_NoOwner verifies that when
// IdentityRepoOwner is empty, the status surfaces a clear failure.
func TestReconcileIdentityRepo_AutoCreate_NoOwner(t *testing.T) {
	ctx := context.Background()
	agent := newTemplateTestAgent("lando")
	minter := &fakeMinter{
		tok: &githubapp.InstallationToken{
			Token:     "ghs_minttoken",
			ExpiresAt: time.Now().Add(60 * time.Minute),
		},
	}
	scaffolder := &fakeScaffolder{}
	// IdentityRepoOwner intentionally empty.
	r, c := newScaffoldReconciler(t, minter, scaffolder, "", agent)

	_, err := r.reconcileIdentityRepo(ctx, agent)
	if err == nil {
		t.Fatal("expected error when IdentityRepoOwner is empty, got nil")
	}
	if scaffolder.calls != 0 {
		t.Errorf("scaffolder calls: got %d, want 0 (should fail before calling scaffolder)", scaffolder.calls)
	}

	got := &kyberv1.Agent{}
	if err2 := c.Get(ctx, types.NamespacedName{Name: "lando", Namespace: "kyber-system"}, got); err2 != nil {
		t.Fatalf("Get agent: %v", err2)
	}
	if got.Status.IdentityRepo.Phase != kyberv1.AgentIdentityRepoPhaseFailed {
		t.Errorf("status.phase: got %q, want Failed", got.Status.IdentityRepo.Phase)
	}
}

// TestReconcileIdentityRepo_AutoCreate_InvalidTemplateSlug verifies that a
// malformed template slug surfaces a Failed status without calling the scaffolder.
func TestReconcileIdentityRepo_AutoCreate_InvalidTemplateSlug(t *testing.T) {
	ctx := context.Background()
	agent := newTemplateTestAgent("han")
	agent.Spec.IdentityRepo.Template = "not-a-slug" // missing "/"

	minter := &fakeMinter{
		tok: &githubapp.InstallationToken{
			Token:     "ghs_minttoken",
			ExpiresAt: time.Now().Add(60 * time.Minute),
		},
	}
	scaffolder := &fakeScaffolder{}
	r, c := newScaffoldReconciler(t, minter, scaffolder, "matty-v", agent)

	requeue, err := r.reconcileIdentityRepo(ctx, agent)
	if err != nil {
		// Invalid slug → no error returned (no retry), only status update.
		t.Fatalf("reconcileIdentityRepo: got error %v, want nil", err)
	}
	if requeue != 0 {
		t.Errorf("requeue: got %v, want 0 (no retry for bad slug)", requeue)
	}
	if scaffolder.calls != 0 {
		t.Errorf("scaffolder calls: got %d, want 0", scaffolder.calls)
	}

	got := &kyberv1.Agent{}
	if err2 := c.Get(ctx, types.NamespacedName{Name: "han", Namespace: "kyber-system"}, got); err2 != nil {
		t.Fatalf("Get agent: %v", err2)
	}
	if got.Status.IdentityRepo.Phase != kyberv1.AgentIdentityRepoPhaseFailed {
		t.Errorf("status.phase: got %q, want Failed", got.Status.IdentityRepo.Phase)
	}
}
