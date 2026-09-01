package taskstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func validateCancel(l Limits, p CancelParams) error {
	if p.TaskID == "" || p.Agent.Namespace == "" || p.Agent.Name == "" || p.RequestedBy == "" {
		return ErrInvalid
	}
	if len([]byte(p.IdempotencyKey)) > HardMaxIdempotencyBytes {
		return ErrIdempotencyTooLarge
	}
	if p.IdempotencyKey != "" && p.RequestHash == "" {
		return ErrInvalid
	}
	if len([]byte(p.Reason)) > l.MaxCancelReasonBytes {
		return ErrCancelReasonTooLarge
	}
	return nil
}

func (s *PostgresStore) Cancel(ctx context.Context, p CancelParams) (*CancelResult, error) {
	if err := validateCancel(s.limits, p); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if p.IdempotencyKey != "" {
		// Serialize first use of a key before checking it. Locking only the task
		// row is insufficient because the same scoped key can race on two tasks,
		// and a same-task loser would otherwise surface a unique violation instead
		// of the canonical replay/conflict response.
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(concat_ws(chr(10),$1::text,$2::text,$3::text,$4::text),0))`, p.RequestedBy, p.Agent.Namespace, p.Agent.Name, p.IdempotencyKey); err != nil {
			return nil, err
		}
		var hash, taskID string
		var applied bool
		err = tx.QueryRowContext(ctx, `SELECT request_hash,task_id,applied FROM agent_task_cancel_idempotency WHERE created_by=$1 AND agent_namespace=$2 AND agent_name=$3 AND idempotency_key=$4`, p.RequestedBy, p.Agent.Namespace, p.Agent.Name, p.IdempotencyKey).Scan(&hash, &taskID, &applied)
		if err == nil {
			if hash != p.RequestHash {
				return nil, ErrIdempotencyConflict
			}
			t, err := getTaskTx(ctx, tx, p.Agent, taskID)
			if err != nil {
				return nil, err
			}
			if err = tx.Commit(); err != nil {
				return nil, err
			}
			return &CancelResult{Task: t, Applied: applied, Replay: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	t, err := scanTask(tx.QueryRowContext(ctx, selectTask+` WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3 FOR UPDATE`, p.TaskID, p.Agent.Namespace, p.Agent.Name))
	if err != nil {
		return nil, err
	}
	if t.CreatedBy != p.RequestedBy {
		return nil, ErrNotFound
	}
	result := &CancelResult{Task: t}
	if t.Cancellation != nil || t.State == StateCanceling || t.State == StateCanceled {
		result.Replay = true
	} else if t.State == StateQueued || t.State == StateDispatched || t.State == StateInputRequired || t.State == StateAuthRequired {
		var dispatchStatus, attemptID string
		if err = tx.QueryRowContext(ctx, `SELECT status,attempt_token FROM agent_task_dispatches WHERE task_id=$1 FOR UPDATE`, t.ID).Scan(&dispatchStatus, &attemptID); err != nil {
			return nil, err
		}
		var now time.Time
		if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return nil, err
		}
		deadline := now.Add(s.limits.DefaultCancelDeadline)
		maxDeadline := now.Add(s.limits.MaxCancelDeadline)
		if deadline.After(maxDeadline) {
			deadline = maxDeadline
		}
		if deadline.After(t.DeadlineAt) {
			deadline = t.DeadlineAt
		}
		ambiguous := t.State == StateDispatched || dispatchStatus == "attempting" || dispatchStatus == "receipt_pending" || (dispatchStatus == "delivered" && t.State != StateInputRequired && t.State != StateAuthRequired)
		if ambiguous {
			if attemptID == "" {
				return nil, ErrConflict
			}
			_, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET state='canceling',cancel_requested_at=$2,cancel_requested_by=$3,cancel_reason=$4,cancel_deadline_at=$5,version=version+1,updated_at=$2 WHERE id=$1`, t.ID, now, p.RequestedBy, p.Reason, deadline)
			if err == nil {
				_, err = tx.ExecContext(ctx, `INSERT INTO agent_task_cancel_deliveries(task_id,attempt_id,status,adapter_mode,next_delivery_at,updated_at) VALUES($1,$2,'pending','notify_only',$3,$3) ON CONFLICT (task_id) DO NOTHING`, t.ID, attemptID, now)
			}
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET state='canceled',cancel_requested_at=$2,cancel_requested_by=$3,cancel_reason=$4,cancel_deadline_at=$5,version=version+1,updated_at=$2,completed_at=$2 WHERE id=$1`, t.ID, now, p.RequestedBy, p.Reason, deadline)
			if err == nil {
				_, err = tx.ExecContext(ctx, `UPDATE agent_task_dispatches SET status='closed',last_error_code='canceled',lease_owner=NULL,lease_until=NULL,updated_at=$2 WHERE task_id=$1`, t.ID, now)
			}
		}
		if err != nil {
			return nil, err
		}
		result.Applied = true
	}
	if p.IdempotencyKey != "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO agent_task_cancel_idempotency(created_by,agent_namespace,agent_name,idempotency_key,request_hash,task_id,applied) VALUES($1,$2,$3,$4,$5,$6,$7)`, p.RequestedBy, p.Agent.Namespace, p.Agent.Name, p.IdempotencyKey, p.RequestHash, t.ID, result.Applied)
		if err != nil {
			return nil, err
		}
	}
	result.Task, err = getTaskTx(ctx, tx, p.Agent, t.ID)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *PostgresStore) GetControl(ctx context.Context, a AgentRef, id, attemptID string) (*TaskControl, error) {
	if strings.TrimSpace(attemptID) == "" {
		return nil, ErrInvalidAttempt
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var state State
	var currentAttempt string
	var requestedAt sql.NullTime
	var reason string
	err = tx.QueryRowContext(ctx, `SELECT t.state,d.attempt_token,t.cancel_requested_at,t.cancel_reason FROM agent_tasks t JOIN agent_task_dispatches d ON d.task_id=t.id WHERE t.id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3 FOR UPDATE OF t`, id, a.Namespace, a.Name).Scan(&state, &currentAttempt, &requestedAt, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if currentAttempt == "" || currentAttempt != attemptID {
		return nil, ErrInvalidAttempt
	}
	control := &TaskControl{}
	if state == StateCanceling && requestedAt.Valid {
		control.CancelRequested = true
		control.Reason = reason
		control.RequestedAt = requestedAt.Time.UTC()
		if _, err = tx.ExecContext(ctx, `UPDATE agent_task_cancel_deliveries SET status='notified',delivery_count=delivery_count+1,next_delivery_at=clock_timestamp(),updated_at=clock_timestamp() WHERE task_id=$1 AND attempt_id=$2 AND status IN ('pending','delivering','notified')`, id, attemptID); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return control, nil
}

func (s *PostgresStore) AcknowledgeCancel(ctx context.Context, a AgentRef, id, attemptID, acknowledgmentID, note string) (*Task, bool, error) {
	if id == "" || attemptID == "" || acknowledgmentID == "" || len([]byte(acknowledgmentID)) > HardMaxIdempotencyBytes || len([]byte(note)) > HardMaxCancelReasonBytes {
		return nil, false, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	t, err := scanTask(tx.QueryRowContext(ctx, selectTask+` WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3 FOR UPDATE`, id, a.Namespace, a.Name))
	if err != nil {
		return nil, false, err
	}
	var currentAttempt, priorAck string
	if err = tx.QueryRowContext(ctx, `SELECT attempt_id,acknowledgment_id FROM agent_task_cancel_deliveries WHERE task_id=$1 FOR UPDATE`, id).Scan(&currentAttempt, &priorAck); errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrConflict
	} else if err != nil {
		return nil, false, err
	}
	if currentAttempt != attemptID {
		return nil, false, ErrInvalidAttempt
	}
	if priorAck != "" {
		if priorAck != acknowledgmentID {
			return nil, false, ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return nil, false, err
		}
		return t, true, nil
	}
	if t.State != StateCanceling {
		return nil, false, ErrConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET state='canceled',cancel_acknowledged_at=clock_timestamp(),cancel_ack_source='agent',version=version+1,updated_at=clock_timestamp(),completed_at=clock_timestamp() WHERE id=$1`, id)
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE agent_task_cancel_deliveries SET status='acknowledged',acknowledgment_id=$2,lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp() WHERE task_id=$1`, id, acknowledgmentID)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE agent_task_dispatches SET status='closed',last_error_code='canceled',lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp() WHERE task_id=$1`, id)
	}
	if err != nil {
		return nil, false, err
	}
	t, err = getTaskTx(ctx, tx, a, id)
	if err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return t, false, nil
}
