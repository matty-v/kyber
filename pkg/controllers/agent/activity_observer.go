package agent

import (
	"context"
	"fmt"
	"github.com/matty-v/kyber/pkg/activity"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/podtoken"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

const observerVolume = "activity-observer-credential"
const observerMount = "/var/run/secrets/kyber-observer"

// A separate Secret is essential: the legacy pod token is also mounted in
// the runtime container. Only the status sidecar receives this credential.
func (r *AgentReconciler) ensureActivityObserverSecret(ctx context.Context, agent *kyberv1.Agent) error {
	if len(r.PodTokenKey) == 0 || agent.UID == "" {
		return nil
	}
	name := agent.Name + "-activity-observer"
	token := podtoken.Sign(activity.ObserverIdentity(agent.Name, string(agent.UID)), r.PodTokenKey)
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: name}, &secret)
	if err == nil {
		owner := metav1.GetControllerOf(&secret)
		if owner == nil || owner.UID != agent.UID {
			return fmt.Errorf("activity observer secret ownership mismatch")
		}
		if string(secret.Data["token"]) == token {
			return nil
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data["token"] = []byte(token)
		return r.Update(ctx, &secret)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	secret = corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agent.Namespace, Labels: map[string]string{"kyber.io/agent": agent.Name, "app.kubernetes.io/managed-by": "kyber-controller", "kyber.io/secret-kind": "activity-observer"}}, Data: map[string][]byte{"token": []byte(token)}}
	if err := ctrl.SetControllerReference(agent, &secret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, &secret)
}

func workObservationMatchesPod(work *kyberv1.WorkObservation, pod *corev1.Pod) bool {
	if work.PodUID != string(pod.UID) || pod.DeletionTimestamp != nil {
		return false
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name == StatusSidecarContainerName {
			return status.State.Running != nil && status.ContainerID == work.ContainerID
		}
	}
	return false
}
