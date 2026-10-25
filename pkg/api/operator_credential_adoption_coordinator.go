package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/securityaudit"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	validation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type OperatorCredentialAdoptionPlan struct {
	Version        string `json:"version"`
	KeyCount       int    `json:"keyCount"`
	ReconnectCount int    `json:"reconnectCount"`
}
type OperatorCredentialAdoptionOperation struct {
	OperationID string `json:"operationId"`
	Status      string `json:"status"` // pending, failed, finishing, succeeded
	Recoverable bool   `json:"recoverable"`
	ObservedAt  string `json:"observedAt"`
}
type OperatorCredentialAdoptionStart struct {
	OperationID      string `json:"operationId"`
	Version          string `json:"version"`
	ConfirmOwnership bool   `json:"confirmOwnership"`
}
type OperatorCredentialAdoptionRecovery struct {
	OperationID string `json:"operationId"`
	AttemptID   string `json:"attemptId"`
}
type adoptionCoordinatorJournal interface {
	securityaudit.CredentialAdoptionJournal
	Append(context.Context, securityaudit.Event) (securityaudit.Record, error)
	LookupEvent(context.Context, string, string) (securityaudit.Record, error)
}
type operatorCredentialAdoptionManager interface {
	Namespace() string
	Prepare(context.Context, Caller) (OperatorCredentialAdoptionPlan, error)
	Start(context.Context, OperatorCredentialAdoptionStart, Caller, string, func(context.Context) error) (OperatorCredentialAdoptionOperation, error)
	Outcome(context.Context, string) (OperatorCredentialAdoptionOperation, error)
	Pending(context.Context) ([]OperatorCredentialAdoptionOperation, error)
	Recover(context.Context, OperatorCredentialAdoptionRecovery, Caller, string, func(context.Context) error) (OperatorCredentialAdoptionOperation, error)
}

type OperatorCredentialAdoptionCoordinator struct {
	config                                                             *rest.Config
	namespace, sourceName, deploymentName, releaseName, serviceAccount string
	reader                                                             client.Client
	journal                                                            adoptionCoordinatorJournal
	authority                                                          *operatorcredentials.AuthorityStore
	maintenance                                                        *operatorcredentials.MaintenanceStore
}

func NewOperatorCredentialAdoptionCoordinator(config *rest.Config, namespace, sourceName, deploymentName, releaseName, serviceAccount string, journal securityaudit.Store) (*OperatorCredentialAdoptionCoordinator, error) {
	durable, ok := journal.(adoptionCoordinatorJournal)
	if !ok || len(validation.IsDNS1123Label(namespace)) != 0 || !validCredentialSourceLocation(namespace, sourceName) || len(validation.IsDNS1123Subdomain(deploymentName)) != 0 || len(validation.IsDNS1123Subdomain(releaseName)) != 0 || len(validation.IsDNS1123Subdomain(serviceAccount)) != 0 {
		return nil, operatorcredentials.ErrInvalid
	}
	reader, err := newAdoptionNativeClient(config)
	if err != nil {
		return nil, err
	}
	authority, err := operatorcredentials.NewAuthorityStore(config, namespace, operatorcredentials.DefaultSecretName, operatorcredentials.DefaultAuthoritySecretName)
	if err != nil {
		return nil, operatorcredentials.ErrUnavailable
	}
	maintenance, err := operatorcredentials.NewMaintenanceStore(config, namespace)
	if err != nil {
		return nil, operatorcredentials.ErrUnavailable
	}
	return &OperatorCredentialAdoptionCoordinator{config: rest.CopyConfig(config), namespace: namespace, sourceName: sourceName, deploymentName: deploymentName, releaseName: releaseName, serviceAccount: serviceAccount, reader: reader, journal: durable, authority: authority, maintenance: maintenance}, nil
}
func (c *OperatorCredentialAdoptionCoordinator) Namespace() string {
	if c == nil {
		return ""
	}
	return c.namespace
}
func (c *OperatorCredentialAdoptionCoordinator) prepare(ctx context.Context, actor Caller) (OperatorCredentialAdoptionPlan, securityaudit.CredentialIntent, error) {
	fail := func() (OperatorCredentialAdoptionPlan, securityaudit.CredentialIntent, error) {
		return OperatorCredentialAdoptionPlan{}, securityaudit.CredentialIntent{}, operatorcredentials.ErrUnavailable
	}
	if c == nil || !actor.Scopes.full || actor.PrincipalID != installationAdminPrincipalID || actor.CredentialID != installationAdminCredentialID || actor.CredentialGeneration != 1 {
		return fail()
	}
	if _, err := c.authority.Load(ctx); !errors.Is(err, operatorcredentials.ErrUninitialized) {
		return fail()
	}
	maintenance, err := c.maintenance.Inspect(ctx)
	if err != nil || maintenance.Present {
		return fail()
	}
	deployment := &appsv1.Deployment{}
	if c.reader.Get(ctx, client.ObjectKey{Namespace: c.namespace, Name: c.deploymentName}, deployment) != nil || len(deployment.Spec.Template.Spec.Containers) != 1 {
		return fail()
	}
	sourceReader, err := newCredentialAdoptionSourceReader(c.config, c.namespace, c.sourceName)
	if err != nil {
		return fail()
	}
	source, err := sourceReader.Read(ctx)
	if err != nil {
		return fail()
	}
	metadata := source.admissionMetadata()
	metadata.DeploymentName = deployment.Name
	metadata.DeploymentUID = string(deployment.UID)
	metadata.ReleaseName = c.releaseName
	metadata.ControlPlaneImage = deployment.Spec.Template.Spec.Containers[0].Image
	operation := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)
	rootVersion := ""
	for _, reference := range metadata.Sources {
		if reference.Name == metadata.RootSourceName {
			rootVersion = reference.Version
		}
	}
	intent := securityaudit.CredentialIntent{Admission: securityaudit.Event{ID: uuid.NewString(), OperationID: operation, ObservedAt: now, Namespace: c.namespace, PrincipalID: actor.PrincipalID, CredentialID: actor.CredentialID, Generation: actor.CredentialGeneration, RequestID: uuid.NewString(), Action: "credential:adopt", ResourceKind: "credential-adoption", ResourceName: operation, Phase: "authorization", Outcome: "allow", CredentialAuthority: "configuration"}, CompletionEventID: uuid.NewString(), ExpectedVersion: rootVersion, Adoption: &metadata}
	if intent.Validate() != nil {
		return fail()
	}
	preflight, err := newNativeAdoptionPreflight(c.config, intent, "")
	if err != nil {
		return fail()
	}
	source, err = preflight.verify(ctx)
	if err != nil {
		return fail()
	}
	candidate, err := NewAPIKeyAuthenticator(source.root, source.callers...).prepareCredentialAdoption(ctx, c.namespace, adoptionOperation(intent))
	if err != nil {
		return fail()
	}
	registry, err := operatorcredentials.NewSecretStore(c.config, c.namespace, operatorcredentials.DefaultSecretName)
	if err != nil {
		return fail()
	}
	if _, err := registry.Load(ctx); !errors.Is(err, operatorcredentials.ErrUninitialized) {
		return fail()
	}
	after := &appsv1.Deployment{}
	if c.reader.Get(ctx, client.ObjectKeyFromObject(deployment), after) != nil || after.UID != deployment.UID || after.Generation != deployment.Generation || !reflect.DeepEqual(after.Spec, deployment.Spec) {
		return fail()
	}
	// Hash object metadata and declared deployment generation only, never values,
	// value hashes, signers or the private candidate.
	versionInput := struct {
		Namespace  string
		Generation int64
		Metadata   securityaudit.CredentialAdoptionAdmission
	}{c.namespace, deployment.Generation, metadata}
	encoded, _ := json.Marshal(versionInput)
	digest := sha256.Sum256(encoded)
	return OperatorCredentialAdoptionPlan{Version: hex.EncodeToString(digest[:]), KeyCount: len(candidate.document.Credentials), ReconnectCount: candidate.browserReconnects}, intent, nil
}
func (c *OperatorCredentialAdoptionCoordinator) Prepare(ctx context.Context, actor Caller) (OperatorCredentialAdoptionPlan, error) {
	plan, _, err := c.prepare(ctx, actor)
	return plan, err
}
func (c *OperatorCredentialAdoptionCoordinator) Start(ctx context.Context, request OperatorCredentialAdoptionStart, actor Caller, requestID string, authorize func(context.Context) error) (OperatorCredentialAdoptionOperation, error) {
	if !request.ConfirmOwnership || !canonicalAdoptionUUID(request.OperationID) || len(request.Version) != 64 || authorize == nil {
		return OperatorCredentialAdoptionOperation{}, operatorcredentials.ErrInvalid
	}
	existing, err := c.journal.GetCredentialOperation(ctx, c.namespace, request.OperationID)
	if err == nil {
		if !c.validOperation(existing) {
			return OperatorCredentialAdoptionOperation{}, operatorcredentials.ErrConflict
		}
		return c.Outcome(ctx, request.OperationID)
	}
	if !errors.Is(err, securityaudit.ErrOperationNotFound) {
		return OperatorCredentialAdoptionOperation{}, operatorcredentials.ErrUnavailable
	}
	plan, intent, err := c.prepare(ctx, actor)
	if err != nil {
		return OperatorCredentialAdoptionOperation{}, err
	}
	if plan.Version != request.Version {
		return OperatorCredentialAdoptionOperation{}, operatorcredentials.ErrConflict
	}
	if err := authorize(ctx); err != nil {
		return OperatorCredentialAdoptionOperation{}, err
	}
	operationUUID, _ := uuid.Parse(request.OperationID)
	intent.Admission.ID = uuid.NewSHA1(operationUUID, []byte("kyber-adoption-admission/v1")).String()
	intent.CompletionEventID = uuid.NewSHA1(operationUUID, []byte("kyber-adoption-completion/v1")).String()
	intent.Admission.OperationID = request.OperationID
	intent.Admission.ResourceName = request.OperationID
	intent.Admission.RequestID = requestID
	admitted, err := c.journal.AdmitCredentialOperation(ctx, intent)
	if err != nil {
		return OperatorCredentialAdoptionOperation{OperationID: request.OperationID, Status: "pending"}, operatorcredentials.ErrUnavailable
	}
	if !admitted.NewlyAdmitted {
		return c.Outcome(ctx, request.OperationID)
	}
	if err := authorize(ctx); err != nil {
		_, _ = c.journal.FinishCredentialAdoptionOperation(ctx, c.namespace, request.OperationID, "failure", time.Now(), nil)
		return OperatorCredentialAdoptionOperation{}, err
	}
	job, err := buildCredentialAdoptionJob(admitted.Intent, admitted.Intent.Adoption.ControlPlaneImage, c.serviceAccount, "", "")
	if err != nil {
		return OperatorCredentialAdoptionOperation{OperationID: request.OperationID, Status: "pending"}, operatorcredentials.ErrUnavailable
	}
	// Exactly one launch call after acknowledged first admission. A lost create
	// response stays discoverable in the journal; it is never another launch.
	_ = c.reader.Create(ctx, job)
	return c.Outcome(ctx, request.OperationID)
}
func (c *OperatorCredentialAdoptionCoordinator) validOperation(operation securityaudit.CredentialOperation) bool {
	return operation.Intent.Validate() == nil && operation.Intent.Admission.Namespace == c.namespace && operation.Intent.Admission.ResourceKind == "credential-adoption" && operation.Intent.Adoption.DeploymentName == c.deploymentName && operation.Intent.Adoption.ReleaseName == c.releaseName
}
func (c *OperatorCredentialAdoptionCoordinator) Outcome(ctx context.Context, operationID string) (OperatorCredentialAdoptionOperation, error) {
	if !canonicalAdoptionUUID(operationID) {
		return OperatorCredentialAdoptionOperation{}, operatorcredentials.ErrInvalid
	}
	state, err := c.journal.GetCredentialOperation(ctx, c.namespace, operationID)
	if err != nil || !c.validOperation(state) {
		return OperatorCredentialAdoptionOperation{}, operatorcredentials.ErrUnavailable
	}
	result := OperatorCredentialAdoptionOperation{OperationID: operationID, Status: "pending", ObservedAt: state.Intent.Admission.ObservedAt.UTC().Format(time.RFC3339Nano)}
	status, err := c.maintenance.Inspect(ctx)
	if err != nil {
		return result, operatorcredentials.ErrUnavailable
	}
	if state.Completion != nil {
		result.ObservedAt = state.Completion.ObservedAt.UTC().Format(time.RFC3339Nano)
		if state.Completion.Outcome == "failure" {
			if !status.Present {
				result.Status = "failed"
				return result, nil
			}
		} else {
			result.Status = "finishing"
			if state.AdoptionEffect == nil || state.AdoptionEffect.Stage != "selected" {
				return result, operatorcredentials.ErrUnavailable
			}
			snapshot, err := c.authority.VerifyCompletedSelection(ctx, operatorcredentials.AuthoritySelection{UID: state.AdoptionEffect.AuthorityUID, RegistryUID: state.AdoptionEffect.RegistryUID})
			if err != nil {
				return result, operatorcredentials.ErrUnavailable
			}
			reference, found := findAdoptionReference(snapshot, operationID)
			if found && !matchesAdoptionReference(reference, adoptionOperation(state.Intent)) {
				return result, operatorcredentials.ErrUnavailable
			}
			if !status.Present && (!found || reference.Status == "delivered") {
				result.Status = "succeeded"
				return result, nil
			}
		}
	}
	if status.Present {
		if status.Kind != operatorcredentials.MaintenanceAdoption || status.Writer == nil {
			return result, nil
		}
		if state.AdoptionEffect != nil && (state.AdoptionEffect.MaintenanceUID != status.UID || state.AdoptionEffect.AdmissionVersion != status.AdoptionAdmissionVersion) {
			return result, nil
		}
		writerState, err := c.maintenance.WriterState(ctx, status.UID, status.Version)
		if err != nil || writerState != operatorcredentials.MaintenanceWriterStopped {
			return result, nil
		}
		job := &batchv1.Job{}
		if c.reader.Get(ctx, client.ObjectKey{Namespace: c.namespace, Name: status.Writer.JobName}, job) == nil && string(job.UID) == status.Writer.JobUID && job.Labels[operatorcredentials.AdoptionOperationLabel] == operationID {
			result.Recoverable = true
		}
	} else if state.AdoptionEffect == nil {
		jobs := &batchv1.JobList{}
		if err := c.reader.List(ctx, jobs, client.InNamespace(c.namespace), client.MatchingLabels{operatorcredentials.AdoptionOperationLabel: operationID, "app.kubernetes.io/component": "credential-adoption"}, client.Limit(257)); err != nil || jobs.Continue != "" || len(jobs.Items) > 256 {
			return result, operatorcredentials.ErrUnavailable
		}
		result.Recoverable = true
		for _, job := range jobs.Items {
			stopped, err := operatorcredentials.SupervisedJobStopped(ctx, c.reader, job, "adopt-credentials")
			if err != nil || !stopped {
				result.Recoverable = false
				break
			}
		}
	}
	return result, nil
}
func (c *OperatorCredentialAdoptionCoordinator) Pending(ctx context.Context) ([]OperatorCredentialAdoptionOperation, error) {
	states, err := c.journal.PendingCredentialAdoptionOperations(ctx, c.namespace, "", 25)
	if err != nil {
		return nil, operatorcredentials.ErrUnavailable
	}
	result := make([]OperatorCredentialAdoptionOperation, 0, len(states))
	for _, state := range states {
		if !c.validOperation(state) {
			return nil, operatorcredentials.ErrUnavailable
		}
		view, err := c.Outcome(ctx, state.Intent.Admission.OperationID)
		if err != nil {
			return nil, err
		}
		result = append(result, view)
	}
	// Final audit with unfinished outbox/release remains visible too, through
	// the authoritative maintenance operation's bound native Job label.
	status, err := c.maintenance.Inspect(ctx)
	if err != nil {
		return nil, operatorcredentials.ErrUnavailable
	}
	if status.Present && status.Kind == operatorcredentials.MaintenanceAdoption && status.Writer != nil {
		job := &batchv1.Job{}
		if c.reader.Get(ctx, client.ObjectKey{Namespace: c.namespace, Name: status.Writer.JobName}, job) != nil || string(job.UID) != status.Writer.JobUID {
			return nil, operatorcredentials.ErrUnavailable
		}
		id := job.Labels[operatorcredentials.AdoptionOperationLabel]
		if canonicalAdoptionUUID(id) {
			found := false
			for _, view := range result {
				found = found || view.OperationID == id
			}
			if !found {
				view, err := c.Outcome(ctx, id)
				if err != nil {
					return nil, err
				}
				result = append([]OperatorCredentialAdoptionOperation{view}, result...)
			}
		}
	}
	if len(result) > 25 {
		result = result[:25]
	}
	return result, nil
}
func (c *OperatorCredentialAdoptionCoordinator) Recover(ctx context.Context, request OperatorCredentialAdoptionRecovery, actor Caller, requestID string, authorize func(context.Context) error) (OperatorCredentialAdoptionOperation, error) {
	if !canonicalAdoptionUUID(request.OperationID) || !canonicalAdoptionUUID(request.AttemptID) || authorize == nil {
		return OperatorCredentialAdoptionOperation{}, operatorcredentials.ErrInvalid
	}
	outcome, err := c.Outcome(ctx, request.OperationID)
	if err != nil {
		return outcome, err
	}
	if !outcome.Recoverable {
		return outcome, operatorcredentials.ErrConflict
	}
	state, err := c.journal.GetCredentialOperation(ctx, c.namespace, request.OperationID)
	if err != nil || !c.validOperation(state) {
		return outcome, operatorcredentials.ErrUnavailable
	}
	if err := authorize(ctx); err != nil {
		return OperatorCredentialAdoptionOperation{}, err
	}
	attemptUUID, _ := uuid.Parse(request.AttemptID)
	eventID := uuid.NewSHA1(attemptUUID, []byte("kyber-adoption-recovery-authorization/v1")).String()
	if _, err := c.journal.LookupEvent(ctx, c.namespace, eventID); err == nil {
		return c.Outcome(ctx, request.OperationID)
	} else if !errors.Is(err, securityaudit.ErrEventNotFound) {
		return outcome, operatorcredentials.ErrUnavailable
	}
	event := securityaudit.Event{ID: eventID, OperationID: request.OperationID, ObservedAt: time.Now().UTC(), Namespace: c.namespace, PrincipalID: actor.PrincipalID, CredentialID: actor.CredentialID, Generation: actor.CredentialGeneration, RequestID: requestID, Action: "credential:recover", ResourceKind: "credential-adoption", ResourceName: request.OperationID, Phase: "authorization", Outcome: "allow", CredentialAuthority: actor.CredentialAuthority}
	if event.CredentialAuthority == "" {
		event.CredentialAuthority = "configuration"
	}
	if _, err := c.journal.Append(ctx, event); err != nil {
		return outcome, operatorcredentials.ErrUnavailable
	}
	if err := authorize(ctx); err != nil {
		return OperatorCredentialAdoptionOperation{}, err
	}
	deployment := &appsv1.Deployment{}
	if c.reader.Get(ctx, client.ObjectKey{Namespace: c.namespace, Name: c.deploymentName}, deployment) != nil || string(deployment.UID) != state.Intent.Adoption.DeploymentUID || len(deployment.Spec.Template.Spec.Containers) != 1 {
		return outcome, operatorcredentials.ErrUnavailable
	}
	job, err := buildCredentialAdoptionJob(state.Intent, deployment.Spec.Template.Spec.Containers[0].Image, c.serviceAccount, eventID, request.AttemptID)
	if err != nil {
		return outcome, operatorcredentials.ErrUnavailable
	}
	_ = c.reader.Create(ctx, job)
	return c.Outcome(ctx, request.OperationID)
}

var _ operatorCredentialAdoptionManager = (*OperatorCredentialAdoptionCoordinator)(nil)
var _ adoptionCoordinatorJournal = (*securityaudit.PostgresStore)(nil)
