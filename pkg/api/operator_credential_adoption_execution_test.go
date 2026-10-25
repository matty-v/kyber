package api

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/securityaudit"
)

type adoptionExecutionJournal struct {
	state             securityaudit.CredentialOperation
	trace             *[]string
	admitUnknown      bool
	checkpointUnknown string
	finishUnknown     bool
}

func (j *adoptionExecutionJournal) AdmitCredentialOperation(_ context.Context, intent securityaudit.CredentialIntent) (securityaudit.CredentialOperation, error) {
	*j.trace = append(*j.trace, "admit")
	fresh := j.state.Intent.Admission.OperationID == ""
	if fresh {
		j.state.Intent = intent
	} else if !reflect.DeepEqual(j.state.Intent, intent) {
		return securityaudit.CredentialOperation{}, securityaudit.ErrConflict
	}
	result := j.state
	result.NewlyAdmitted = fresh
	if j.admitUnknown {
		return securityaudit.CredentialOperation{}, securityaudit.ErrUnavailable
	}
	return result, nil
}
func (j *adoptionExecutionJournal) GetCredentialOperation(context.Context, string, string) (securityaudit.CredentialOperation, error) {
	if j.state.Intent.Admission.OperationID == "" {
		return securityaudit.CredentialOperation{}, securityaudit.ErrOperationNotFound
	}
	return j.state, nil
}
func (j *adoptionExecutionJournal) PendingCredentialOperations(context.Context, string, string, int) ([]securityaudit.CredentialOperation, error) {
	return nil, nil
}
func (j *adoptionExecutionJournal) PendingCredentialAdoptionOperations(context.Context, string, string, int) ([]securityaudit.CredentialOperation, error) {
	return []securityaudit.CredentialOperation{j.state}, nil
}
func (j *adoptionExecutionJournal) FinishCredentialOperation(context.Context, string, string, string, time.Time) (securityaudit.Event, error) {
	return securityaudit.Event{}, securityaudit.ErrInvalid
}
func (j *adoptionExecutionJournal) RecordCredentialAdoptionEffect(_ context.Context, _, _ string, effect securityaudit.CredentialAdoptionEffect) (securityaudit.CredentialOperation, error) {
	if effect.Validate() != nil || j.state.Completion != nil {
		return securityaudit.CredentialOperation{}, securityaudit.ErrConflict
	}
	*j.trace = append(*j.trace, "checkpoint:"+effect.Stage)
	copy := effect
	j.state.AdoptionEffect = &copy
	if j.checkpointUnknown == effect.Stage {
		return securityaudit.CredentialOperation{}, securityaudit.ErrUnavailable
	}
	return j.state, nil
}
func (j *adoptionExecutionJournal) FinishCredentialAdoptionOperation(_ context.Context, _, _, outcome string, at time.Time, expected *securityaudit.CredentialAdoptionEffect) (securityaudit.Event, error) {
	if !reflect.DeepEqual(j.state.AdoptionEffect, expected) || outcome == "failure" && expected != nil && expected.Stage != "reserved" || outcome == "success" && (expected == nil || expected.Stage != "selected") {
		return securityaudit.Event{}, securityaudit.ErrConflict
	}
	*j.trace = append(*j.trace, "finish:"+outcome)
	event := j.state.Intent.Admission
	event.ID = j.state.Intent.CompletionEventID
	event.Phase = "completion"
	event.Outcome = outcome
	event.ObservedAt = at
	j.state.Completion = &event
	if j.finishUnknown {
		return securityaudit.Event{}, securityaudit.ErrUnavailable
	}
	return event, nil
}

type adoptionExecutionEffects struct {
	reservation                                            securityaudit.CredentialAdoptionEffect
	t                                                      *testing.T
	journal                                                *adoptionExecutionJournal
	candidate                                              *operatorcredentials.Snapshot
	selected                                               *operatorcredentials.AuthoritySelection
	sealed                                                 bool
	released                                               bool
	createUnknown, bindUnknown, ackUnknown, releaseUnknown bool
	checkErr                                               error
	creates, binds, seals, acks, releases                  int
}

func (f *adoptionExecutionEffects) ReservationMetadata(context.Context) (securityaudit.CredentialAdoptionEffect, error) {
	return f.reservation, nil
}
func (f *adoptionExecutionEffects) Check(context.Context) error {
	if f.released {
		return operatorcredentials.ErrConflict
	}
	return f.checkErr
}
func (f *adoptionExecutionEffects) Candidate(context.Context) (operatorcredentials.Snapshot, error) {
	if f.candidate == nil {
		return operatorcredentials.Snapshot{}, operatorcredentials.ErrUninitialized
	}
	return *f.candidate, nil
}
func (f *adoptionExecutionEffects) CreateCandidate(_ context.Context, doc operatorcredentials.Document) (operatorcredentials.Snapshot, error) {
	f.t.Helper()
	if f.journal.state.AdoptionEffect == nil || f.journal.state.AdoptionEffect.Stage != "candidate" {
		f.t.Fatal("create preceded acknowledged effect admission")
	}
	f.creates++
	*f.journal.trace = append(*f.journal.trace, "create")
	f.candidate = &operatorcredentials.Snapshot{UID: uuid.NewString(), Version: "candidate-version", Document: doc.Clone()}
	if f.createUnknown {
		return operatorcredentials.Snapshot{}, operatorcredentials.ErrUnavailable
	}
	return *f.candidate, nil
}
func (f *adoptionExecutionEffects) Selection(context.Context) (operatorcredentials.AuthoritySelection, error) {
	if f.selected == nil {
		return operatorcredentials.AuthoritySelection{}, operatorcredentials.ErrUninitialized
	}
	return *f.selected, nil
}
func (f *adoptionExecutionEffects) BindCandidate(_ context.Context, version string) error {
	f.t.Helper()
	if f.journal.state.AdoptionEffect == nil || f.journal.state.AdoptionEffect.Stage != "selection" || f.candidate == nil || f.candidate.Version != version {
		f.t.Fatal("selection preceded its exact candidate/effect admission")
	}
	f.binds++
	*f.journal.trace = append(*f.journal.trace, "bind")
	f.selected = &operatorcredentials.AuthoritySelection{UID: uuid.NewString(), RegistryUID: f.candidate.UID}
	if f.bindUnknown {
		return operatorcredentials.ErrUnavailable
	}
	return nil
}
func (f *adoptionExecutionEffects) SealSelection(_ context.Context, selection operatorcredentials.AuthoritySelection) (operatorcredentials.Snapshot, error) {
	f.t.Helper()
	if f.selected == nil || selection != *f.selected || f.candidate == nil {
		f.t.Fatal("sealing unrelated authority")
	}
	if !f.sealed {
		f.seals++
		*f.journal.trace = append(*f.journal.trace, "seal")
		f.sealed = true
		f.candidate.Version = "sealed-version"
	}
	return *f.candidate, nil
}
func (f *adoptionExecutionEffects) LoadSelected(context.Context) (operatorcredentials.Snapshot, error) {
	if f.selected == nil {
		return operatorcredentials.Snapshot{}, operatorcredentials.ErrUninitialized
	}
	return *f.candidate, nil
}
func (f *adoptionExecutionEffects) ReplaceSelected(_ context.Context, previous operatorcredentials.Snapshot, next operatorcredentials.Document) (operatorcredentials.Snapshot, error) {
	f.t.Helper()
	if f.journal.state.Completion == nil || f.journal.state.Completion.Outcome != "success" || !f.sealed || previous.UID != f.candidate.UID || previous.Version != f.candidate.Version {
		f.t.Fatal("outbox acknowledged before durable completed authority")
	}
	f.acks++
	*f.journal.trace = append(*f.journal.trace, "ack")
	f.candidate.Document = next.Clone()
	f.candidate.Version = "ack-version"
	if f.ackUnknown {
		return operatorcredentials.Snapshot{}, operatorcredentials.ErrUnavailable
	}
	return *f.candidate, nil
}
func (f *adoptionExecutionEffects) Release(context.Context) error {
	f.t.Helper()
	if f.journal.state.Completion == nil {
		f.t.Fatal("maintenance released without durable finality")
	}
	f.releases++
	*f.journal.trace = append(*f.journal.trace, "release")
	if f.releaseUnknown {
		return operatorcredentials.ErrUnavailable
	}
	f.released = true
	return nil
}
func adoptionExecutionFixture(t *testing.T) (*credentialAdoptionExecution, *adoptionExecutionJournal, *adoptionExecutionEffects, *credentialAdoptionSources, *int) {
	t.Helper()
	reader, _ := credentialSourcesFixture(t)
	source, err := reader.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	operation := adoptionFixtureOperation()
	metadata := source.admissionMetadata()
	metadata.DeploymentName = "kyber-test-control-plane"
	metadata.DeploymentUID = uuid.NewString()
	metadata.ReleaseName = "kyber-test"
	metadata.ControlPlaneImage = "ghcr.io/matty-v/kyber-control-plane:1.9.0"
	rootVersion := ""
	for key, version := range source.versions {
		if key.Name == source.rootName {
			rootVersion = version.version
		}
	}
	intent := securityaudit.CredentialIntent{Admission: securityaudit.Event{ID: operation.AdmissionEventID, OperationID: operation.ID, ObservedAt: operation.At.UTC().Truncate(time.Microsecond), Namespace: source.namespace, PrincipalID: operation.ActorPrincipalID, CredentialID: operation.ActorCredentialID, Generation: 1, RequestID: operation.RequestID, Action: "credential:adopt", ResourceKind: "credential-adoption", ResourceName: operation.ID, Phase: "authorization", Outcome: "allow", CredentialAuthority: "configuration"}, CompletionEventID: operation.CompletionEventID, ExpectedVersion: rootVersion, Adoption: &metadata}
	if intent.Validate() != nil {
		t.Fatal("invalid fixture admission")
	}
	trace := []string{}
	journal := &adoptionExecutionJournal{trace: &trace}
	effects := &adoptionExecutionEffects{t: t, journal: journal, reservation: securityaudit.CredentialAdoptionEffect{Stage: "reserved", MaintenanceUID: uuid.NewString(), AdmissionVersion: "original-reservation-version"}}
	if _, err := journal.AdmitCredentialOperation(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	calls := new(int)
	execution := &credentialAdoptionExecution{intent: intent, journal: journal, effects: effects, preflight: func(context.Context) (*credentialAdoptionSources, error) { *calls++; return source, nil }}
	return execution, journal, effects, source, calls
}
func TestAdoptionExecutionOrdersEffectsPreservesKeysAndCompletes(t *testing.T) {
	execution, journal, effects, source, calls := adoptionExecutionFixture(t)
	if err := execution.run(context.Background()); err != nil {
		t.Fatal("handoff failed", err)
	}
	if effects.creates != 1 || effects.binds != 1 || effects.seals != 1 || effects.acks != 1 || effects.releases != 1 || !effects.released || *calls != 2 {
		t.Fatal("handoff repeated effects or skipped final preflight")
	}
	expected := []string{"admit", "checkpoint:reserved", "checkpoint:candidate", "create", "checkpoint:registry", "checkpoint:selection", "bind", "seal", "checkpoint:selected", "finish:success", "ack", "release"}
	if !reflect.DeepEqual(*journal.trace, expected) {
		t.Fatal("effect/audit ordering changed")
	}
	configured := NewAPIKeyAuthenticator(source.root, source.callers...)
	registry, err := compileRegistryCredentials(effects.candidate.Document, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{source.root, source.callers[0].Key, source.callers[1].Key} {
		before, err := configured.authenticateConfigured(adoptionBearer(value))
		if err != nil {
			t.Fatal(err)
		}
		after, err := registry.authenticate(adoptionBearer(value))
		if err != nil || !sameCredentialIdentity(*before, *after) || !reflect.DeepEqual(before.Scopes, after.Scopes) || !reflect.DeepEqual(before.AgentResources, after.AgentResources) {
			t.Fatal("handoff changed accepted material/identity/grants")
		}
	}
}
func TestAdoptionExecutionObservesLostCreateAndBindWithoutReplay(t *testing.T) {
	execution, _, effects, _, _ := adoptionExecutionFixture(t)
	effects.createUnknown = true
	effects.bindUnknown = true
	if err := execution.run(context.Background()); err != nil || effects.creates != 1 || effects.binds != 1 || !effects.released {
		t.Fatal("committed unknown create/bind was replayed or not reconciled", err)
	}
}
func TestAdoptionExecutionRequiresDurableOriginalLaunchAdmission(t *testing.T) {
	execution, journal, effects, _, _ := adoptionExecutionFixture(t)
	journal.state = securityaudit.CredentialOperation{}
	if !errors.Is(execution.run(context.Background()), errCredentialAdoptionPending) || effects.creates != 0 || effects.releases != 0 {
		t.Fatal("unjournaled worker performed effects")
	}
	if _, err := journal.AdmitCredentialOperation(context.Background(), execution.intent); err != nil {
		t.Fatal(err)
	}
	if err := execution.run(context.Background()); err != nil || effects.creates != 1 {
		t.Fatal("original durable operation did not complete", err)
	}
}

func TestAdoptionExecutionUnknownCheckpointNeverStartsAnEffectWithoutAck(t *testing.T) {
	execution, journal, effects, _, _ := adoptionExecutionFixture(t)
	journal.checkpointUnknown = "candidate"
	if !errors.Is(execution.run(context.Background()), errCredentialAdoptionPending) || effects.creates != 0 || effects.releases != 0 {
		t.Fatal("unknown checkpoint authorized create or release")
	}
	journal.checkpointUnknown = ""
	if err := execution.run(context.Background()); err != nil || effects.creates != 1 {
		t.Fatal("original admitted candidate did not recover", err)
	}
}
func TestAdoptionExecutionUnknownAuditAndAckRecoverWithoutRepeatingKeys(t *testing.T) {
	for _, kind := range []string{"audit", "ack", "release"} {
		t.Run(kind, func(t *testing.T) {
			execution, journal, effects, _, _ := adoptionExecutionFixture(t)
			switch kind {
			case "audit":
				journal.finishUnknown = true
			case "ack":
				effects.ackUnknown = true
			case "release":
				effects.releaseUnknown = true
			}
			if !errors.Is(execution.run(context.Background()), errCredentialAdoptionPending) {
				t.Fatal("unknown completion presented as complete")
			}
			journal.finishUnknown = false
			effects.ackUnknown = false
			effects.releaseUnknown = false
			if err := execution.run(context.Background()); err != nil || effects.creates != 1 || effects.binds != 1 || effects.acks != 1 {
				t.Fatal("recovery recreated keys, rebound selection or repeated committed ack", err)
			}
		})
	}
}
func TestAdoptionExecutionRejectsChangedInputsOnlyBeforeAnyEffect(t *testing.T) {
	execution, journal, effects, source, _ := adoptionExecutionFixture(t)
	source.rootName = "different-root"
	if !errors.Is(execution.run(context.Background()), errCredentialAdoptionRejected) || effects.creates != 0 || effects.binds != 0 || !effects.released || journal.state.Completion == nil || journal.state.Completion.Outcome != "failure" {
		t.Fatal("changed root graph accepted or lacked safe terminal outcome")
	}
}
