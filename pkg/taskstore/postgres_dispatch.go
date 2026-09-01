package taskstore

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *PostgresStore) ClaimPending(ctx context.Context, owner string, lease time.Duration) (*DispatchClaim, error) {
	if owner == "" || lease <= 0 {
		return nil, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `SELECT t.id,t.agent_namespace,t.agent_name,t.created_by,t.prompt,t.correlation,t.state,t.failure_code,t.response,t.version,t.created_at,t.updated_at,t.deadline_at,t.retain_until,t.completed_at,d.attempts
		FROM agent_task_dispatches d JOIN agent_tasks t ON t.id=d.task_id
		WHERE d.status='pending' AND t.state='queued' AND d.attempts<$1 AND d.next_attempt_at<=clock_timestamp() AND t.deadline_at>clock_timestamp()
		AND NOT EXISTS (SELECT 1 FROM agent_tasks canceling WHERE canceling.agent_namespace=t.agent_namespace AND canceling.agent_name=t.agent_name AND canceling.state='canceling')
		AND NOT EXISTS (SELECT 1 FROM agent_task_dispatches active JOIN agent_tasks at ON at.id=active.task_id WHERE at.agent_namespace=t.agent_namespace AND at.agent_name=t.agent_name AND active.status IN ('leased','attempting','receipt_pending'))
		ORDER BY t.created_at,t.id FOR UPDATE OF d SKIP LOCKED LIMIT 1`, s.limits.MaxDispatchAttempts)
	t := &Task{}
	var completed sql.NullTime
	var attempts int
	err = row.Scan(&t.ID, &t.AgentNamespace, &t.AgentName, &t.CreatedBy, &t.Prompt, &t.Correlation, &t.State, &t.FailureCode, &t.Response, &t.Version, &t.CreatedAt, &t.UpdatedAt, &t.DeadlineAt, &t.RetainUntil, &completed, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoDispatch
	}
	if err != nil {
		return nil, err
	}
	if completed.Valid {
		v := completed.Time
		t.CompletedAt = &v
	}
	if err = loadConversation(ctx, tx, t); err != nil {
		return nil, err
	}
	// PostgreSQL text parameters cannot contain NUL bytes. Kubernetes namespace
	// and name values cannot contain '/', so it is also an unambiguous lane key.
	lane := t.AgentNamespace + "/" + t.AgentName
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lane); err != nil {
		return nil, err
	}
	var laneBusy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agent_task_dispatches active JOIN agent_tasks at ON at.id=active.task_id WHERE at.agent_namespace=$1 AND at.agent_name=$2 AND active.task_id<>$3 AND active.status IN ('leased','attempting','receipt_pending'))`, t.AgentNamespace, t.AgentName, t.ID).Scan(&laneBusy); err != nil {
		return nil, err
	}
	if laneBusy {
		return nil, ErrNoDispatch
	}
	var leaseUntil time.Time
	if err = tx.QueryRowContext(ctx, `UPDATE agent_task_dispatches SET status='leased',lease_owner=$2,lease_until=clock_timestamp()+$3::interval,attempts=attempts+1,updated_at=clock_timestamp() WHERE task_id=$1 AND status='pending' RETURNING lease_until`, t.ID, owner, lease.String()).Scan(&leaseUntil); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoDispatch
	} else if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &DispatchClaim{Task: t, LeaseOwner: owner, LeaseUntil: leaseUntil, Attempts: attempts + 1}, nil
}

func (s *PostgresStore) BeginAttempt(ctx context.Context, a AgentRef, id, owner, attemptID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE agent_task_dispatches d SET status='attempting',attempt_started_at=clock_timestamp(),attempt_token=$5,updated_at=clock_timestamp() FROM agent_tasks t WHERE d.task_id=t.id AND d.task_id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3 AND d.status='leased' AND d.lease_owner=$4 AND d.lease_until>clock_timestamp()`, id, a.Namespace, a.Name, owner, attemptID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		var status, token string
		if err := tx.QueryRowContext(ctx, `SELECT d.status,d.attempt_token FROM agent_task_dispatches d JOIN agent_tasks t ON t.id=d.task_id WHERE d.task_id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3`, id, a.Namespace, a.Name).Scan(&status, &token); err == nil && status == "delivered" && token == attemptID {
			return nil
		}
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_task_interactions SET status='consumed' WHERE task_id=$1 AND status='answered'`, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *PostgresStore) MarkReceiptPending(ctx context.Context, a AgentRef, id, owner, attemptID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE agent_task_dispatches d SET status='receipt_pending',updated_at=clock_timestamp() FROM agent_tasks t WHERE d.task_id=t.id AND d.task_id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3 AND d.status='attempting' AND d.lease_owner=$4 AND d.attempt_token=$5`, id, a.Namespace, a.Name, owner, attemptID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		var status, token string
		if err := s.db.QueryRowContext(ctx, `SELECT d.status,d.attempt_token FROM agent_task_dispatches d JOIN agent_tasks t ON t.id=d.task_id WHERE d.task_id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3`, id, a.Namespace, a.Name).Scan(&status, &token); err == nil && status == "delivered" && token == attemptID {
			return nil
		}
		return ErrConflict
	}
	return nil
}
func (s *PostgresStore) ReleaseLease(ctx context.Context, a AgentRef, id, owner string, backoff time.Duration) error {
	res, err := s.db.ExecContext(ctx, `UPDATE agent_task_dispatches d SET status='pending',lease_owner=NULL,lease_until=NULL,next_attempt_at=clock_timestamp()+$5::interval,updated_at=clock_timestamp() FROM agent_tasks t WHERE d.task_id=t.id AND d.task_id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3 AND d.status='leased' AND d.lease_owner=$4`, id, a.Namespace, a.Name, owner, backoff.String())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	return nil
}

func (s *PostgresStore) RenewLease(ctx context.Context, a AgentRef, id, owner string, lease time.Duration) error {
	res, err := s.db.ExecContext(ctx, `UPDATE agent_task_dispatches d SET lease_until=clock_timestamp()+$5::interval,updated_at=clock_timestamp() FROM agent_tasks t WHERE d.task_id=t.id AND d.task_id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3 AND d.status='leased' AND d.lease_owner=$4`, id, a.Namespace, a.Name, owner, lease.String())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	return nil
}

func (s *PostgresStore) FailDelivery(ctx context.Context, a AgentRef, id, owner string, version int64, code FailureCode) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET state='failed',failure_code=$5,version=version+1,updated_at=clock_timestamp(),completed_at=clock_timestamp() WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3 AND version=$4 AND state='queued'`, id, a.Namespace, a.Name, version, code)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	res, err = tx.ExecContext(ctx, `UPDATE agent_task_dispatches SET status='closed',last_error_code=$3,lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp() WHERE task_id=$1 AND lease_owner=$2 AND status IN ('leased','attempting','receipt_pending')`, id, owner, code)
	if err != nil {
		return err
	}
	n, _ = res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	return tx.Commit()
}

func (s *PostgresStore) AcceptReceipt(ctx context.Context, a AgentRef, r Receipt) (*Task, bool, error) {
	if r.TaskID == "" || r.AttemptID == "" || r.Runtime == "" || r.SessionID == "" {
		return nil, false, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var token, receiptID, runtime, sessionID, turnID, status string
	err = tx.QueryRowContext(ctx, `SELECT d.attempt_token,d.receipt_id,d.receipt_runtime,d.receipt_session_id,d.receipt_turn_id,d.status FROM agent_task_dispatches d JOIN agent_tasks t ON t.id=d.task_id WHERE d.task_id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3 FOR UPDATE OF d,t`, r.TaskID, a.Namespace, a.Name).Scan(&token, &receiptID, &runtime, &sessionID, &turnID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	if token != r.AttemptID {
		return nil, false, ErrReceiptConflict
	}
	if receiptID != "" {
		if receiptID != r.AttemptID || runtime != r.Runtime || sessionID != r.SessionID || turnID != r.TurnID {
			return nil, false, ErrReceiptConflict
		}
		t, err := getTaskTx(ctx, tx, a, r.TaskID)
		if err != nil {
			return nil, false, err
		}
		if err = tx.Commit(); err != nil {
			return nil, false, err
		}
		return t, false, nil
	}
	if status != "attempting" && status != "receipt_pending" {
		return nil, false, ErrConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE agent_task_dispatches SET status='delivered',receipt_id=$2,receipt_runtime=$3,receipt_session_id=$4,receipt_turn_id=$5,lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp() WHERE task_id=$1`, r.TaskID, r.AttemptID, r.Runtime, r.SessionID, r.TurnID)
	if err != nil {
		return nil, false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET state='dispatched',version=version+1,updated_at=clock_timestamp() WHERE id=$1 AND state='queued'`, r.TaskID)
	if err != nil {
		return nil, false, err
	}
	t, err := getTaskTx(ctx, tx, a, r.TaskID)
	if err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return t, true, nil
}

func (s *PostgresStore) GetReceipt(ctx context.Context, a AgentRef, attemptID string) (*Receipt, error) {
	r := &Receipt{}
	err := s.db.QueryRowContext(ctx, `SELECT d.task_id,d.receipt_id,d.receipt_runtime,d.receipt_session_id,d.receipt_turn_id FROM agent_task_dispatches d JOIN agent_tasks t ON t.id=d.task_id WHERE d.attempt_token=$1 AND t.agent_namespace=$2 AND t.agent_name=$3 AND d.receipt_id<>''`, attemptID, a.Namespace, a.Name).Scan(&r.TaskID, &r.AttemptID, &r.Runtime, &r.SessionID, &r.TurnID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func (s *PostgresStore) Reconcile(ctx context.Context, limit int) (*ReconcileResult, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := &ReconcileResult{}
	if _, err := tx.ExecContext(ctx, `WITH picked AS (SELECT id FROM agent_task_objects WHERE status='pending' AND lease_until<clock_timestamp() FOR UPDATE SKIP LOCKED LIMIT $1) UPDATE agent_task_objects o SET status='deleting',lease_owner=NULL,lease_until=NULL,next_attempt_at=clock_timestamp() FROM picked WHERE o.id=picked.id`, limit); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `WITH expired AS (SELECT id FROM agent_tasks WHERE state IN ('canceled','completed','failed','rejected') AND retain_until<clock_timestamp() LIMIT $1) UPDATE agent_task_objects o SET status='deleting' FROM expired WHERE o.task_id=expired.id AND o.status='ready'`, limit); err != nil {
		return nil, err
	}
	queries := []struct {
		q string
		n *int64
	}{
		{`WITH picked AS (SELECT d.task_id FROM agent_task_dispatches d JOIN agent_tasks t ON t.id=d.task_id WHERE d.status='leased' AND t.state='queued' AND d.lease_until<clock_timestamp() FOR UPDATE OF d SKIP LOCKED LIMIT $1) UPDATE agent_task_dispatches d SET status='pending',lease_owner=NULL,lease_until=NULL,next_attempt_at=clock_timestamp(),updated_at=clock_timestamp() FROM picked WHERE d.task_id=picked.task_id`, &out.RequeuedLeases},
		{`WITH picked AS (SELECT d.task_id FROM agent_task_dispatches d WHERE d.status IN ('attempting','receipt_pending') AND d.lease_until<clock_timestamp() FOR UPDATE SKIP LOCKED LIMIT $1), closed AS (UPDATE agent_task_dispatches d SET status='closed',last_error_code='delivery_unknown',lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp() FROM picked WHERE d.task_id=picked.task_id RETURNING d.task_id) UPDATE agent_tasks t SET state='failed',failure_code='delivery_unknown',version=version+1,updated_at=clock_timestamp(),completed_at=clock_timestamp() FROM closed WHERE t.id=closed.task_id AND t.state IN ('queued','dispatched')`, &out.UnknownAttempts},
		{`WITH picked AS (SELECT id FROM agent_tasks WHERE state IN ('queued','dispatched','input_required','auth_required') AND deadline_at<clock_timestamp() FOR UPDATE SKIP LOCKED LIMIT $1), closed AS (UPDATE agent_tasks t SET state='failed',failure_code=CASE WHEN t.state='input_required' THEN 'input_timeout' WHEN t.state='auth_required' THEN 'auth_timeout' ELSE 'deadline_exceeded' END,version=version+1,updated_at=clock_timestamp(),completed_at=clock_timestamp() FROM picked WHERE t.id=picked.id RETURNING t.id) UPDATE agent_task_dispatches d SET status='closed',last_error_code='deadline_exceeded',lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp() FROM closed WHERE d.task_id=closed.id`, &out.ExpiredTasks},
		{`WITH picked AS (SELECT i.id,i.task_id,i.type FROM agent_task_interactions i JOIN agent_tasks t ON t.id=i.task_id WHERE i.status='paused' AND i.expires_at<clock_timestamp() AND t.state IN ('input_required','auth_required') FOR UPDATE OF i,t SKIP LOCKED LIMIT $1), expired AS (UPDATE agent_task_interactions i SET status='expired' FROM picked WHERE i.id=picked.id RETURNING picked.task_id,picked.type), failed AS (UPDATE agent_tasks t SET state='failed',failure_code=CASE WHEN expired.type='authorization' THEN 'auth_timeout' ELSE 'input_timeout' END,version=version+1,updated_at=clock_timestamp(),completed_at=clock_timestamp() FROM expired WHERE t.id=expired.task_id RETURNING t.id) UPDATE agent_task_dispatches d SET status='closed',last_error_code='interaction_timeout',updated_at=clock_timestamp() FROM failed WHERE d.task_id=failed.id`, &out.ExpiredInteractions},
		{`WITH picked AS (SELECT id FROM agent_tasks WHERE state='canceling' AND cancel_deadline_at<clock_timestamp() FOR UPDATE SKIP LOCKED LIMIT $1), failed AS (UPDATE agent_tasks t SET state='failed',failure_code='cancel_unconfirmed',version=version+1,updated_at=clock_timestamp(),completed_at=clock_timestamp() FROM picked WHERE t.id=picked.id RETURNING t.id), closed_cancel AS (UPDATE agent_task_cancel_deliveries c SET status='closed',last_safe_error='cancel_unconfirmed',lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp() FROM failed WHERE c.task_id=failed.id) UPDATE agent_task_dispatches d SET status='closed',last_error_code='cancel_unconfirmed',lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp() FROM failed WHERE d.task_id=failed.id`, &out.CancelUnconfirmed},
		{`WITH picked AS (SELECT c.task_id FROM agent_task_cancel_deliveries c JOIN agent_tasks t ON t.id=c.task_id WHERE c.status IN ('pending','delivering','notified','interrupted') AND t.state IN ('completed','failed','canceled') FOR UPDATE OF c SKIP LOCKED LIMIT $1) UPDATE agent_task_cancel_deliveries c SET status='closed',lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp() FROM picked WHERE c.task_id=picked.task_id`, &out.ClosedCancellations},
		{`WITH picked AS (SELECT t.id FROM agent_tasks t WHERE t.state IN ('canceled','completed','failed','rejected') AND t.retain_until<clock_timestamp() AND NOT EXISTS (SELECT 1 FROM agent_task_objects o WHERE o.task_id=t.id) FOR UPDATE SKIP LOCKED LIMIT $1) DELETE FROM agent_tasks t USING picked WHERE t.id=picked.id`, &out.DeletedTasks},
	}
	for _, item := range queries {
		res, err := tx.ExecContext(ctx, item.q, limit)
		if err != nil {
			return nil, err
		}
		*item.n, _ = res.RowsAffected()
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PostgresStore) ClaimObjectDeletions(ctx context.Context, owner string, lease time.Duration, limit int) ([]ObjectDeletion, error) {
	if owner == "" || lease <= 0 || limit < 1 || limit > 1000 {
		return nil, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `WITH picked AS (SELECT id FROM agent_task_objects WHERE status='deleting' AND next_attempt_at<=clock_timestamp() AND (lease_until IS NULL OR lease_until<clock_timestamp()) ORDER BY next_attempt_at,created_at FOR UPDATE SKIP LOCKED LIMIT $1) UPDATE agent_task_objects o SET lease_owner=$2,lease_until=clock_timestamp()+$3::interval FROM picked WHERE o.id=picked.id RETURNING o.id,o.object_key`, limit, owner, lease.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectDeletion
	for rows.Next() {
		var object ObjectDeletion
		if err := rows.Scan(&object.ID, &object.ObjectKey); err != nil {
			return nil, err
		}
		object.LeaseOwner = owner
		out = append(out, object)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteObjectRecord(ctx context.Context, id, owner string) error {
	if id == "" || owner == "" {
		return ErrInvalid
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM agent_task_objects WHERE id=$1 AND status='deleting' AND lease_owner=$2 AND lease_until>=clock_timestamp()`, id, owner)
	if err == nil {
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrConflict
		}
	}
	return err
}

func (s *PostgresStore) RetryObjectDeletion(ctx context.Context, id, owner, message string) error {
	if id == "" || owner == "" {
		return ErrInvalid
	}
	if len(message) > 1024 {
		message = message[:1024]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE agent_task_objects SET deletion_attempts=deletion_attempts+1,last_error=$3,lease_owner=NULL,lease_until=NULL,next_attempt_at=clock_timestamp()+(LEAST(3600,power(2,LEAST(deletion_attempts,11)))::text||' seconds')::interval WHERE id=$1 AND status='deleting' AND lease_owner=$2`, id, owner, message)
	return err
}
