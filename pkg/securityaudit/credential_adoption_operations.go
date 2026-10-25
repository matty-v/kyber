package securityaudit

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strings"
	"time"

	validation "k8s.io/apimachinery/pkg/util/validation"
)

// Source versions contain object metadata only. The pinned root document also
// pins caller definition ordering and reference keys; neither is copied here.
type CredentialAdoptionSource struct {
	Name    string `json:"name"`
	UID     string `json:"uid"`
	Version string `json:"version"`
}
type CredentialAdoptionAdmission struct {
	DeploymentName    string                     `json:"deploymentName"`
	DeploymentUID     string                     `json:"deploymentUid"`
	ReleaseName       string                     `json:"releaseName"`
	ControlPlaneImage string                     `json:"controlPlaneImage"`
	RootSourceName    string                     `json:"rootSourceName"`
	Sources           []CredentialAdoptionSource `json:"sources"`
}

func (i CredentialIntent) validateAdoption() error {
	e := i.Admission
	if e.Action != "credential:adopt" || e.CredentialAuthority != "configuration" || i.RegistryUID != "" || i.MaintenanceUID != "" || e.ResourceName != e.OperationID || !validMaintenanceUID(e.ID) || !validMaintenanceUID(e.OperationID) || !validMaintenanceUID(i.CompletionEventID) || i.Adoption == nil {
		return ErrInvalid
	}
	a := i.Adoption
	if len(validation.IsDNS1123Subdomain(a.DeploymentName)) != 0 || !validMaintenanceUID(a.DeploymentUID) || len(validation.IsDNS1123Subdomain(a.ReleaseName)) != 0 || !strings.HasPrefix(a.ControlPlaneImage, "ghcr.io/matty-v/kyber-control-plane:") || len(a.ControlPlaneImage) > 256 || !adoptionImageTag.MatchString(strings.TrimPrefix(a.ControlPlaneImage, "ghcr.io/matty-v/kyber-control-plane:")) {
		return ErrInvalid
	}
	if len(validation.IsDNS1123Subdomain(a.RootSourceName)) != 0 || len(a.Sources) < 1 || len(a.Sources) > 1025 {
		return ErrInvalid
	}
	root := false
	previous := ""
	uids := map[string]bool{}
	for _, source := range a.Sources {
		if len(validation.IsDNS1123Subdomain(source.Name)) != 0 || source.Name <= previous || !validMaintenanceUID(source.UID) || !validActorID(source.Version) || uids[source.UID] {
			return ErrInvalid
		}
		previous = source.Name
		uids[source.UID] = true
		if source.Name == a.RootSourceName {
			root = true
			if i.ExpectedVersion != source.Version {
				return ErrInvalid
			}
		}
	}
	if !root {
		return ErrInvalid
	}
	return nil
}

var adoptionImageTag = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// A checkpoint admits the next possible effect or records its observed UID.
// An admitted create can remain pending forever; absence alone cannot fence a
// delayed Kubernetes create and therefore cannot justify final failure.
type CredentialAdoptionEffect struct {
	MaintenanceUID   string `json:"maintenanceUid"`
	AdmissionVersion string `json:"admissionVersion"`
	Stage            string `json:"stage"` // reserved, candidate, registry, selection, selected
	RegistryUID      string `json:"registryUid,omitempty"`
	AuthorityUID     string `json:"authorityUid,omitempty"`
}

func (e CredentialAdoptionEffect) Validate() error {
	if !validMaintenanceUID(e.MaintenanceUID) || !validActorID(e.AdmissionVersion) {
		return ErrInvalid
	}
	switch e.Stage {
	case "reserved", "candidate":
		if e.RegistryUID == "" && e.AuthorityUID == "" {
			return nil
		}
	case "registry", "selection":
		if validMaintenanceUID(e.RegistryUID) && e.AuthorityUID == "" {
			return nil
		}
	case "selected":
		if validMaintenanceUID(e.RegistryUID) && validMaintenanceUID(e.AuthorityUID) {
			return nil
		}
	}
	return ErrInvalid
}
func validAdoptionTransition(previous *CredentialAdoptionEffect, next CredentialAdoptionEffect) bool {
	if next.Validate() != nil {
		return false
	}
	if previous == nil {
		return next.Stage == "reserved"
	}
	if previous.MaintenanceUID != next.MaintenanceUID || previous.AdmissionVersion != next.AdmissionVersion {
		return false
	}
	if *previous == next {
		return true
	}
	switch previous.Stage {
	case "reserved":
		return next.Stage == "candidate"
	case "candidate":
		return next.Stage == "registry"
	case "registry":
		return next.Stage == "selection" && previous.RegistryUID == next.RegistryUID
	case "selection":
		return next.Stage == "selected" && previous.RegistryUID == next.RegistryUID
	}
	return false
}
func decodeCredentialEffects(intent CredentialIntent, raw []byte) (*CredentialMaintenanceEffect, *CredentialAdoptionEffect, error) {
	if intent.Admission.ResourceKind != "credential-adoption" {
		effect, err := decodeCredentialMaintenanceEffect(intent, raw)
		return effect, nil, err
	}
	if len(raw) == 0 {
		return nil, nil, nil
	}
	if len(raw) > 4096 {
		return nil, nil, ErrUnavailable
	}
	var effect CredentialAdoptionEffect
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&effect) != nil || effect.Validate() != nil {
		return nil, nil, ErrUnavailable
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, nil, ErrUnavailable
	}
	return nil, &effect, nil
}

type CredentialAdoptionJournal interface {
	CredentialJournal
	RecordCredentialAdoptionEffect(context.Context, string, string, CredentialAdoptionEffect) (CredentialOperation, error)
	PendingCredentialAdoptionOperations(context.Context, string, string, int) ([]CredentialOperation, error)
	FinishCredentialAdoptionOperation(context.Context, string, string, string, time.Time, *CredentialAdoptionEffect) (Event, error)
}

var _ CredentialAdoptionJournal = (*PostgresStore)(nil)

func (s *PostgresStore) PendingCredentialAdoptionOperations(ctx context.Context, namespace, after string, limit int) ([]CredentialOperation, error) {
	return s.pendingCredentialOperations(ctx, namespace, after, limit, "credential-adoption")
}
func (s *PostgresStore) FinishCredentialAdoptionOperation(ctx context.Context, namespace, operationID, outcome string, at time.Time, expected *CredentialAdoptionEffect) (Event, error) {
	return s.finishCredentialOperation(ctx, namespace, operationID, outcome, at, "credential-adoption", nil, expected)
}

func (s *PostgresStore) RecordCredentialAdoptionEffect(ctx context.Context, namespace, operationID string, effect CredentialAdoptionEffect) (CredentialOperation, error) {
	if !namespaceName.MatchString(namespace) || !validMaintenanceUID(operationID) || effect.Validate() != nil {
		return CredentialOperation{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CredentialOperation{}, databaseError(ctx, err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, commitLock); err != nil {
		return CredentialOperation{}, databaseError(ctx, err)
	}
	var raw, completion, previous []byte
	err = tx.QueryRowContext(ctx, `SELECT intent,completion,effect FROM operator_credential_operations WHERE namespace=$1 AND operation_id=$2 FOR UPDATE`, namespace, operationID).Scan(&raw, &completion, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialOperation{}, ErrOperationNotFound
	}
	if err != nil {
		return CredentialOperation{}, databaseError(ctx, err)
	}
	var intent CredentialIntent
	if json.Unmarshal(raw, &intent) != nil || intent.Validate() != nil || intent.Admission.Namespace != namespace || intent.Admission.OperationID != operationID || intent.Admission.ResourceKind != "credential-adoption" {
		return CredentialOperation{}, ErrInvalid
	}
	_, old, err := decodeCredentialEffects(intent, previous)
	if err != nil {
		return CredentialOperation{}, err
	}
	if !validAdoptionTransition(old, effect) || len(completion) > 0 && !reflect.DeepEqual(old, &effect) {
		return CredentialOperation{}, ErrConflict
	}
	if old == nil || *old != effect {
		payload, err := json.Marshal(effect)
		if err != nil {
			return CredentialOperation{}, ErrInvalid
		}
		if _, err = tx.ExecContext(ctx, `UPDATE operator_credential_operations SET effect=$3 WHERE namespace=$1 AND operation_id=$2`, namespace, operationID, payload); err != nil {
			return CredentialOperation{}, databaseError(ctx, err)
		}
	}
	final, err := decodeCredentialCompletion(intent, completion)
	if err != nil {
		return CredentialOperation{}, err
	}
	if err = tx.Commit(); err != nil {
		return CredentialOperation{}, databaseError(ctx, err)
	}
	return CredentialOperation{Intent: intent, Completion: final, AdoptionEffect: &effect}, nil
}
