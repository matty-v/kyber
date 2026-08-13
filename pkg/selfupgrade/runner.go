// Package selfupgrade implements the mechanics of Kyber upgrading itself:
// pull the target chart, apply its CRDs, run the Helm upgrade, verify what
// came back, and roll back if it did not.
//
// Two decisions shape everything in here.
//
// **It runs in a Job, not in the control-plane process.** The control plane is
// the thing being replaced; a process cannot reliably supervise its own
// termination, and an upgrade that dies halfway because its supervisor was
// rolled is the worst possible outcome. A Job survives the restart and its log
// is the upgrade record. The Job runs the CURRENT control-plane image with a
// different entrypoint — same image, so there is nothing extra to build,
// publish, or pin.
//
// **Every failure path rolls back.** Helm's own `--atomic` is deliberately not
// used: it would give us two rollback code paths (Helm's, for an upgrade that
// fails, and ours, for an upgrade that succeeds but produces a cluster that
// does not work) with different logging and different timeouts. One path,
// always ours, always logged with the reason.
//
// See dave-agent spec 2026-08-10-kyber-owns-its-deployment.md §4.
package selfupgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultUpgradeTimeout bounds `helm upgrade --wait`. Generous: a control-plane
// rollout on a small box pulls a new image over a home connection.
const DefaultUpgradeTimeout = 15 * time.Minute

// DefaultVerifyTimeout bounds the post-upgrade health check, which starts only
// after Helm already reported every workload ready.
const DefaultVerifyTimeout = 5 * time.Minute

// chartName is the directory `helm pull --untar` creates, and the chart Kyber
// upgrades itself with.
const chartName = "kyber"

// Config is everything the upgrade needs to know. All fields except the
// timeouts are required.
type Config struct {
	// Release and Namespace identify the Helm release to upgrade. This must
	// already BE a Helm release — see Run's first step.
	Release   string
	Namespace string

	// ChartRef is the OCI repository holding the chart, without a version:
	// oci://ghcr.io/matty-v/charts/kyber
	ChartRef string

	// TargetVersion is the exact chart version to move to. Never a range,
	// never a floating tag: the operator's stated intent is a version.
	TargetVersion string

	// HelmBin is the helm executable. Shipped in the control-plane image.
	HelmBin string

	// WorkDir is scratch space for the pulled chart and the captured values.
	// The Job mounts an emptyDir here because its root filesystem is read-only.
	WorkDir string

	// UpgradeTimeout bounds `helm upgrade --wait`; VerifyTimeout bounds the
	// health check that follows. Zero uses the defaults above.
	UpgradeTimeout time.Duration
	VerifyTimeout  time.Duration

	// DryRun renders the upgrade and runs every precondition without changing
	// anything: no CRDs applied, no release written, no verification. It is
	// how an operator finds out that their values pin image tags BEFORE they
	// press the button.
	DryRun bool
}

func (c Config) upgradeTimeout() time.Duration {
	if c.UpgradeTimeout > 0 {
		return c.UpgradeTimeout
	}
	return DefaultUpgradeTimeout
}

func (c Config) verifyTimeout() time.Duration {
	if c.VerifyTimeout > 0 {
		return c.VerifyTimeout
	}
	return DefaultVerifyTimeout
}

// Validate rejects a configuration that cannot produce a correct upgrade.
func (c Config) Validate() error {
	var missing []string
	for _, f := range []struct {
		name  string
		value string
	}{
		{"release", c.Release},
		{"namespace", c.Namespace},
		{"chart reference", c.ChartRef},
		{"target version", c.TargetVersion},
		{"helm binary", c.HelmBin},
		{"work directory", c.WorkDir},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("incomplete upgrade configuration: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// Commander runs external commands. An interface so the step sequence can be
// tested without a cluster or a helm binary — the ordering of pull, CRD apply,
// upgrade and rollback is the part most worth pinning down in a test.
type Commander interface {
	// Output runs a command and returns its stdout. Used where the result is
	// data we parse.
	Output(ctx context.Context, name string, args ...string) (string, error)
	// Stream runs a command, forwarding stdout and stderr to the Job log as
	// they arrive. Used where the operator wants to watch it happen.
	Stream(ctx context.Context, name string, args ...string) error
}

// Verifier answers "did the cluster actually come back as the version we
// asked for?" after Helm reports success.
type Verifier interface {
	Verify(ctx context.Context, targetVersion string) error
}

// Runner executes one upgrade.
type Runner struct {
	Cfg Config
	Cmd Commander
	K8s client.Client
	Ver Verifier
	Log *slog.Logger
	Now func() time.Time

	// ApplyCRDsFn overrides how CRDs are applied. Nil uses ApplyCRDs against
	// K8s, which is what the Job does. The seam exists because the fake
	// client's server-side-apply support does not match a real API server's,
	// and a test that passes against a fake apply would be asserting the
	// fake's behaviour rather than ours (see
	// reference_kyber_silent_failure_wrong_cause_class).
	ApplyCRDsFn func(ctx context.Context, crds []*unstructured.Unstructured) ([]string, error)

	crds string // overridden in tests; defaults to <workdir>/kyber/crds
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// releaseStatus is the subset of `helm status -o json` we read.
type releaseStatus struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Info    struct {
		Status string `json:"status"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Version    string `json:"version"`
			AppVersion string `json:"appVersion"`
		} `json:"metadata"`
	} `json:"chart"`
}

// Run performs the upgrade. It returns nil only when the new version is
// installed AND verified.
//
// On any failure after the release has been written, it rolls back to the
// revision that was live when Run started and returns an error describing both
// the original failure and the rollback outcome. A rollback that itself fails
// is reported loudly — that is the state a human has to look at.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.Cfg.Validate(); err != nil {
		return err
	}
	log := r.log()
	started := r.now()

	log.Info("self-upgrade starting",
		"release", r.Cfg.Release,
		"namespace", r.Cfg.Namespace,
		"chart", r.Cfg.ChartRef,
		"targetVersion", r.Cfg.TargetVersion,
		"dryRun", r.Cfg.DryRun)

	// 1. Establish that this IS a Helm release. On an ArgoCD-managed cluster
	// there is no release Secret at all, and `helm upgrade --install` would
	// cheerfully CREATE one over the top of resources ArgoCD owns — two
	// managers fighting over the same objects. That is why this uses `upgrade`
	// and not `upgrade --install`, and why a missing release is fatal here.
	current, err := r.currentRelease(ctx)
	if err != nil {
		return err
	}
	log.Info("current release",
		"revision", current.Version,
		"chartVersion", current.Chart.Metadata.Version,
		"appVersion", current.Chart.Metadata.AppVersion,
		"status", current.Info.Status)

	// 2. Capture the operator's values explicitly. NOT `--reuse-values`:
	// that flag carries values forward invisibly, so what a release is
	// configured with stops being knowable from anything you can read. Writing
	// them to a file and passing -f means the Job log and the file together
	// say exactly what was applied.
	valuesPath, valuesYAML, err := r.captureValues(ctx)
	if err != nil {
		return err
	}

	// 3. Refuse an upgrade that cannot change what runs.
	pinned, err := PinnedImagePaths(valuesYAML)
	if err != nil {
		return fmt.Errorf("inspect release values: %w", err)
	}
	if len(pinned) > 0 {
		return PinnedImageError(pinned)
	}

	// 4. Pull the target chart. Doing this before touching anything means a
	// bad version, an unreachable registry or a missing artifact fails while
	// the cluster is still untouched.
	chartDir, err := r.pullChart(ctx)
	if err != nil {
		return err
	}

	// 5. CRDs, before the upgrade, fail-closed. Helm will not do this.
	if err := r.applyCRDs(ctx, chartDir); err != nil {
		return err
	}

	// 6. The upgrade itself.
	if err := r.upgrade(ctx, chartDir, valuesPath); err != nil {
		if r.Cfg.DryRun {
			return err
		}
		return r.rollback(ctx, current.Version, err)
	}
	if r.Cfg.DryRun {
		log.Info("dry run complete: no CRDs applied, no release written", "elapsed", r.now().Sub(started).String())
		return nil
	}

	// 7. Verify against the running cluster, not against Helm's opinion of it.
	// "deployed" is a statement about the release record; it has been true of
	// clusters serving a stale build before.
	if err := r.verify(ctx); err != nil {
		return r.rollback(ctx, current.Version, err)
	}

	log.Info("self-upgrade complete",
		"targetVersion", r.Cfg.TargetVersion,
		"fromRevision", current.Version,
		"elapsed", r.now().Sub(started).String())
	return nil
}

func (r *Runner) currentRelease(ctx context.Context) (releaseStatus, error) {
	var st releaseStatus
	out, err := r.Cmd.Output(ctx, r.Cfg.HelmBin,
		"status", r.Cfg.Release, "--namespace", r.Cfg.Namespace, "--output", "json")
	if err != nil {
		return st, fmt.Errorf(
			"cannot read Helm release %q in namespace %q: %w. "+
				"Kyber only upgrades itself where it is a real Helm release — if this cluster was deployed by "+
				"ArgoCD or by applying rendered manifests, adopt it into a release first",
			r.Cfg.Release, r.Cfg.Namespace, err)
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return st, fmt.Errorf("parse helm status output: %w", err)
	}
	if st.Version <= 0 {
		return st, fmt.Errorf("helm status reported revision %d for release %q, which is not a revision we can roll back to",
			st.Version, r.Cfg.Release)
	}
	return st, nil
}

// captureValues writes the release's user-supplied values to a file and
// returns the path and the YAML.
func (r *Runner) captureValues(ctx context.Context) (string, string, error) {
	out, err := r.Cmd.Output(ctx, r.Cfg.HelmBin,
		"get", "values", r.Cfg.Release, "--namespace", r.Cfg.Namespace, "--output", "yaml")
	if err != nil {
		return "", "", fmt.Errorf("read current release values: %w", err)
	}
	// A release installed with no overrides prints "null".
	if strings.TrimSpace(out) == "null" {
		out = ""
	}
	path := filepath.Join(r.Cfg.WorkDir, "values.yaml")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return "", "", fmt.Errorf("write captured values to %s: %w", path, err)
	}
	r.log().Info("captured release values", "path", path, "bytes", len(out))
	return path, out, nil
}

func (r *Runner) pullChart(ctx context.Context) (string, error) {
	ref := strings.TrimSuffix(r.Cfg.ChartRef, "/")
	if err := r.Cmd.Stream(ctx, r.Cfg.HelmBin,
		"pull", ref,
		"--version", r.Cfg.TargetVersion,
		"--untar",
		"--untardir", r.Cfg.WorkDir,
	); err != nil {
		return "", fmt.Errorf("pull chart %s version %s: %w", ref, r.Cfg.TargetVersion, err)
	}
	dir := filepath.Join(r.Cfg.WorkDir, chartName)
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("chart %s version %s did not unpack to %s: %w",
			ref, r.Cfg.TargetVersion, dir, err)
	}
	r.log().Info("pulled chart", "version", r.Cfg.TargetVersion, "path", dir)
	return dir, nil
}

func (r *Runner) crdDir(chartDir string) string {
	if r.crds != "" {
		return r.crds
	}
	return filepath.Join(chartDir, "crds")
}

func (r *Runner) applyCRDs(ctx context.Context, chartDir string) error {
	dir := r.crdDir(chartDir)
	crds, err := LoadCRDs(dir)
	if err != nil {
		return fmt.Errorf("load CRDs from the target chart: %w", err)
	}
	if len(crds) == 0 {
		// A Kyber chart without CRDs is not a Kyber chart. Treat it as a bad
		// artifact rather than assuming the cluster's existing CRDs are fine.
		return fmt.Errorf("target chart %s contains no CRDs in %s; refusing to upgrade against an unknown chart layout",
			r.Cfg.TargetVersion, dir)
	}
	if r.Cfg.DryRun {
		names := make([]string, 0, len(crds))
		for _, c := range crds {
			names = append(names, c.GetName())
		}
		r.log().Info("dry run: would apply CRDs", "crds", names)
		return nil
	}
	apply := r.ApplyCRDsFn
	if apply == nil {
		if r.K8s == nil {
			return errors.New("no Kubernetes client configured; cannot apply CRDs, and Helm will not apply them either")
		}
		apply = func(ctx context.Context, crds []*unstructured.Unstructured) ([]string, error) {
			return ApplyCRDs(ctx, r.K8s, crds)
		}
	}
	applied, err := apply(ctx, crds)
	if err != nil {
		return fmt.Errorf("apply CRDs before upgrading (nothing else has been changed): %w", err)
	}
	r.log().Info("applied CRDs", "crds", applied)
	return nil
}

func (r *Runner) upgrade(ctx context.Context, chartDir, valuesPath string) error {
	args := []string{
		"upgrade", r.Cfg.Release, chartDir,
		"--namespace", r.Cfg.Namespace,
		"--values", valuesPath,
		"--wait",
		"--timeout", r.Cfg.upgradeTimeout().String(),
	}
	if r.Cfg.DryRun {
		args = append(args, "--dry-run")
	}
	if err := r.Cmd.Stream(ctx, r.Cfg.HelmBin, args...); err != nil {
		return fmt.Errorf("helm upgrade to %s failed: %w", r.Cfg.TargetVersion, err)
	}
	return nil
}

func (r *Runner) verify(ctx context.Context) error {
	if r.Ver == nil {
		return errors.New("no verifier configured; refusing to call an unverified upgrade successful")
	}
	vctx, cancel := context.WithTimeout(ctx, r.Cfg.verifyTimeout())
	defer cancel()
	if err := r.Ver.Verify(vctx, r.Cfg.TargetVersion); err != nil {
		return fmt.Errorf("upgrade installed but the cluster did not come back healthy on %s: %w",
			r.Cfg.TargetVersion, err)
	}
	r.log().Info("verified", "targetVersion", r.Cfg.TargetVersion)
	return nil
}

// rollback returns to the revision that was live before this run.
//
// The context is deliberately detached from the caller's: if the Job is being
// torn down or the upgrade timed out, that same expiry must not also kill the
// rollback and leave the cluster on the half-applied release.
func (r *Runner) rollback(ctx context.Context, revision int, cause error) error {
	log := r.log()
	log.Error("upgrade failed; rolling back", "toRevision", revision, "error", cause.Error())

	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.Cfg.upgradeTimeout())
	defer cancel()

	err := r.Cmd.Stream(rbCtx, r.Cfg.HelmBin,
		"rollback", r.Cfg.Release, fmt.Sprint(revision),
		"--namespace", r.Cfg.Namespace,
		"--wait",
		"--timeout", r.Cfg.upgradeTimeout().String(),
	)
	if err != nil {
		log.Error("ROLLBACK FAILED — this cluster needs a human",
			"release", r.Cfg.Release, "toRevision", revision, "error", err.Error())
		return fmt.Errorf("upgrade failed (%w) AND the rollback to revision %d also failed: %v — "+
			"the release is in an indeterminate state; run `helm history %s -n %s` and roll back by hand",
			cause, revision, err, r.Cfg.Release, r.Cfg.Namespace)
	}
	log.Info("rolled back", "toRevision", revision)
	return fmt.Errorf("upgrade failed and was rolled back to revision %d: %w", revision, cause)
}
