package api

import (
	"strings"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/securityaudit"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	validation "k8s.io/apimachinery/pkg/util/validation"
)

func adoptionJobName(operationID string) string       { return "kyber-key-adopt-" + operationID }
func adoptionRecoveryJobName(attemptID string) string { return "kyber-key-recover-" + attemptID }

// Build only the fixed native execution. No operator material, source JSON,
// database URL or password is projected into its arguments/environment.
func buildCredentialAdoptionJob(intent securityaudit.CredentialIntent, image, serviceAccount, recoveryEventID, attemptID string) (*batchv1.Job, error) {
	if intent.Validate() != nil || intent.Admission.ResourceKind != "credential-adoption" || len(validation.IsDNS1123Subdomain(serviceAccount)) != 0 || !strings.HasPrefix(image, "ghcr.io/matty-v/kyber-control-plane:") || strings.ContainsAny(image, "@ \t\r\n") {
		return nil, operatorcredentials.ErrInvalid
	}
	name := adoptionJobName(intent.Admission.OperationID)
	if recoveryEventID != "" {
		if !canonicalAdoptionUUID(recoveryEventID) || !canonicalAdoptionUUID(attemptID) {
			return nil, operatorcredentials.ErrInvalid
		}
		name = adoptionRecoveryJobName(attemptID)
	} else if attemptID != "" || image != intent.Adoption.ControlPlaneImage {
		return nil, operatorcredentials.ErrInvalid
	}
	zero, one := int32(0), int32(1)
	deadline := int64(600)
	grace := int64(10)
	user := int64(65532)
	yes, no := true, false
	labels := map[string]string{"app.kubernetes.io/name": "kyber", "app.kubernetes.io/component": "credential-adoption", "app.kubernetes.io/managed-by": "kyber-control-plane", operatorcredentials.AdoptionOperationLabel: intent.Admission.OperationID}
	environment := []corev1.EnvVar{
		{Name: "KYBER_ADOPT_NAMESPACE", Value: intent.Admission.Namespace},
		{Name: "KYBER_ADOPT_DEPLOYMENT", Value: intent.Adoption.DeploymentName},
		{Name: "KYBER_ADOPT_OPERATION_ID", Value: intent.Admission.OperationID},
		{Name: "KYBER_ADOPT_POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
		{Name: "KYBER_ADOPT_POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.uid"}}},
		{Name: "HOME", Value: "/tmp"},
	}
	if recoveryEventID != "" {
		environment = append(environment, corev1.EnvVar{Name: "KYBER_ADOPT_RECOVERY_AUTHORIZATION", Value: recoveryEventID})
	}
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: intent.Admission.Namespace, Name: name, Labels: labels}, Spec: batchv1.JobSpec{BackoffLimit: &zero, Parallelism: &one, Completions: &one, ActiveDeadlineSeconds: &deadline, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{ServiceAccountName: serviceAccount, AutomountServiceAccountToken: &yes, RestartPolicy: corev1.RestartPolicyNever, TerminationGracePeriodSeconds: &grace, Containers: []corev1.Container{{Name: "adopt-credentials", Image: image, Command: []string{"/usr/local/bin/kyber-credential-adopt"}, Env: environment, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("128Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")}}, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, RunAsNonRoot: &yes, RunAsUser: &user, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}, VolumeMounts: []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}}}, Volumes: []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(32<<20, resource.BinarySI)}}}}}}}}, nil
}
