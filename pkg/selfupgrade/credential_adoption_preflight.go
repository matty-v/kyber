package selfupgrade

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Deployment env may include a private database URL. This is an internal
// snapshot, never a response body or diagnostic representation.
type AdoptionReleaseSnapshot struct {
	Revision   int
	Deployment *appsv1.Deployment
	Pods       []corev1.Pod
}

func (AdoptionReleaseSnapshot) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "credential handoff release [redacted]")
}
func (AdoptionReleaseSnapshot) MarshalJSON() ([]byte, error) {
	return nil, operatorcredentials.ErrSensitive
}

// VerifyAdoptionRelease checks the publisher-asserted effective release and all
// ready/owned replicas, including refusing extra old component Pods. It does not
// verify loaded credentials, quiesce old Jobs, reserve maintenance or write.
func (v *MaintenanceReleaseVerifier) VerifyAdoptionRelease(ctx context.Context, release, deploymentName, deploymentUID string) (AdoptionReleaseSnapshot, error) {
	fail := func() (AdoptionReleaseSnapshot, error) {
		return AdoptionReleaseSnapshot{}, ErrMaintenanceReleaseUnverified
	}
	if v == nil || v.reader == nil || v.helm == nil || v.namespace == "" || release == "" || deploymentName == "" || deploymentUID == "" {
		return fail()
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	current, err := v.releaseStatus(ctx, release)
	if err != nil {
		return fail()
	}
	manifest, err := v.helm.Output(ctx, "/usr/local/bin/helm", "get", "manifest", release, "--namespace", v.namespace, "--revision", strconv.Itoa(current.Version))
	if err != nil || len(manifest) > maxCompatibilityManifestBytes {
		return fail()
	}
	expected, err := credentialCompatibleDeployment(manifest, v.namespace, deploymentName)
	if err != nil {
		return fail()
	}
	live, pods, err := v.verifyDeployment(ctx, expected, true)
	if err != nil || string(live.UID) != deploymentUID {
		return fail()
	}
	all := &corev1.PodList{}
	if err := v.reader.List(ctx, all, client.InNamespace(v.namespace), client.MatchingLabels{"app.kubernetes.io/component": "control-plane"}, client.Limit(257)); err != nil || all.Continue != "" || len(all.Items) != len(pods) {
		return fail()
	}
	expectedUIDs := map[string]bool{}
	for _, pod := range pods {
		expectedUIDs[string(pod.UID)] = true
	}
	for _, pod := range all.Items {
		if pod.DeletionTimestamp != nil || !expectedUIDs[string(pod.UID)] {
			return fail()
		}
	}
	final, err := v.releaseStatus(ctx, release)
	if err != nil || final.Version != current.Version {
		return fail()
	}
	return AdoptionReleaseSnapshot{Revision: current.Version, Deployment: live, Pods: pods}, nil
}
