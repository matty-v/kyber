package api

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/securityaudit"
	"github.com/matty-v/kyber/pkg/selfupgrade"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	validation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Worker metadata contains no operator credential or database URL. The native
// Job receives names/IDs only and loads the original admission from PostgreSQL.
type CredentialAdoptionWorkerMetadata struct {
	Namespace                    string
	PodName                      string
	PodUID                       string
	DeploymentName               string
	OperationID                  string
	RecoveryAuthorizationEventID string
}

func canonicalAdoptionUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

// RunNativeCredentialAdoption is the fixed native binary entrypoint. Runtime
// protocol zero cannot execute it, including through an operator env override.
func RunNativeCredentialAdoption(ctx context.Context, config *rest.Config, metadata CredentialAdoptionWorkerMetadata) error {
	if operatorcredentials.RuntimeAuthorityProtocolVersion < 1 {
		return operatorcredentials.ErrUnsupported
	}
	return runNativeCredentialAdoption(ctx, config, metadata)
}
func runNativeCredentialAdoption(ctx context.Context, config *rest.Config, metadata CredentialAdoptionWorkerMetadata) error {
	if len(validation.IsDNS1123Label(metadata.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(metadata.PodName)) != 0 || len(validation.IsDNS1123Subdomain(metadata.DeploymentName)) != 0 || !canonicalAdoptionUUID(metadata.PodUID) || !canonicalAdoptionUUID(metadata.OperationID) || metadata.RecoveryAuthorizationEventID != "" && !canonicalAdoptionUUID(metadata.RecoveryAuthorizationEventID) {
		return operatorcredentials.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	reader, err := newAdoptionNativeClient(config)
	if err != nil {
		return errCredentialAdoptionPending
	}
	pod, job, err := readNativeAdoptionExecution(ctx, reader, metadata)
	if err != nil {
		return errCredentialAdoptionPending
	}
	deployment := &appsv1.Deployment{}
	if reader.Get(ctx, client.ObjectKey{Namespace: metadata.Namespace, Name: metadata.DeploymentName}, deployment) != nil || deployment.UID == "" || deployment.Labels["app.kubernetes.io/instance"] == "" {
		return errCredentialAdoptionPending
	}
	release, err := selfupgrade.NewMaintenanceReleaseVerifier(config, metadata.Namespace)
	if err != nil {
		return errCredentialAdoptionPending
	}
	profile, err := release.VerifyAdoptionRelease(ctx, deployment.Labels["app.kubernetes.io/instance"], deployment.Name, string(deployment.UID))
	if err != nil || len(profile.Deployment.Spec.Template.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != profile.Deployment.Spec.Template.Spec.Containers[0].Image {
		return errCredentialAdoptionPending
	}
	connection, err := readAdoptionDatabaseURL(ctx, reader, metadata.Namespace, profile.Deployment.Spec.Template.Spec.Containers[0])
	if err != nil {
		return errCredentialAdoptionPending
	}
	database, err := sql.Open("postgres", connection.value)
	if err != nil {
		return errCredentialAdoptionPending
	}
	defer database.Close()
	database.SetMaxOpenConns(2)
	database.SetMaxIdleConns(1)
	database.SetConnMaxLifetime(2 * time.Minute)
	ping, pingCancel := context.WithTimeout(ctx, 3*time.Second)
	err = database.PingContext(ping)
	pingCancel()
	if err != nil {
		return errCredentialAdoptionPending
	}
	journal, err := securityaudit.NewPostgresStore(database)
	if err != nil {
		return errCredentialAdoptionPending
	}
	state, err := journal.GetCredentialOperation(ctx, metadata.Namespace, metadata.OperationID)
	if err != nil || state.Intent.Validate() != nil || state.Intent.Admission.ResourceKind != "credential-adoption" || state.Intent.Adoption.DeploymentName != metadata.DeploymentName || state.Intent.Adoption.DeploymentUID != string(deployment.UID) || state.Intent.Adoption.ReleaseName != deployment.Labels["app.kubernetes.io/instance"] {
		return errCredentialAdoptionPending
	}
	recovering := metadata.RecoveryAuthorizationEventID != ""
	if !recovering && state.Intent.Adoption.ControlPlaneImage != pod.Spec.Containers[0].Image {
		return errCredentialAdoptionPending
	}
	if recovering {
		record, err := journal.LookupEvent(ctx, metadata.Namespace, metadata.RecoveryAuthorizationEventID)
		if err != nil || !validAdoptionRecoveryAuthorization(record.Event, metadata.OperationID) {
			return errCredentialAdoptionPending
		}
		if !currentNativeRecoveryAdministrator(ctx, config, state.Intent, record.Event) {
			return errCredentialAdoptionPending
		}
	}
	maintenance, err := operatorcredentials.NewMaintenanceStore(config, metadata.Namespace)
	if err != nil {
		return errCredentialAdoptionPending
	}
	status, err := maintenance.Inspect(ctx)
	if err != nil {
		return errCredentialAdoptionPending
	}
	if !status.Present && state.Completion != nil {
		if state.Completion.Outcome == "failure" {
			return errCredentialAdoptionRejected
		}
		authority, err := operatorcredentials.NewAuthorityStore(config, metadata.Namespace, operatorcredentials.DefaultSecretName, operatorcredentials.DefaultAuthoritySecretName)
		if err != nil || state.AdoptionEffect == nil || state.AdoptionEffect.Stage != "selected" {
			return errCredentialAdoptionPending
		}
		snapshot, err := authority.VerifyCompletedSelection(ctx, operatorcredentials.AuthoritySelection{UID: state.AdoptionEffect.AuthorityUID, RegistryUID: state.AdoptionEffect.RegistryUID})
		if err != nil {
			return errCredentialAdoptionPending
		}
		if reference, found := findAdoptionReference(snapshot, metadata.OperationID); found && (reference.Status != "delivered" || !matchesAdoptionReference(reference, adoptionOperation(state.Intent))) {
			return errCredentialAdoptionPending
		}
		return nil
	}
	var reservation *operatorcredentials.MaintenanceReservation
	if status.Present {
		if !recovering || status.Kind != operatorcredentials.MaintenanceAdoption {
			return errCredentialAdoptionPending
		}
		if state.AdoptionEffect != nil && (state.AdoptionEffect.MaintenanceUID != status.UID || state.AdoptionEffect.AdmissionVersion != status.AdoptionAdmissionVersion) {
			return errCredentialAdoptionPending
		}
		reservation, err = maintenance.ResumeAdoptionForPod(ctx, status.UID, status.Version, metadata.OperationID, pod.Name, string(pod.UID))
	} else {
		// A missing previously-bound reservation is not a new bootstrap boundary.
		if state.AdoptionEffect != nil {
			return errCredentialAdoptionPending
		}
		reservation, err = maintenance.AcquireAdoptionForPod(ctx, metadata.OperationID, pod.Name, string(pod.UID))
	}
	if err != nil {
		return errCredentialAdoptionPending
	}
	effects, err := newNativeAdoptionEffects(config, metadata.Namespace, maintenance, reservation)
	if err != nil {
		return errCredentialAdoptionPending
	}
	preflight, err := newNativeAdoptionPreflight(config, state.Intent, string(job.UID))
	if err != nil {
		return errCredentialAdoptionPending
	}
	execution := credentialAdoptionExecution{intent: state.Intent, journal: journal, effects: effects, preflight: preflight.verify}
	return execution.run(ctx)
}
func validAdoptionRecoveryAuthorization(event securityaudit.Event, operationID string) bool {
	return event.Validate() == nil && event.Action == "credential:recover" && event.OperationID == operationID && event.ResourceKind == "credential-adoption" && event.ResourceName == operationID && event.Phase == "authorization" && event.Outcome == "allow"
}
func currentNativeRecoveryAdministrator(ctx context.Context, config *rest.Config, intent securityaudit.CredentialIntent, event securityaudit.Event) bool {
	authority, err := operatorcredentials.NewAuthorityStore(config, intent.Admission.Namespace, operatorcredentials.DefaultSecretName, operatorcredentials.DefaultAuthoritySecretName)
	if err != nil {
		return false
	}
	snapshot, err := authority.Load(ctx)
	if errors.Is(err, operatorcredentials.ErrUninitialized) {
		return event.CredentialAuthority == "configuration" && event.PrincipalID == installationAdminPrincipalID && event.CredentialID == installationAdminCredentialID && event.Generation == 1
	}
	if err != nil || event.CredentialAuthority != "registry" {
		return false
	}
	for _, credential := range snapshot.Document.Credentials {
		if credential.ID == event.CredentialID && credential.PrincipalID == event.PrincipalID && credential.Generation == event.Generation && credential.State == operatorcredentials.StateActive && credential.Role == operatorcredentials.RoleAdministrator && (credential.ExpiresAt == nil || time.Now().Before(*credential.ExpiresAt)) {
			return true
		}
	}
	return false
}
func readNativeAdoptionExecution(ctx context.Context, reader client.Client, metadata CredentialAdoptionWorkerMetadata) (*corev1.Pod, *batchv1.Job, error) {
	pod := &corev1.Pod{}
	if reader.Get(ctx, client.ObjectKey{Namespace: metadata.Namespace, Name: metadata.PodName}, pod) != nil || string(pod.UID) != metadata.PodUID || pod.DeletionTimestamp != nil || pod.Spec.NodeName == "" || len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 || pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		return nil, nil, operatorcredentials.ErrUnavailable
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "Job" || owner.APIVersion != "batch/v1" || !canonicalAdoptionUUID(string(owner.UID)) {
		return nil, nil, operatorcredentials.ErrUnavailable
	}
	job := &batchv1.Job{}
	if reader.Get(ctx, client.ObjectKey{Namespace: metadata.Namespace, Name: owner.Name}, job) != nil || job.UID != owner.UID || job.Labels[operatorcredentials.AdoptionOperationLabel] != metadata.OperationID || job.Labels["app.kubernetes.io/component"] != "credential-adoption" || job.Spec.TTLSecondsAfterFinished != nil || len(job.Spec.Template.Spec.Containers) != 1 || !reflectAdoptionContainer(pod.Spec.Containers[0], job.Spec.Template.Spec.Containers[0]) {
		return nil, nil, operatorcredentials.ErrUnavailable
	}
	container := pod.Spec.Containers[0]
	if container.Name != "adopt-credentials" || len(container.Command) != 1 || container.Command[0] != "/usr/local/bin/kyber-credential-adopt" || len(container.Args) != 0 || !strings.HasPrefix(container.Image, "ghcr.io/matty-v/kyber-control-plane:") || len(container.EnvFrom) != 0 {
		return nil, nil, operatorcredentials.ErrUnavailable
	}
	// Only fixed metadata and Downward-API fields are part of this process.
	expected := map[string]string{"HOME": "/tmp", "KYBER_ADOPT_NAMESPACE": metadata.Namespace, "KYBER_ADOPT_DEPLOYMENT": metadata.DeploymentName, "KYBER_ADOPT_OPERATION_ID": metadata.OperationID, "KYBER_ADOPT_RECOVERY_AUTHORIZATION": metadata.RecoveryAuthorizationEventID}
	seen := map[string]bool{}
	for _, env := range container.Env {
		if seen[env.Name] {
			return nil, nil, operatorcredentials.ErrUnavailable
		}
		seen[env.Name] = true
		if env.Name == "KYBER_ADOPT_POD_NAME" || env.Name == "KYBER_ADOPT_POD_UID" {
			path := "metadata.name"
			if env.Name == "KYBER_ADOPT_POD_UID" {
				path = "metadata.uid"
			}
			if env.Value != "" || env.ValueFrom == nil || env.ValueFrom.FieldRef == nil || env.ValueFrom.FieldRef.FieldPath != path || env.ValueFrom.SecretKeyRef != nil || env.ValueFrom.ConfigMapKeyRef != nil || env.ValueFrom.ResourceFieldRef != nil {
				return nil, nil, operatorcredentials.ErrUnavailable
			}
		} else if value, ok := expected[env.Name]; !ok || env.ValueFrom != nil || env.Value != value {
			return nil, nil, operatorcredentials.ErrUnavailable
		}
	}
	for _, name := range []string{"KYBER_ADOPT_NAMESPACE", "KYBER_ADOPT_DEPLOYMENT", "KYBER_ADOPT_OPERATION_ID", "KYBER_ADOPT_POD_NAME", "KYBER_ADOPT_POD_UID"} {
		if !seen[name] {
			return nil, nil, operatorcredentials.ErrUnavailable
		}
	}
	if metadata.RecoveryAuthorizationEventID != "" && !seen["KYBER_ADOPT_RECOVERY_AUTHORIZATION"] {
		return nil, nil, operatorcredentials.ErrUnavailable
	}
	return pod, job, nil
}
func reflectAdoptionContainer(pod, job corev1.Container) bool {
	return pod.Name == job.Name && pod.Image == job.Image && reflect.DeepEqual(pod.Command, job.Command) && reflect.DeepEqual(pod.Args, job.Args) && reflect.DeepEqual(pod.Env, job.Env)
}
