package selfupgrade

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultPollInterval is how often verification re-reads the cluster.
const DefaultPollInterval = 5 * time.Second

// DeploymentVerifier decides whether an upgrade actually took.
//
// It checks three separate things, because each one has been true while
// another was false on these clusters:
//
//  1. The control-plane Deployment is rolled out — observedGeneration caught
//     up, every replica updated and ready.
//  2. The running container's image tag is the version we asked for. This is
//     the check that catches an upgrade which rewrote the templates but left
//     the old images running, and it is deliberately read from the live
//     Deployment rather than from Helm's release record. "Synced and Healthy"
//     while serving a stale revision is a failure mode this platform has
//     actually shipped.
//  3. /healthz answers 200 — the new binary is serving, not just scheduled.
//
// Deliberately NOT checked: /api/v1/version. It requires an API key, which
// would mean mounting the cluster's credentials Secret into the upgrade Job to
// learn something the Deployment's image tag already tells us.
type DeploymentVerifier struct {
	Client     client.Client
	Namespace  string
	Deployment string

	// HealthURL is the in-cluster control-plane health endpoint. Unauthenticated.
	HealthURL string

	HTTP     *http.Client
	Interval time.Duration
	Log      *slog.Logger
}

func (v *DeploymentVerifier) interval() time.Duration {
	if v.Interval > 0 {
		return v.Interval
	}
	return DefaultPollInterval
}

func (v *DeploymentVerifier) log() *slog.Logger {
	if v.Log != nil {
		return v.Log
	}
	return slog.Default()
}

func (v *DeploymentVerifier) httpClient() *http.Client {
	if v.HTTP != nil {
		return v.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Verify blocks until the cluster is demonstrably running targetVersion, or
// the context expires. The error names the check that never passed, and the
// last thing it saw — an upgrade that timed out with "still on 1.0.1" is a
// different problem from one that timed out with "0/1 replicas ready".
func (v *DeploymentVerifier) Verify(ctx context.Context, targetVersion string) error {
	ticker := time.NewTicker(v.interval())
	defer ticker.Stop()

	var last string
	for {
		state, err := v.check(ctx, targetVersion)
		if err == nil {
			return nil
		}
		last = err.Error()
		if state != "" {
			v.log().Info("verifying upgrade", "state", state)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting: %s", last)
		case <-ticker.C:
		}
	}
}

// check runs one round. The returned string is a human-readable state for the
// log; the error is nil only when every check passed.
func (v *DeploymentVerifier) check(ctx context.Context, targetVersion string) (string, error) {
	if v.Client == nil {
		return "", fmt.Errorf("no Kubernetes client configured")
	}
	dep := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: v.Namespace, Name: v.Deployment}
	if err := v.Client.Get(ctx, key, dep); err != nil {
		return "", fmt.Errorf("read Deployment %s/%s: %w", v.Namespace, v.Deployment, err)
	}

	image := controlPlaneImage(dep)
	tag := imageTag(image)
	if tag != targetVersion {
		return fmt.Sprintf("image is %s, want tag %s", image, targetVersion),
			fmt.Errorf("the control-plane Deployment still runs %s; the upgrade did not change the image to %s",
				image, targetVersion)
	}

	if dep.Status.ObservedGeneration < dep.Generation {
		return "rollout not observed yet", fmt.Errorf("Deployment %s has not observed generation %d yet",
			v.Deployment, dep.Generation)
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	if dep.Status.UpdatedReplicas < want || dep.Status.ReadyReplicas < want || dep.Status.UnavailableReplicas > 0 {
		return fmt.Sprintf("%d/%d updated, %d/%d ready", dep.Status.UpdatedReplicas, want, dep.Status.ReadyReplicas, want),
			fmt.Errorf("rollout incomplete: %d/%d updated, %d/%d ready, %d unavailable",
				dep.Status.UpdatedReplicas, want, dep.Status.ReadyReplicas, want, dep.Status.UnavailableReplicas)
	}

	if err := v.probeHealth(ctx); err != nil {
		return "health endpoint not answering yet", err
	}
	return "", nil
}

func (v *DeploymentVerifier) probeHealth(ctx context.Context) error {
	if v.HealthURL == "" {
		return fmt.Errorf("no health URL configured; refusing to call an unprobed control plane healthy")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.HealthURL, nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	resp, err := v.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", v.HealthURL, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %d, want 200", v.HealthURL, resp.StatusCode)
	}
	return nil
}

// controlPlaneImage returns the image of the first container in the pod spec.
// The control-plane Deployment has exactly one container; taking [0] rather
// than searching by name keeps this working if the container is ever renamed,
// which is a rename we would otherwise discover as a mysterious verification
// timeout.
func controlPlaneImage(dep *appsv1.Deployment) string {
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	return dep.Spec.Template.Spec.Containers[0].Image
}

// imageTag extracts the tag from a reference, tolerating a registry port and a
// digest suffix. Returns "" for a reference with no tag.
func imageTag(image string) string {
	if image == "" {
		return ""
	}
	// Drop any digest: repo:tag@sha256:… or repo@sha256:…
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	colon := strings.LastIndex(image, ":")
	if colon < 0 {
		return ""
	}
	// A colon before the last slash is a registry port, not a tag.
	if slash := strings.LastIndex(image, "/"); slash > colon {
		return ""
	}
	return image[colon+1:]
}
