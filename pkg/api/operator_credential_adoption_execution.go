package api

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"time"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/securityaudit"
)

var errCredentialAdoptionPending = errors.New("credential handoff needs an outcome check")
var errCredentialAdoptionRejected = errors.New("credential handoff could not be verified; reload before trying again")

// The production adapter owns an exact private maintenance handle. No public
// request can construct this interface or obtain its ownership capability.
type credentialAdoptionEffects interface {
	Check(context.Context) error
	ReservationMetadata(context.Context) (securityaudit.CredentialAdoptionEffect, error)
	Candidate(context.Context) (operatorcredentials.Snapshot, error)
	CreateCandidate(context.Context, operatorcredentials.Document) (operatorcredentials.Snapshot, error)
	Selection(context.Context) (operatorcredentials.AuthoritySelection, error)
	BindCandidate(context.Context, string) error
	SealSelection(context.Context, operatorcredentials.AuthoritySelection) (operatorcredentials.Snapshot, error)
	ReplaceSelected(context.Context, operatorcredentials.Snapshot, operatorcredentials.Document) (operatorcredentials.Snapshot, error)
	Release(context.Context) error
}

type credentialAdoptionExecution struct {
	intent  securityaudit.CredentialIntent
	journal securityaudit.CredentialAdoptionJournal
	effects credentialAdoptionEffects
	// This callback verifies published release/replicas/old writers and returns
	// the exact frozen source graph. It is not a permission to skip Check.
	preflight func(context.Context) (*credentialAdoptionSources, error)
}

func (s *credentialAdoptionSources) admissionMetadata() securityaudit.CredentialAdoptionAdmission {
	sources := make([]securityaudit.CredentialAdoptionSource, 0, len(s.versions))
	for key, version := range s.versions {
		sources = append(sources, securityaudit.CredentialAdoptionSource{Name: key.Name, UID: string(version.uid), Version: version.version})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })
	return securityaudit.CredentialAdoptionAdmission{RootSourceName: s.rootName, Sources: sources}
}
func adoptionOperation(intent securityaudit.CredentialIntent) operatorcredentials.Operation {
	e := intent.Admission
	return operatorcredentials.Operation{ID: e.OperationID, AdmissionEventID: e.ID, CompletionEventID: intent.CompletionEventID, RequestID: e.RequestID, Action: "adopt", ActorPrincipalID: e.PrincipalID, ActorCredentialID: e.CredentialID, ActorGeneration: e.Generation, Authority: "configuration", At: e.ObservedAt.UTC().Truncate(time.Microsecond), Status: "pending"}
}
func matchesAdoptionReference(actual, expected operatorcredentials.Operation) bool {
	if actual.Status != "pending" && actual.Status != "delivered" {
		return false
	}
	actual.Status = "pending"
	return actual == expected
}
func findAdoptionReference(snapshot operatorcredentials.Snapshot, id string) (operatorcredentials.Operation, bool) {
	for _, reference := range snapshot.Document.Operations {
		if reference.ID == id {
			return reference, true
		}
	}
	return operatorcredentials.Operation{}, false
}

// run consumes the immutable admission created before native Job launch.
// Its adapter already owns a single-execution private reservation; a worker
// cannot invent an admission from resubmitted input or a new timestamp.
func (e *credentialAdoptionExecution) run(ctx context.Context) error {
	if e == nil || e.journal == nil || e.effects == nil || e.intent.Validate() != nil || e.intent.Admission.ResourceKind != "credential-adoption" {
		return operatorcredentials.ErrInvalid
	}
	if e.effects.Check(ctx) != nil {
		return errCredentialAdoptionPending
	}
	state, err := e.journal.GetCredentialOperation(ctx, e.intent.Admission.Namespace, e.intent.Admission.OperationID)
	if err != nil || !reflect.DeepEqual(state.Intent, e.intent) {
		return errCredentialAdoptionPending
	}
	if state.Completion != nil {
		if state.Completion.Outcome == "failure" {
			if state.AdoptionEffect != nil && state.AdoptionEffect.Stage != "reserved" || e.effects.Release(ctx) != nil {
				return errCredentialAdoptionPending
			}
			return errCredentialAdoptionRejected
		}
		return e.complete(ctx, state)
	}
	reservation, err := e.effects.ReservationMetadata(ctx)
	if err != nil || reservation.Stage != "reserved" || reservation.Validate() != nil {
		return errCredentialAdoptionPending
	}
	if state.AdoptionEffect == nil {
		state, err = e.journal.RecordCredentialAdoptionEffect(ctx, e.intent.Admission.Namespace, e.intent.Admission.OperationID, reservation)
		if err != nil {
			return errCredentialAdoptionPending
		}
	}
	if state.AdoptionEffect.MaintenanceUID != reservation.MaintenanceUID || state.AdoptionEffect.AdmissionVersion != reservation.AdmissionVersion {
		return errCredentialAdoptionPending
	}
	selection, selectionErr := e.effects.Selection(ctx)
	if selectionErr == nil {
		// Two administrators can finish preflight before either launch acquires
		// maintenance. If another operation already selected authority before
		// this one admitted a candidate, durably reject this operation and
		// release its reservation. It must not strand a successfully managed
		// installation behind an unrelated, effect-free pending handoff.
		if state.AdoptionEffect.Stage == "reserved" {
			return e.rejectWithoutEffects(ctx, state)
		}
		if state.AdoptionEffect == nil || (state.AdoptionEffect.Stage != "selection" && state.AdoptionEffect.Stage != "selected") || state.AdoptionEffect.RegistryUID != selection.RegistryUID {
			return errCredentialAdoptionPending
		}
		return e.selectAndComplete(ctx, state, selection)
	}
	if !errors.Is(selectionErr, operatorcredentials.ErrUninitialized) {
		return errCredentialAdoptionPending
	}
	if state.AdoptionEffect != nil && state.AdoptionEffect.Stage == "selected" {
		return errCredentialAdoptionPending
	}
	if e.preflight == nil {
		return e.rejectWithoutEffects(ctx, state)
	}
	source, err := e.preflight(ctx)
	if err != nil || source == nil {
		return e.rejectWithoutEffects(ctx, state)
	}
	metadata := source.admissionMetadata()
	if !sameAdoptionSources(metadata, *e.intent.Adoption) || source.namespace != e.intent.Admission.Namespace || source.Verify(ctx) != nil {
		return e.rejectWithoutEffects(ctx, state)
	}
	candidate, err := NewAPIKeyAuthenticator(source.root, source.callers...).prepareCredentialAdoption(ctx, source.namespace, adoptionOperation(e.intent))
	if err != nil {
		return e.rejectWithoutEffects(ctx, state)
	}
	// Detect an unrelated fixed-name object before admitting any create.
	snapshot, readErr := e.effects.Candidate(ctx)
	if readErr == nil && !reflect.DeepEqual(snapshot.Document.Clone(), candidate.document.Clone()) {
		return e.rejectWithoutEffects(ctx, state)
	}
	if readErr != nil && !errors.Is(readErr, operatorcredentials.ErrUninitialized) {
		return e.rejectWithoutEffects(ctx, state)
	}
	if state.AdoptionEffect.Stage == "reserved" {
		state, err = e.journal.RecordCredentialAdoptionEffect(ctx, source.namespace, e.intent.Admission.OperationID, nextAdoptionCheckpoint(state.AdoptionEffect, "candidate", "", ""))
		if err != nil {
			return errCredentialAdoptionPending
		}
	}
	if state.AdoptionEffect.Stage == "candidate" {
		if errors.Is(readErr, operatorcredentials.ErrUninitialized) {
			if source.Verify(ctx) != nil || e.effects.Check(ctx) != nil {
				return errCredentialAdoptionPending
			}
			snapshot, err = e.effects.CreateCandidate(ctx, candidate.document)
			if err != nil {
				// Observe a possible committed create once; never submit another create
				// after an uncertain response inside this execution.
				snapshot, err = e.effects.Candidate(ctx)
				if err != nil {
					return errCredentialAdoptionPending
				}
			}
		}
		if !reflect.DeepEqual(snapshot.Document.Clone(), candidate.document.Clone()) || snapshot.UID == "" || snapshot.Version == "" {
			return errCredentialAdoptionPending
		}
		state, err = e.journal.RecordCredentialAdoptionEffect(ctx, source.namespace, e.intent.Admission.OperationID, nextAdoptionCheckpoint(state.AdoptionEffect, "registry", snapshot.UID, ""))
		if err != nil {
			return errCredentialAdoptionPending
		}
	}
	if state.AdoptionEffect.RegistryUID != snapshot.UID || !reflect.DeepEqual(snapshot.Document.Clone(), candidate.document.Clone()) {
		return errCredentialAdoptionPending
	}
	if state.AdoptionEffect.Stage == "registry" {
		state, err = e.journal.RecordCredentialAdoptionEffect(ctx, source.namespace, e.intent.Admission.OperationID, nextAdoptionCheckpoint(state.AdoptionEffect, "selection", snapshot.UID, ""))
		if err != nil {
			return errCredentialAdoptionPending
		}
	}
	if state.AdoptionEffect.Stage != "selection" || source.Verify(ctx) != nil || e.effects.Check(ctx) != nil {
		return errCredentialAdoptionPending
	}
	// Recheck the complete loaded replica/release gate at the handoff boundary,
	// not only the source objects observed before candidate creation.
	confirmed, err := e.preflight(ctx)
	if err != nil || confirmed == nil || confirmed.namespace != e.intent.Admission.Namespace || !sameAdoptionSources(confirmed.admissionMetadata(), *e.intent.Adoption) || confirmed.Verify(ctx) != nil {
		return errCredentialAdoptionPending
	}
	_ = e.effects.BindCandidate(ctx, snapshot.Version)
	selection, err = e.effects.Selection(ctx)
	if err != nil || selection.RegistryUID != snapshot.UID {
		return errCredentialAdoptionPending
	}
	// A lost create response is reconciled by its actual UID-bound selection,
	// never by calling Bind a second time. The same evidence applies on success.
	return e.selectAndComplete(ctx, state, selection)
}

func (e *credentialAdoptionExecution) rejectWithoutEffects(ctx context.Context, state securityaudit.CredentialOperation) error {
	if state.AdoptionEffect != nil && state.AdoptionEffect.Stage != "reserved" {
		return errCredentialAdoptionPending
	}
	if _, err := e.journal.FinishCredentialAdoptionOperation(ctx, e.intent.Admission.Namespace, e.intent.Admission.OperationID, "failure", time.Now(), state.AdoptionEffect); err != nil {
		return errCredentialAdoptionPending
	}
	if e.effects.Release(ctx) != nil {
		return errCredentialAdoptionPending
	}
	return errCredentialAdoptionRejected
}
func (e *credentialAdoptionExecution) selectAndComplete(ctx context.Context, state securityaudit.CredentialOperation, selection operatorcredentials.AuthoritySelection) error {
	if state.AdoptionEffect == nil || state.AdoptionEffect.RegistryUID != selection.RegistryUID || e.effects.Check(ctx) != nil {
		return errCredentialAdoptionPending
	}
	if state.AdoptionEffect.Stage == "selected" && state.AdoptionEffect.AuthorityUID != selection.UID {
		return errCredentialAdoptionPending
	}
	sealed, err := e.effects.SealSelection(ctx, selection)
	if err != nil || sealed.UID != selection.RegistryUID {
		return errCredentialAdoptionPending
	}
	reference, found := findAdoptionReference(sealed, e.intent.Admission.OperationID)
	if !found || !matchesAdoptionReference(reference, adoptionOperation(e.intent)) {
		return errCredentialAdoptionPending
	}
	if state.AdoptionEffect.Stage != "selected" {
		state, err = e.journal.RecordCredentialAdoptionEffect(ctx, e.intent.Admission.Namespace, e.intent.Admission.OperationID, nextAdoptionCheckpoint(state.AdoptionEffect, "selected", selection.RegistryUID, selection.UID))
		if err != nil {
			return errCredentialAdoptionPending
		}
	}
	completion, err := e.journal.FinishCredentialAdoptionOperation(ctx, e.intent.Admission.Namespace, e.intent.Admission.OperationID, "success", time.Now(), state.AdoptionEffect)
	if err != nil {
		return errCredentialAdoptionPending
	}
	state.Completion = &completion
	return e.complete(ctx, state)
}
func (e *credentialAdoptionExecution) complete(ctx context.Context, state securityaudit.CredentialOperation) error {
	if state.Completion == nil || state.Completion.Outcome != "success" || state.AdoptionEffect == nil || state.AdoptionEffect.Stage != "selected" || e.effects.Check(ctx) != nil {
		return errCredentialAdoptionPending
	}
	selected, err := e.effects.Selection(ctx)
	if err != nil || selected.UID != state.AdoptionEffect.AuthorityUID || selected.RegistryUID != state.AdoptionEffect.RegistryUID {
		return errCredentialAdoptionPending
	}
	snapshot, err := e.effects.SealSelection(ctx, selected)
	if err != nil || snapshot.UID != state.AdoptionEffect.RegistryUID {
		return errCredentialAdoptionPending
	}
	reference, found := findAdoptionReference(snapshot, e.intent.Admission.OperationID)
	if found {
		if !matchesAdoptionReference(reference, adoptionOperation(e.intent)) {
			return errCredentialAdoptionPending
		}
		if reference.Status == "pending" {
			next := snapshot.Document.Clone()
			for i := range next.Operations {
				if next.Operations[i].ID == reference.ID {
					next.Operations[i].Status = "delivered"
				}
			}
			if _, err := e.effects.ReplaceSelected(ctx, snapshot, next); err != nil {
				return errCredentialAdoptionPending
			}
		}
	}
	if e.effects.Release(ctx) != nil {
		return errCredentialAdoptionPending
	}
	return nil
}

func sameAdoptionSources(actual, expected securityaudit.CredentialAdoptionAdmission) bool {
	return actual.RootSourceName == expected.RootSourceName && reflect.DeepEqual(actual.Sources, expected.Sources)
}
func nextAdoptionCheckpoint(previous *securityaudit.CredentialAdoptionEffect, stage, registry, authority string) securityaudit.CredentialAdoptionEffect {
	next := *previous
	next.Stage = stage
	next.RegistryUID = registry
	next.AuthorityUID = authority
	return next
}

func credentialAdoptionPending(document operatorcredentials.Document) bool {
	for _, operation := range document.Operations {
		if operation.Action == "adopt" && operation.Status == "pending" {
			return true
		}
	}
	return false
}
