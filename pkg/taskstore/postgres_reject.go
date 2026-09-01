package taskstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

func (s *PostgresStore) Reject(ctx context.Context, a AgentRef, id, attemptID, reason string) (*Task, error) {
	if attemptID == "" || strings.TrimSpace(reason) == "" || len([]byte(reason)) > s.limits.MaxResponseBytes {
		return nil, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var state State
	var current, status string
	err = tx.QueryRowContext(ctx, `SELECT t.state,d.attempt_token,d.status FROM agent_tasks t JOIN agent_task_dispatches d ON d.task_id=t.id WHERE t.id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3 FOR UPDATE OF t,d`, id, a.Namespace, a.Name).Scan(&state, &current, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if state != StateDispatched || current != attemptID || status != "delivered" {
		return nil, ErrInvalidAttempt
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET state='rejected',response=$2,version=version+1,updated_at=clock_timestamp(),completed_at=clock_timestamp() WHERE id=$1`, id, reason); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_task_dispatches SET status='closed',last_error_code='rejected',updated_at=clock_timestamp() WHERE task_id=$1`, id); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_task_messages(task_id,sequence,role,kind,text_value,created_at) SELECT $1,COALESCE(max(sequence),0)+1,'agent','terminal_summary',$2,clock_timestamp() FROM agent_task_messages WHERE task_id=$1`, id, reason); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, a, id)
}
