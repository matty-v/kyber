package selfupgrade

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeCredentialCoordinator struct {
	checkpointErr       error
	checkpoints         []UpgradeCredentialEffects
	selected            bool
	readErr             error
	reserveErr          error
	checkErr            error
	releaseErr          error
	reserved            int
	released            int
	checked             int
	reads               int
	onRead              func(int) (bool, error)
	maintenanceDisabled bool
}

func (c *fakeCredentialCoordinator) MaintenanceRequired() bool { return !c.maintenanceDisabled }

func (c *fakeCredentialCoordinator) Reserve(context.Context) (UpgradeCredentialReservation, error) {
	c.reserved++
	if c.reserveErr != nil {
		return nil, c.reserveErr
	}
	return c, nil
}
func (c *fakeCredentialCoordinator) Selected(context.Context) (bool, error) {
	c.reads++
	if c.onRead != nil {
		return c.onRead(c.reads)
	}
	return c.selected, c.readErr
}
func (c *fakeCredentialCoordinator) RecordEffects(_ context.Context, effects UpgradeCredentialEffects) error {
	c.checkpoints = append(c.checkpoints, effects)
	return c.checkpointErr
}
func (c *fakeCredentialCoordinator) Check(context.Context) error   { c.checked++; return c.checkErr }
func (c *fakeCredentialCoordinator) Release(context.Context) error { c.released++; return c.releaseErr }

func TestCredentialUpgradeReservationLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failOn      string
		verifyErr   error
		dry         bool
		wantRelease int
	}{
		{name: "success", wantRelease: 1},
		{name: "preflight failure", failOn: "status", wantRelease: 1},
		{name: "uncertain upgrade", failOn: "upgrade kyber-canary"},
		{name: "failed verification", verifyErr: errors.New("not ready")},
		{name: "dry run", dry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeCommander{failOn: tc.failOn}
			r := newRunner(t, cmd, &fakeVerifier{err: tc.verifyErr})
			r.Cfg.DryRun = tc.dry
			guard := &fakeCredentialCoordinator{}
			r.CredentialCoordinator = guard
			err := r.Run(context.Background())
			if guard.released != tc.wantRelease {
				t.Fatalf("releases=%d want=%d", guard.released, tc.wantRelease)
			}
			if tc.dry && guard.reserved != 0 || !tc.dry && guard.reserved != 1 {
				t.Fatal("wrong reservation behavior")
			}
			if (tc.failOn != "" || tc.verifyErr != nil) != (err != nil) {
				t.Fatal("wrong operation result", err)
			}
			if !tc.dry && tc.wantRelease == 0 && (err == nil || !strings.Contains(err.Error(), "maintenance retained")) {
				t.Fatal("uncertain effect not retained", err)
			}
		})
	}
}

func TestCredentialUpgradeRefusesBeforeEffects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		guard *fakeCredentialCoordinator
	}{
		{"authority outage", &fakeCredentialCoordinator{readErr: errors.New("unavailable")}},
		{"reservation held", &fakeCredentialCoordinator{reserveErr: errors.New("held")}},
		{"reservation lost", &fakeCredentialCoordinator{checkErr: errors.New("lost")}},
		{"checkpoint unavailable", &fakeCredentialCoordinator{checkpointErr: errors.New("unavailable")}},
		{"incompatible target", &fakeCredentialCoordinator{selected: true}},
		{"authority changed", &fakeCredentialCoordinator{onRead: func(n int) (bool, error) { return n > 1, nil }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeCommander{}
			r := newRunner(t, cmd, &fakeVerifier{})
			r.CredentialCoordinator = tc.guard
			applied := false
			r.ApplyCRDsFn = func(context.Context, []*unstructured.Unstructured) ([]string, error) { applied = true; return nil, nil }
			if err := r.Run(context.Background()); err == nil {
				t.Fatal("missing refusal")
			}
			if applied || cmd.ran("upgrade") || cmd.ran("rollback") {
				t.Fatal("refusal occurred after effects")
			}
		})
	}
}

func TestCredentialUpgradeReleaseFailureNeverReportsSuccess(t *testing.T) {
	r := newRunner(t, &fakeCommander{}, &fakeVerifier{})
	r.CredentialCoordinator = &fakeCredentialCoordinator{releaseErr: errors.New("release unavailable")}
	if err := r.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "release operator credential maintenance") {
		t.Fatal("release failure hidden", err)
	}
}

func TestCredentialUpgradeChecksTargetRollbackAndLiveWorkload(t *testing.T) {
	for _, mode := range []string{"compatible", "old target", "old rollback", "custom target", "live mismatch", "missing live client"} {
		t.Run(mode, func(t *testing.T) {
			target := credentialCompatibleManifest
			previous := strings.ReplaceAll(target, "v1.9.0", "v1.8.1")
			live, err := credentialCompatibleDeployment(previous, "kyber-system", "kyber-control-plane")
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "old target":
				target = strings.Replace(target, `protocol: "1"`, `protocol: "0"`, 1)
			case "old rollback":
				previous = strings.Replace(previous, `protocol: "1"`, `protocol: "0"`, 1)
			case "custom target":
				target = strings.Replace(target, "        image: ghcr.io/matty-v/kyber-control-plane:v1.9.0", "        image: custom.invalid/control-plane:v1.9.0", 1)
			case "live mismatch":
				live.Spec.Template.Spec.Containers[0].Image = "ghcr.io/matty-v/kyber-control-plane:v1.8.0"
			}
			cmd := &fakeCommander{templateManifest: target, rollbackManifest: previous}
			r := newRunner(t, cmd, &fakeVerifier{})
			r.Cfg.TargetVersion = "1.9.0"
			r.Cfg.ControlPlaneDeployment = "kyber-control-plane"
			guard := &fakeCredentialCoordinator{selected: true}
			r.CredentialCoordinator = guard
			scheme := runtime.NewScheme()
			if err := appsv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			r.K8s = fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()
			if mode == "missing live client" {
				r.K8s = nil
			}
			applied := false
			r.ApplyCRDsFn = func(context.Context, []*unstructured.Unstructured) ([]string, error) { applied = true; return nil, nil }
			err = r.Run(context.Background())
			if mode == "compatible" {
				if err != nil || !applied || !cmd.ran("upgrade") || guard.released != 1 {
					t.Fatal("compatible upgrade failed", err)
				}
				return
			}
			if !errors.Is(err, errCredentialCompatibility) || applied || cmd.ran("upgrade") || cmd.ran("rollback") || guard.released != 1 {
				t.Fatal("compatibility refusal was late or incomplete", err)
			}
		})
	}
}

type revokeBeforeRollbackVerifier struct{ guard *fakeCredentialCoordinator }

func (v revokeBeforeRollbackVerifier) Verify(context.Context, string) error {
	v.guard.checkErr = errors.New("ownership lost")
	return errors.New("target health failed")
}

func TestCredentialUpgradeRefusesUnownedRollbackAndRetainsRecovery(t *testing.T) {
	cmd := &fakeCommander{}
	guard := &fakeCredentialCoordinator{}
	r := newRunner(t, cmd, revokeBeforeRollbackVerifier{guard: guard})
	r.CredentialCoordinator = guard
	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing rollback") || !strings.Contains(err.Error(), "maintenance retained") || cmd.ran("rollback") || guard.released != 0 {
		t.Fatal("unowned rollback ran or reservation was released", err)
	}
}

type credentialVerifyFunc func(context.Context, string) error

func (f credentialVerifyFunc) Verify(ctx context.Context, version string) error {
	return f(ctx, version)
}

func TestCredentialUpgradeRechecksApprovedRollbackRevision(t *testing.T) {
	for _, mode := range []string{"unchanged", "old protocol", "changed image", "unavailable history"} {
		t.Run(mode, func(t *testing.T) {
			previous := strings.ReplaceAll(credentialCompatibleManifest, "v1.9.0", "v1.8.1")
			live, err := credentialCompatibleDeployment(previous, "kyber-system", "kyber-control-plane")
			if err != nil {
				t.Fatal(err)
			}
			cmd := &fakeCommander{templateManifest: credentialCompatibleManifest, rollbackManifest: previous}
			verify := credentialVerifyFunc(func(context.Context, string) error {
				switch mode {
				case "old protocol":
					cmd.rollbackManifest = strings.Replace(previous, `protocol: "1"`, `protocol: "0"`, 1)
				case "changed image":
					cmd.rollbackManifest = strings.ReplaceAll(previous, "v1.8.1", "v1.8.2")
				case "unavailable history":
					cmd.failOn = "get manifest"
				}
				return errors.New("target health failed")
			})
			r := newRunner(t, cmd, verify)
			r.Cfg.TargetVersion = "1.9.0"
			r.Cfg.ControlPlaneDeployment = "kyber-control-plane"
			guard := &fakeCredentialCoordinator{selected: true}
			r.CredentialCoordinator = guard
			scheme := runtime.NewScheme()
			if err := appsv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			r.K8s = fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()
			err = r.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "maintenance retained") || guard.released != 0 {
				t.Fatal("failed operation lost its recovery state", err)
			}
			if cmd.ran("rollback") != (mode == "unchanged") {
				t.Fatal("incorrect rollback behavior", err)
			}
		})
	}
}

func TestCredentialUpgradeCheckpointPrecedesExternalEffects(t *testing.T) {
	cmd := &fakeCommander{}
	runner := newRunner(t, cmd, &fakeVerifier{})
	guard := &fakeCredentialCoordinator{}
	runner.CredentialCoordinator = guard
	runner.ApplyCRDsFn = func(context.Context, []*unstructured.Unstructured) ([]string, error) {
		if len(guard.checkpoints) != 1 {
			t.Fatal("CRD effect began without one checkpoint")
		}
		checkpoint := guard.checkpoints[0]
		if checkpoint.Release != runner.Cfg.Release || checkpoint.InitialRevision < 1 || checkpoint.TargetVersion != runner.Cfg.TargetVersion || checkpoint.ControlPlaneDeployment != runner.Cfg.ControlPlaneDeployment || checkpoint.Selected {
			t.Fatal("incorrect effect checkpoint")
		}
		return []string{"fixture-crd"}, nil
	}
	if err := runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(guard.checkpoints) != 1 {
		t.Fatal("effect checkpoint repeated")
	}
}
