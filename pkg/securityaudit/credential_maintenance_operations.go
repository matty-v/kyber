package securityaudit

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
)

func validMaintenanceUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

// CredentialMaintenanceEffect is the immutable CAS delete intent committed
// before releasing a fenced maintenance object. It contains no credential data.
type CredentialMaintenanceEffect struct {
	ResourceUID     string `json:"resourceUid"`
	ResourceVersion string `json:"resourceVersion"`
	ProofID         string `json:"proofId"`
}

func (e CredentialMaintenanceEffect) Validate() error {
	if !validMaintenanceUID(e.ResourceUID) || !validActorID(e.ResourceVersion) || !validMaintenanceUID(e.ProofID) {
		return ErrInvalid
	}
	return nil
}

type CredentialMaintenanceJournal interface {
	CredentialJournal
	RecordCredentialMaintenanceEffect(context.Context, string, string, CredentialMaintenanceEffect) (CredentialOperation, error)
	PendingCredentialMaintenanceOperations(context.Context, string, string, int) ([]CredentialOperation, error)
	FinishCredentialMaintenanceOperation(context.Context, string, string, string, time.Time, *CredentialMaintenanceEffect) (Event, error)
}

var _ CredentialMaintenanceJournal = (*PostgresStore)(nil)

func decodeCredentialMaintenanceEffect(intent CredentialIntent, raw []byte) (*CredentialMaintenanceEffect, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if intent.Admission.ResourceKind != "credential-maintenance" || len(raw) > 4096 {
		return nil, ErrUnavailable
	}
	var effect CredentialMaintenanceEffect
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&effect) != nil || effect.Validate() != nil || effect.ResourceUID != intent.MaintenanceUID || effect.ResourceVersion == intent.ExpectedVersion {
		return nil, ErrUnavailable
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, ErrUnavailable
	}
	return &effect, nil
}
func (s *PostgresStore) PendingCredentialMaintenanceOperations(ctx context.Context, namespace, after string, limit int) ([]CredentialOperation, error) {
	return s.pendingCredentialOperations(ctx, namespace, after, limit, "credential-maintenance")
}

// RecordCredentialMaintenanceEffect freezes one delete intent. A changed proof
// is a conflict. Finalization compares the observed checkpoint under the same
// transaction lock, preventing a late effect admission after terminal failure.
func (s *PostgresStore) RecordCredentialMaintenanceEffect(ctx context.Context, namespace, operationID string, effect CredentialMaintenanceEffect) (CredentialOperation, error) {
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
	if json.Unmarshal(raw, &intent) != nil || intent.Validate() != nil || intent.Admission.Namespace != namespace || intent.Admission.OperationID != operationID || intent.Admission.ResourceKind != "credential-maintenance" || effect.ResourceUID != intent.MaintenanceUID || effect.ResourceVersion == intent.ExpectedVersion {
		return CredentialOperation{}, ErrInvalid
	}
	old, err := decodeCredentialMaintenanceEffect(intent, previous)
	if err != nil {
		return CredentialOperation{}, err
	}
	if old != nil {
		if *old != effect {
			return CredentialOperation{}, ErrConflict
		}
	} else {
		if len(completion) > 0 {
			return CredentialOperation{}, ErrConflict
		}
		payload, _ := json.Marshal(effect)
		if _, err = tx.ExecContext(ctx, `UPDATE operator_credential_operations SET effect=$3 WHERE namespace=$1 AND operation_id=$2 AND completion IS NULL`, namespace, operationID, payload); err != nil {
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
	return CredentialOperation{Intent: intent, Completion: final, Effect: &effect}, nil
}
func (s *PostgresStore) FinishCredentialMaintenanceOperation(ctx context.Context, namespace, operationID, outcome string, at time.Time, expectedEffect *CredentialMaintenanceEffect) (Event, error) {
	return s.finishCredentialOperation(ctx, namespace, operationID, outcome, at, "credential-maintenance", expectedEffect, nil)
}
