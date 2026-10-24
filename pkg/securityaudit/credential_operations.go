package securityaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// CredentialIntent contains only recovery metadata, never a key, request body,
// cookie, hash of key material or a serialized candidate. ExpectedVersion is an
// opaque Kubernetes CAS version; RegistryUID pins the selected incarnation.
// Admission and intent commit together before any credential effect is allowed.
type CredentialIntent struct {
	Admission         Event  `json:"admission"`
	CompletionEventID string `json:"completionEventId"`
	ExpectedVersion   string `json:"expectedVersion"`
	RegistryUID       string `json:"registryUid"`
}

var ErrOperationNotFound = errors.New("credential operation not found")

// CredentialJournal is optional to ordinary request authentication. Credential
// management requires it; ordinary reads never gain a PostgreSQL dependency.
type CredentialJournal interface {
	AdmitCredentialOperation(context.Context, CredentialIntent) (CredentialOperation, error)
	GetCredentialOperation(context.Context, string, string) (CredentialOperation, error)
	PendingCredentialOperations(context.Context, string, string, int) ([]CredentialOperation, error)
	FinishCredentialOperation(context.Context, string, string, string, time.Time) (Event, error)
}

var _ CredentialJournal = (*PostgresStore)(nil)

type CredentialOperation struct {
	Intent     CredentialIntent
	Completion *Event
}

func (i CredentialIntent) Validate() error {
	e := i.Admission
	if e.Validate() != nil || e.Phase != "authorization" || e.Outcome != "allow" || e.CredentialAuthority != "registry" || e.ResourceKind != "credential" {
		return ErrInvalid
	}
	switch e.Action {
	case "credential:create", "credential:update", "credential:rotate", "credential:revoke":
	default:
		return ErrInvalid
	}
	if _, err := uuid.Parse(i.CompletionEventID); err != nil || i.CompletionEventID == e.ID || !validActorID(i.ExpectedVersion) || !validActorID(i.RegistryUID) {
		return ErrInvalid
	}
	return nil
}

// AdmitCredentialOperation is immutable and idempotent for identical intent.
// A lost response is reconciled by reading the operation, never by assuming the
// key write failed or resubmitting it with a refreshed resource version.
func (s *PostgresStore) AdmitCredentialOperation(ctx context.Context, intent CredentialIntent) (CredentialOperation, error) {
	intent.Admission = intent.Admission.normalized()
	if err := intent.Validate(); err != nil {
		return CredentialOperation{}, err
	}
	payload, err := json.Marshal(intent)
	if err != nil {
		return CredentialOperation{}, ErrInvalid
	}
	eventPayload, err := json.Marshal(intent.Admission)
	if err != nil {
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
	var same bool
	var completion []byte
	err = tx.QueryRowContext(ctx, `SELECT intent=$3::jsonb,completion FROM operator_credential_operations WHERE namespace=$1 AND operation_id=$2`, intent.Admission.Namespace, intent.Admission.OperationID, payload).Scan(&same, &completion)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = appendEventInTransaction(ctx, tx, intent.Admission, eventPayload); err != nil {
			return CredentialOperation{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO operator_credential_operations(namespace,operation_id,intent) VALUES($1,$2,$3)`, intent.Admission.Namespace, intent.Admission.OperationID, payload)
		if err != nil {
			return CredentialOperation{}, databaseError(ctx, err)
		}
	} else if err != nil {
		return CredentialOperation{}, databaseError(ctx, err)
	} else if !same {
		return CredentialOperation{}, ErrConflict
	}
	completionEvent, err := decodeCredentialCompletion(intent, completion)
	if err != nil {
		return CredentialOperation{}, err
	}
	result := CredentialOperation{Intent: intent, Completion: completionEvent}
	if err = tx.Commit(); err != nil {
		return CredentialOperation{}, databaseError(ctx, err)
	}
	return result, nil
}

// GetCredentialOperation resolves a lost response without accepting a new
// candidate or requiring a client to reconstruct private admission metadata.
func (s *PostgresStore) GetCredentialOperation(ctx context.Context, namespace, operationID string) (CredentialOperation, error) {
	if !namespaceName.MatchString(namespace) {
		return CredentialOperation{}, ErrInvalid
	}
	if _, err := uuid.Parse(operationID); err != nil {
		return CredentialOperation{}, ErrInvalid
	}
	var raw, final []byte
	err := s.db.QueryRowContext(ctx, `SELECT intent,completion FROM operator_credential_operations WHERE namespace=$1 AND operation_id=$2`, namespace, operationID).Scan(&raw, &final)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialOperation{}, ErrOperationNotFound
	}
	if err != nil {
		return CredentialOperation{}, databaseError(ctx, err)
	}
	var intent CredentialIntent
	if json.Unmarshal(raw, &intent) != nil || intent.Validate() != nil || intent.Admission.Namespace != namespace || intent.Admission.OperationID != operationID {
		return CredentialOperation{}, ErrUnavailable
	}
	completion, err := decodeCredentialCompletion(intent, final)
	if err != nil {
		return CredentialOperation{}, err
	}
	return CredentialOperation{Intent: intent, Completion: completion}, nil
}

// PendingCredentialOperations returns bounded recovery work. Pending intents
// survive ordinary history retention; they are not expired based on age. Pass
// the last returned operation ID to continue past unresolved earlier work.
// This is a live scan, not a frozen inventory; restart it periodically.
func (s *PostgresStore) PendingCredentialOperations(ctx context.Context, namespace, afterOperationID string, limit int) ([]CredentialOperation, error) {
	if !namespaceName.MatchString(namespace) || limit < 1 || limit > 200 {
		return nil, ErrInvalid
	}
	if afterOperationID != "" {
		if _, err := uuid.Parse(afterOperationID); err != nil {
			return nil, ErrInvalid
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT intent FROM operator_credential_operations WHERE namespace=$1 AND completion IS NULL AND ($3='' OR operation_id > NULLIF($3,'')::uuid) ORDER BY operation_id LIMIT $2`, namespace, limit, afterOperationID)
	if err != nil {
		return nil, databaseError(ctx, err)
	}
	defer rows.Close()
	result := []CredentialOperation{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, databaseError(ctx, err)
		}
		var intent CredentialIntent
		if json.Unmarshal(raw, &intent) != nil || intent.Validate() != nil || intent.Admission.Namespace != namespace {
			return nil, ErrUnavailable
		}
		result = append(result, CredentialOperation{Intent: intent})
	}
	if err = rows.Err(); err != nil {
		return nil, databaseError(ctx, err)
	}
	return result, nil
}

// FinishCredentialOperation records a definitive reconciled outcome exactly
// once with its stable event ID. "unknown" stays pending: a delayed CAS must be
// fenced or observed committed before failure/success can become final.
// The caller, not this metadata journal, proves that outcome against authority.
func (s *PostgresStore) FinishCredentialOperation(ctx context.Context, namespace, operationID, outcome string, observedAt time.Time) (Event, error) {
	if !namespaceName.MatchString(namespace) || (outcome != "success" && outcome != "failure") || observedAt.IsZero() {
		return Event{}, ErrInvalid
	}
	if _, err := uuid.Parse(operationID); err != nil {
		return Event{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, databaseError(ctx, err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, commitLock); err != nil {
		return Event{}, databaseError(ctx, err)
	}
	var raw, final []byte
	err = tx.QueryRowContext(ctx, `SELECT intent,completion FROM operator_credential_operations WHERE namespace=$1 AND operation_id=$2 FOR UPDATE`, namespace, operationID).Scan(&raw, &final)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrConflict
	}
	if err != nil {
		return Event{}, databaseError(ctx, err)
	}
	var intent CredentialIntent
	if json.Unmarshal(raw, &intent) != nil || intent.Validate() != nil || intent.Admission.Namespace != namespace || intent.Admission.OperationID != operationID {
		return Event{}, ErrUnavailable
	}
	event := intent.Admission
	event.ID = intent.CompletionEventID
	event.Phase = "completion"
	event.Outcome = outcome
	event.ObservedAt = observedAt
	event = event.normalized()
	if len(final) > 0 {
		previous, err := decodeCredentialCompletion(intent, final)
		if err != nil {
			return Event{}, err
		}
		if previous.Outcome != outcome {
			return Event{}, ErrConflict
		}
		// Return the original committed timestamp, not the retry's clock.
		// No second completion is appended after ordinary history retention.
		return *previous, nil
	}
	if event.Validate() != nil {
		return Event{}, ErrInvalid
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return Event{}, ErrInvalid
	}
	if _, err = appendEventInTransaction(ctx, tx, event, payload); err != nil {
		return Event{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE operator_credential_operations SET completion=$3 WHERE namespace=$1 AND operation_id=$2`, namespace, operationID, payload); err != nil {
		return Event{}, databaseError(ctx, err)
	}
	if err = tx.Commit(); err != nil {
		return Event{}, databaseError(ctx, err)
	}
	return event, nil
}

func decodeCredentialCompletion(intent CredentialIntent, raw []byte) (*Event, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var event Event
	if json.Unmarshal(raw, &event) != nil || event.Validate() != nil || (event.Outcome != "success" && event.Outcome != "failure") {
		return nil, ErrUnavailable
	}
	event = event.normalized()
	expected := intent.Admission.normalized()
	expected.ID = intent.CompletionEventID
	expected.Phase = "completion"
	expected.Outcome = event.Outcome
	expected.ObservedAt = event.ObservedAt
	if event != expected {
		return nil, ErrUnavailable
	}
	return &event, nil
}
