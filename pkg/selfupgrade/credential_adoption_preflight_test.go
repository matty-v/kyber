package selfupgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAdoptionReleaseChecksEveryComponentPodAndPublisherProfile(t *testing.T) {
	for _, scenario := range []string{"healthy", "extra-old-pod", "wrong-deployment", "unready", "unsupported-image"} {
		t.Run(scenario, func(t *testing.T) {
			verifier, _, c, deployment, pod, _ := maintenanceVerifierFixture(t)
			ctx := context.Background()
			pod.Labels["app.kubernetes.io/component"] = "control-plane"
			if c.Update(ctx, pod) != nil {
				t.Fatal("fixture Pod update failed")
			}
			uid := string(deployment.UID)
			switch scenario {
			case "extra-old-pod":
				extra := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: deployment.Namespace, Name: "old-control-plane", UID: types.UID(uuid.NewString()), Labels: map[string]string{"app.kubernetes.io/component": "control-plane"}}}
				if c.Create(ctx, extra) != nil {
					t.Fatal("extra Pod fixture failed")
				}
			case "wrong-deployment":
				uid = uuid.NewString()
			case "unready":
				pod.Status.Conditions = nil
				if c.Status().Update(ctx, pod) != nil {
					t.Fatal("status fixture failed")
				}
			case "unsupported-image":
				verifier.helm.(*maintenanceHelmFixture).manifest = strings.ReplaceAll(credentialCompatibleManifest, `protocol: "1"`, `protocol: "0"`)
			}
			snapshot, err := verifier.VerifyAdoptionRelease(ctx, "kyber", deployment.Name, uid)
			if scenario == "healthy" {
				if err != nil || snapshot.Revision != 8 || snapshot.Deployment.UID != deployment.UID || len(snapshot.Pods) != 1 || snapshot.Pods[0].UID != pod.UID {
					t.Fatal("healthy publisher release did not verify", err)
				}
				if _, err := json.Marshal(snapshot); !errors.Is(err, operatorcredentials.ErrSensitive) {
					t.Fatal("private release snapshot serialized")
				}
				for _, format := range []string{"%v", "%+v", "%#v", "%x"} {
					if !strings.Contains(fmt.Sprintf(format, snapshot), "redacted") {
						t.Fatal("release diagnostic did not redact")
					}
				}
			} else if !errors.Is(err, ErrMaintenanceReleaseUnverified) {
				t.Fatal("unsupported/inconsistent release accepted", err)
			}
			// The verifier must never create or alter source/controller objects.
			after := &corev1.Pod{}
			if c.Get(ctx, client.ObjectKeyFromObject(pod), after) != nil || after.UID != pod.UID {
				t.Fatal("verification changed Pod identity")
			}
		})
	}
}
