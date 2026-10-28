package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNativeAdoptionJobContainsOnlyMetadataAndFixedExecution(t *testing.T) {
	execution, _, _, source, _ := adoptionExecutionFixture(t)
	job, err := buildCredentialAdoptionJob(execution.intent, execution.intent.Adoption.ControlPlaneImage, "kyber-adopt", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if job.Name != adoptionJobName(execution.intent.Admission.OperationID) || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.TTLSecondsAfterFinished != nil || job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever || len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatal("job retries or discards original writer proof")
	}
	container := job.Spec.Template.Spec.Containers[0]
	if len(container.Args) != 0 || len(container.Command) != 1 || container.Command[0] != "/usr/local/bin/kyber-credential-adopt" || len(container.EnvFrom) != 0 || container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("job accepted an alternate execution or writable root")
	}
	raw, _ := json.Marshal(job)
	for _, forbidden := range []string{source.root, source.callers[0].Key, source.callers[1].Key, "postgres://", "POSTGRES_PASSWORD", "KYBER_API_KEY", "registry.json", `"sources"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("job projected credential/configuration data")
		}
	}
	attempt, event := uuid.NewString(), uuid.NewString()
	recovery, err := buildCredentialAdoptionJob(execution.intent, execution.intent.Adoption.ControlPlaneImage, "kyber-adopt", event, attempt)
	if err != nil || recovery.Name != adoptionRecoveryJobName(attempt) || recovery.Labels[operatorcredentials.AdoptionOperationLabel] != execution.intent.Admission.OperationID {
		t.Fatal("recovery replaced original operation identity", err)
	}
	if _, err := buildCredentialAdoptionJob(execution.intent, "custom.invalid/image:1.0", "kyber-adopt", "", ""); !errors.Is(err, operatorcredentials.ErrInvalid) {
		t.Fatal("custom initial image accepted")
	}
}
func TestNativeAdoptionWorkerVerifiesOwnImmutableMetadata(t *testing.T) {
	execution, _, _, _, _ := adoptionExecutionFixture(t)
	job, err := buildCredentialAdoptionJob(execution.intent, execution.intent.Adoption.ControlPlaneImage, "kyber-adopt", "", "")
	if err != nil {
		t.Fatal(err)
	}
	job.UID = types.UID(uuid.NewString())
	controller := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: "native-pod", UID: types.UID(uuid.NewString()), OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller}}}, Spec: *job.Spec.Template.Spec.DeepCopy()}
	pod.Spec.NodeName = "test-node"
	metadata := CredentialAdoptionWorkerMetadata{Namespace: pod.Namespace, PodName: pod.Name, PodUID: string(pod.UID), OperationID: execution.intent.Admission.OperationID, DeploymentName: execution.intent.Adoption.DeploymentName}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	for _, scenario := range []string{"valid", "extra-secret-env", "different-image", "wrong-operation", "different-pod"} {
		t.Run(scenario, func(t *testing.T) {
			candidate := pod.DeepCopy()
			otherJob := job.DeepCopy()
			input := metadata
			switch scenario {
			case "extra-secret-env":
				candidate.Spec.Containers[0].Env = append(candidate.Spec.Containers[0].Env, corev1.EnvVar{Name: "KYBER_API_KEY", Value: "bad-test"})
			case "different-image":
				candidate.Spec.Containers[0].Image = "other/image:test"
			case "wrong-operation":
				input.OperationID = uuid.NewString()
			case "different-pod":
				input.PodUID = uuid.NewString()
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(candidate, otherJob).Build()
			_, _, err := readNativeAdoptionExecution(context.Background(), reader, input)
			if (err == nil) != (scenario == "valid") {
				t.Fatal("incorrect native execution identity validation", scenario)
			}
		})
	}
}
func TestNativeAdoptionProtocolZeroCannotBeEnabledByEnvironment(t *testing.T) {
	if operatorcredentials.RuntimeAuthorityProtocolVersion != 0 {
		t.Skip("pre-activation contract only")
	}
	t.Setenv("KYBER_OPERATOR_CREDENTIAL_AUTHORITY_PROTOCOL", "1")
	config := &rest.Config{Host: "https://unused.invalid", WrapTransport: func(http.RoundTripper) http.RoundTripper {
		t.Fatal("inactive native worker read Kubernetes")
		return nil
	}}
	if err := RunNativeCredentialAdoption(context.Background(), config, CredentialAdoptionWorkerMetadata{}); !errors.Is(err, operatorcredentials.ErrUnsupported) {
		t.Fatal("environment activated incomplete protocol", err)
	}
}
