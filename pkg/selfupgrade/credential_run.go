package selfupgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type credentialUpgradeRun struct {
	coordinator   UpgradeCredentialCoordinator
	reservation   UpgradeCredentialReservation
	selected      bool
	safeToRelease bool
	fromRevision  int
	rollbackImage string
}

// Run coordinates before any release/authority decision. Reservation cleanup is
// explicit, not deferred: a panic, process death or uncertain external effect
// must retain ownership for recovery rather than silently allow adoption.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.Cfg.Validate(); err != nil {
		return err
	}
	if r.CredentialCoordinator == nil {
		return errors.New("operator credential upgrade coordination is required")
	}
	started := r.now()
	session := &credentialUpgradeRun{coordinator: r.CredentialCoordinator, safeToRelease: true}
	if !r.Cfg.DryRun && r.CredentialCoordinator.MaintenanceRequired() {
		var err error
		session.reservation, err = r.CredentialCoordinator.Reserve(ctx)
		if err != nil {
			return err
		}
		if session.reservation == nil {
			return errors.New("operator credential maintenance reservation unavailable")
		}
	}
	selected, err := r.CredentialCoordinator.Selected(ctx)
	session.selected = selected
	if err == nil {
		err = r.runReserved(ctx, session)
	}
	if session.reservation != nil {
		if session.safeToRelease {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			releaseErr := session.reservation.Release(releaseCtx)
			cancel()
			if releaseErr != nil {
				err = errors.Join(err, fmt.Errorf("release operator credential maintenance: %w", releaseErr))
			}
		} else {
			err = errors.Join(err, errors.New("operator credential maintenance retained: reconcile the prior upgrade and stop its writers before recovery; another upgrade or adoption must not proceed"))
		}
	}
	if err == nil && !r.Cfg.DryRun {
		r.log().Info("self-upgrade complete", "targetVersion", r.Cfg.TargetVersion, "fromRevision", session.fromRevision, "elapsed", r.now().Sub(started).String())
	}
	return err
}

func (s *credentialUpgradeRun) check(ctx context.Context) error {
	if s.reservation != nil {
		if err := s.reservation.Check(ctx); err != nil {
			return fmt.Errorf("check operator credential maintenance: %w", err)
		}
	}
	selected, err := s.coordinator.Selected(ctx)
	if err != nil {
		return err
	}
	if selected != s.selected {
		return errors.New("operator credential authority changed during the upgrade; refusing further changes")
	}
	return nil
}

func (r *Runner) validateCredentialCompatibility(ctx context.Context, revision int, chartDir, valuesPath string, session *credentialUpgradeRun) error {
	if r.Cfg.ChartRef != "oci://ghcr.io/matty-v/charts/kyber" || r.Cfg.ControlPlaneDeployment == "" {
		return errCredentialCompatibility
	}
	rendered, err := r.Cmd.Output(ctx, r.Cfg.HelmBin, "template", r.Cfg.Release, chartDir, "--namespace", r.Cfg.Namespace, "--values", valuesPath)
	if err != nil {
		return errCredentialCompatibility
	}
	target, err := credentialCompatibleDeployment(rendered, r.Cfg.Namespace, r.Cfg.ControlPlaneDeployment)
	if err != nil || !tagMatchesVersion(imageTag(controlPlaneImage(target)), r.Cfg.TargetVersion) {
		return errCredentialCompatibility
	}
	rollback, err := r.Cmd.Output(ctx, r.Cfg.HelmBin, "get", "manifest", r.Cfg.Release, "--namespace", r.Cfg.Namespace, "--revision", strconv.Itoa(revision))
	if err != nil {
		return errCredentialCompatibility
	}
	previous, err := credentialCompatibleDeployment(rollback, r.Cfg.Namespace, r.Cfg.ControlPlaneDeployment)
	if err != nil || r.K8s == nil {
		return errCredentialCompatibility
	}
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	current := &appsv1.Deployment{}
	if err := r.K8s.Get(readCtx, client.ObjectKey{Namespace: r.Cfg.Namespace, Name: r.Cfg.ControlPlaneDeployment}, current); err != nil {
		return errCredentialCompatibility
	}
	current.APIVersion = "apps/v1"
	current.Kind = "Deployment"
	raw, err := json.Marshal(current)
	if err != nil {
		return errCredentialCompatibility
	}
	live, err := credentialCompatibleDeployment(string(raw), r.Cfg.Namespace, r.Cfg.ControlPlaneDeployment)
	if err != nil || controlPlaneImage(live) != controlPlaneImage(previous) {
		return errCredentialCompatibility
	}
	session.rollbackImage = controlPlaneImage(previous)
	return nil
}

func (r *Runner) checkCredentialRollback(ctx context.Context, revision int, session *credentialUpgradeRun) error {
	if !session.selected {
		return nil
	}
	if session.rollbackImage == "" {
		return errCredentialCompatibility
	}
	manifest, err := r.Cmd.Output(ctx, r.Cfg.HelmBin, "get", "manifest", r.Cfg.Release, "--namespace", r.Cfg.Namespace, "--revision", strconv.Itoa(revision))
	if err != nil {
		return errCredentialCompatibility
	}
	previous, err := credentialCompatibleDeployment(manifest, r.Cfg.Namespace, r.Cfg.ControlPlaneDeployment)
	if err != nil || controlPlaneImage(previous) != session.rollbackImage {
		return errCredentialCompatibility
	}
	return nil
}
