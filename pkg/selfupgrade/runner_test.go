package selfupgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fakeCommander records every command and lets a test fail a chosen helm
// subcommand. Recording the ORDER is the point: the sequence (status → get
// values → pull → upgrade → rollback) is the safety property, not any single
// call.
type fakeCommander struct {
	calls []string

	// values is what `helm get values` returns.
	values string
	// status is what `helm status -o json` returns.
	status string
	// failOn fails the first command whose joined args contain this string.
	failOn string
	// failRollback fails the rollback too, simulating the worst case.
	failRollback bool
	// onPull runs when the pull command is issued, so a test can materialise
	// the chart directory helm would have unpacked.
	onPull func() error
}

func (f *fakeCommander) record(args []string) string {
	joined := strings.Join(args, " ")
	f.calls = append(f.calls, joined)
	return joined
}

func (f *fakeCommander) Output(_ context.Context, _ string, args ...string) (string, error) {
	joined := f.record(args)
	if f.failOn != "" && strings.Contains(joined, f.failOn) {
		return "", errors.New("boom")
	}
	switch {
	case strings.HasPrefix(joined, "status"):
		if f.status != "" {
			return f.status, nil
		}
		return `{"name":"kyber","version":4,"info":{"status":"deployed"},"chart":{"metadata":{"version":"1.0.1","appVersion":"1.0.1"}}}`, nil
	case strings.HasPrefix(joined, "get values"):
		return f.values, nil
	}
	return "", nil
}

func (f *fakeCommander) Stream(_ context.Context, _ string, args ...string) error {
	joined := f.record(args)
	if strings.HasPrefix(joined, "rollback") {
		if f.failRollback {
			return errors.New("rollback boom")
		}
		return nil
	}
	if f.failOn != "" && strings.Contains(joined, f.failOn) {
		return errors.New("boom")
	}
	if strings.HasPrefix(joined, "pull") && f.onPull != nil {
		return f.onPull()
	}
	return nil
}

func (f *fakeCommander) ran(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

type fakeVerifier struct {
	err    error
	called bool
}

func (v *fakeVerifier) Verify(context.Context, string) error {
	v.called = true
	return v.err
}

const testCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: agents.kyber.io
spec:
  group: kyber.io
`

// newRunner wires a Runner over a temp work dir, with the chart directory
// materialised when helm pull runs.
func newRunner(t *testing.T, cmd *fakeCommander, ver Verifier) *Runner {
	t.Helper()
	work := t.TempDir()
	chartDir := filepath.Join(work, chartName)
	crdDir := filepath.Join(chartDir, "crds")

	if cmd.onPull == nil {
		cmd.onPull = func() error {
			if err := os.MkdirAll(crdDir, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(crdDir, "agents.yaml"), []byte(testCRD), 0o644)
		}
	}

	return &Runner{
		Cfg: Config{
			Release:       "kyber-canary",
			Namespace:     "kyber-system",
			ChartRef:      "oci://ghcr.io/matty-v/charts/kyber",
			TargetVersion: "1.0.2",
			HelmBin:       "helm",
			WorkDir:       work,
		},
		Cmd: cmd,
		Ver: ver,
		ApplyCRDsFn: func(_ context.Context, crds []*unstructured.Unstructured) ([]string, error) {
			names := make([]string, 0, len(crds))
			for _, c := range crds {
				names = append(names, c.GetName())
			}
			return names, nil
		},
	}
}

func TestRun_HappyPath_OrdersStepsAndVerifies(t *testing.T) {
	cmd := &fakeCommander{}
	ver := &fakeVerifier{}
	r := newRunner(t, cmd, ver)

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if !ver.called {
		t.Error("verifier was never called; an unverified upgrade must not be reported as successful")
	}

	want := []string{"status", "get values", "pull", "upgrade"}
	var idx []int
	for _, w := range want {
		found := -1
		for i, c := range cmd.calls {
			if strings.HasPrefix(c, w) {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("command %q never ran; got %v", w, cmd.calls)
		}
		idx = append(idx, found)
	}
	for i := 1; i < len(idx); i++ {
		if idx[i] < idx[i-1] {
			t.Errorf("commands out of order: %q ran before %q (%v)", want[i], want[i-1], cmd.calls)
		}
	}
	if cmd.ran("rollback") {
		t.Error("rolled back on a successful upgrade")
	}
}

// The guard that matters most: values pinning image tags mean the upgrade
// cannot change what runs, so it must refuse BEFORE pulling or applying
// anything.
func TestRun_RefusesWhenValuesPinImageTags(t *testing.T) {
	cmd := &fakeCommander{values: "image:\n  controlPlane:\n    tag: v1.0.1\n"}
	r := newRunner(t, cmd, &fakeVerifier{})

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run() = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "image.controlPlane.tag") {
		t.Errorf("error must name the offending path, got: %v", err)
	}
	if cmd.ran("pull") || cmd.ran("upgrade") {
		t.Errorf("refused too late — it already touched the cluster: %v", cmd.calls)
	}
}

func TestRun_EmptyTagIsNotAPin(t *testing.T) {
	cmd := &fakeCommander{values: "image:\n  controlPlane:\n    tag: \"\"\n"}
	r := newRunner(t, cmd, &fakeVerifier{})

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil (an empty tag means 'take the chart default')", err)
	}
}

// No Helm release means ArgoCD or raw manifests own these resources. `helm
// upgrade --install` would create a competing owner, so this must stop.
func TestRun_RefusesWhenNotAHelmRelease(t *testing.T) {
	cmd := &fakeCommander{failOn: "status"}
	r := newRunner(t, cmd, &fakeVerifier{})

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run() = nil, want an error when the release cannot be read")
	}
	if !strings.Contains(err.Error(), "adopt it into a release") {
		t.Errorf("error should tell the operator what to do, got: %v", err)
	}
	if cmd.ran("upgrade") {
		t.Error("upgraded despite not being able to read the release")
	}
}

func TestRun_FailedUpgradeRollsBackToTheStartingRevision(t *testing.T) {
	cmd := &fakeCommander{failOn: "upgrade"}
	ver := &fakeVerifier{}
	r := newRunner(t, cmd, ver)

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run() = nil, want the upgrade failure")
	}
	if !cmd.ran("rollback kyber-canary 4") {
		t.Errorf("did not roll back to revision 4 (the revision live at start): %v", cmd.calls)
	}
	if ver.called {
		t.Error("verified after a failed upgrade")
	}
	if !strings.Contains(err.Error(), "rolled back to revision 4") {
		t.Errorf("error should say it rolled back, got: %v", err)
	}
}

// Helm reporting "deployed" is not the same as the cluster working. A failed
// verification must roll back just like a failed upgrade.
func TestRun_FailedVerificationRollsBack(t *testing.T) {
	cmd := &fakeCommander{}
	r := newRunner(t, cmd, &fakeVerifier{err: errors.New("still on the old image")})

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run() = nil, want the verification failure")
	}
	if !cmd.ran("rollback") {
		t.Errorf("a healthy-looking release that fails verification must roll back: %v", cmd.calls)
	}
	if !strings.Contains(err.Error(), "still on the old image") {
		t.Errorf("error should carry the verification reason, got: %v", err)
	}
}

func TestRun_FailedRollbackSaysAHumanIsNeeded(t *testing.T) {
	cmd := &fakeCommander{failOn: "upgrade", failRollback: true}
	r := newRunner(t, cmd, &fakeVerifier{})

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run() = nil, want an error")
	}
	for _, want := range []string{"rollback to revision 4 also failed", "helm history"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q, got: %v", want, err)
		}
	}
}

// Helm never upgrades CRDs, so if this step fails the new templates would run
// against an old schema. Nothing may proceed.
func TestRun_CRDFailureStopsBeforeUpgrade(t *testing.T) {
	cmd := &fakeCommander{}
	r := newRunner(t, cmd, &fakeVerifier{})
	r.ApplyCRDsFn = func(context.Context, []*unstructured.Unstructured) ([]string, error) {
		return nil, errors.New("forbidden")
	}

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run() = nil, want the CRD failure")
	}
	if cmd.ran("upgrade") {
		t.Errorf("upgraded after the CRDs failed to apply: %v", cmd.calls)
	}
	if cmd.ran("rollback") {
		t.Error("rolled back despite never having written a release")
	}
	if !strings.Contains(err.Error(), "nothing else has been changed") {
		t.Errorf("error should say the cluster is untouched, got: %v", err)
	}
}

// A chart with no crds/ is not a Kyber chart. Assuming the cluster's existing
// CRDs are fine would be the silent-half-upgrade failure this design exists to
// prevent.
func TestRun_ChartWithoutCRDsIsRefused(t *testing.T) {
	cmd := &fakeCommander{}
	cmd.onPull = func() error { return nil }
	r := newRunner(t, cmd, &fakeVerifier{})
	// Materialise the chart dir but no crds/ inside it.
	chartDir := filepath.Join(r.Cfg.WorkDir, chartName)
	if err := os.MkdirAll(chartDir, 0o755); err != nil {
		t.Fatal(err)
	}

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run() = nil, want a refusal")
	}
	if cmd.ran("upgrade") {
		t.Error("upgraded against a chart with no CRDs")
	}
}

func TestRun_DryRunChangesNothing(t *testing.T) {
	cmd := &fakeCommander{}
	ver := &fakeVerifier{}
	r := newRunner(t, cmd, ver)
	r.Cfg.DryRun = true
	r.ApplyCRDsFn = func(context.Context, []*unstructured.Unstructured) ([]string, error) {
		t.Error("dry run applied CRDs")
		return nil, nil
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if ver.called {
		t.Error("dry run verified a cluster it never changed")
	}
	var upgrade string
	for _, c := range cmd.calls {
		if strings.HasPrefix(c, "upgrade") {
			upgrade = c
		}
	}
	if !strings.Contains(upgrade, "--dry-run") {
		t.Errorf("upgrade command missing --dry-run: %q", upgrade)
	}
}

// --reuse-values hides what a release is configured with. The captured values
// file is the record, and it must be what gets passed.
func TestRun_PassesCapturedValuesNotReuseValues(t *testing.T) {
	cmd := &fakeCommander{values: "api:\n  publicURL: https://example.test\n"}
	r := newRunner(t, cmd, &fakeVerifier{})

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	var upgrade string
	for _, c := range cmd.calls {
		if strings.HasPrefix(c, "upgrade") {
			upgrade = c
		}
	}
	if strings.Contains(upgrade, "--reuse-values") {
		t.Errorf("upgrade used --reuse-values: %q", upgrade)
	}
	valuesPath := filepath.Join(r.Cfg.WorkDir, "values.yaml")
	if !strings.Contains(upgrade, "--values "+valuesPath) {
		t.Errorf("upgrade did not pass the captured values file: %q", upgrade)
	}
	written, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("captured values not written: %v", err)
	}
	if !strings.Contains(string(written), "https://example.test") {
		t.Errorf("captured values file does not hold the release values: %q", written)
	}
}

func TestConfigValidate_NamesEveryMissingField(t *testing.T) {
	err := Config{Release: "kyber"}.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error")
	}
	for _, want := range []string{"namespace", "chart reference", "target version", "helm binary", "work directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q, got: %v", want, err)
		}
	}
}

func TestCaptureValues_NullBecomesEmpty(t *testing.T) {
	cmd := &fakeCommander{values: "null\n"}
	r := newRunner(t, cmd, &fakeVerifier{})

	_, yaml, err := r.captureValues(context.Background())
	if err != nil {
		t.Fatalf("captureValues() = %v", err)
	}
	if yaml != "" {
		t.Errorf("captureValues() = %q, want empty for a release with no overrides", yaml)
	}
}

func TestPinnedImagePaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		yaml  string
		want  []string
		isErr bool
	}{
		{name: "empty", yaml: "", want: nil},
		{name: "no image block", yaml: "api:\n  publicURL: x\n", want: nil},
		{name: "tag", yaml: "image:\n  nodeAgent:\n    tag: v1.0.1\n", want: []string{"image.nodeAgent.tag"}},
		{name: "digest", yaml: "image:\n  nodeAgent:\n    digest: sha256:abc\n", want: []string{"image.nodeAgent.digest"}},
		{name: "empty tag is not a pin", yaml: "image:\n  nodeAgent:\n    tag: \"\"\n", want: nil},
		{name: "repository alone is fine", yaml: "image:\n  nodeAgent:\n    repository: ghcr.io/x\n", want: nil},
		{name: "scalar under image", yaml: "image:\n  pullPolicy: IfNotPresent\n", want: nil},
		{name: "malformed", yaml: "image:\n\t- broken\n", isErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PinnedImagePaths(tc.yaml)
			if tc.isErr {
				if err == nil {
					t.Fatal("PinnedImagePaths() = nil error, want one")
				}
				return
			}
			if err != nil {
				t.Fatalf("PinnedImagePaths() = %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("PinnedImagePaths() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Sorted output keeps the refusal message stable, which matters because it is
// the thing an operator reads out of a Job log.
func TestPinnedImagePaths_IsSorted(t *testing.T) {
	got, err := PinnedImagePaths("image:\n  nodeAgent:\n    tag: v1\n  controlPlane:\n    tag: v1\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"image.controlPlane.tag", "image.nodeAgent.tag"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("PinnedImagePaths() = %v, want %v", got, want)
	}
}
