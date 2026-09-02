package taskstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

const maxEventReadPage = 200

func newTaskEventID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "event_" + hex.EncodeToString(b[:]), nil
}

// appendTaskEventTx must be called in the transaction that accepted the public
// task mutation. Callers lock the task row before mutation, which serializes
// sequence allocation and guarantees no committed per-task gaps.
func appendTaskEventTx(ctx context.Context, tx *sql.Tx, taskID string, typ EventType, payload any) error {
	eventID, err := newTaskEventID()
	if err != nil {
		return err
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if len(b) > HardMaxJSONPartBytes {
		return ErrResultTooLarge
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO agent_task_events(event_id,task_id,tenant_id,owner_principal_id,agent_resource_id,sequence,task_version,type,occurred_at,payload_version,payload)
		SELECT $2,t.id,t.tenant_id,t.owner_principal_id,t.agent_resource_id,
		       COALESCE((SELECT max(e.sequence) FROM agent_task_events e WHERE e.task_id=t.id),0)+1,
		       t.version,$3,clock_timestamp(),'v1',$4
		FROM agent_tasks t WHERE t.id=$1`, taskID, eventID, typ, b)
	if err != nil {
		return err
	}
	if n, rowsErr := res.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if n != 1 {
		return ErrNotFound
	}
	return nil
}

func terminalState(s State) bool {
	switch s {
	case StateCanceled, StateCompleted, StateFailed, StateRejected:
		return true
	default:
		return false
	}
}

func (s *PostgresStore) EventHighWater(ctx context.Context, a AgentRef, taskID string, auth AuthorizationContext) (int64, error) {
	if !auth.Valid() {
		return 0, ErrNotFound
	}
	var high int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(max(e.sequence),0)
		FROM agent_tasks t LEFT JOIN agent_task_events e ON e.task_id=t.id
		WHERE t.id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3
		  AND t.tenant_id=$4 AND t.owner_principal_id=$5 AND t.agent_resource_id=$6
		GROUP BY t.id`, taskID, a.Namespace, a.Name, auth.TenantID, auth.PrincipalID, auth.AgentResourceID).Scan(&high)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return high, err
}

// EventSnapshot reads the authoritative task and its event high-water from one
// repeatable-read snapshot. Returning them separately could let a mutation land
// between reads and make a client skip an event that its task snapshot lacks.
func (s *PostgresStore) EventSnapshot(ctx context.Context, a AgentRef, taskID string, auth AuthorizationContext) (*Task, int64, error) {
	if !auth.Valid() {
		return nil, 0, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	t, err := scanTask(tx.QueryRowContext(ctx, selectTask+` WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3 AND tenant_id=$4 AND owner_principal_id=$5 AND agent_resource_id=$6 AND (state IN ('queued','dispatched','input_required','auth_required','canceling') OR retain_until>=clock_timestamp())`, taskID, a.Namespace, a.Name, auth.TenantID, auth.PrincipalID, auth.AgentResourceID))
	if err != nil {
		return nil, 0, err
	}
	t.Results, err = loadResults(ctx, tx, taskID)
	if err != nil {
		return nil, 0, err
	}
	if err = loadConversation(ctx, tx, t); err != nil {
		return nil, 0, err
	}
	var high int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(max(sequence),0) FROM agent_task_events WHERE task_id=$1 AND tenant_id=$2 AND owner_principal_id=$3 AND agent_resource_id=$4`, taskID, auth.TenantID, auth.PrincipalID, auth.AgentResourceID).Scan(&high); err != nil {
		return nil, 0, err
	}
	if err = tx.Commit(); err != nil {
		return nil, 0, err
	}
	return t, high, nil
}

func (s *PostgresStore) ReadEvents(ctx context.Context, p EventReadParams) (*EventPage, error) {
	if !p.Authorization.Valid() || p.TaskID == "" || p.AfterSequence < 0 || p.Through < 0 {
		return nil, ErrInvalid
	}
	if p.Limit <= 0 {
		p.Limit = 100
	}
	if p.Limit > maxEventReadPage {
		p.Limit = maxEventReadPage
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	page := &EventPage{Events: make([]TaskEvent, 0, p.Limit)}
	var state State
	err = tx.QueryRowContext(ctx, `
		SELECT t.state,COALESCE(min(e.sequence),0),COALESCE(max(e.sequence),0)
		FROM agent_tasks t LEFT JOIN agent_task_events e ON e.task_id=t.id
		WHERE t.id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3
		  AND t.tenant_id=$4 AND t.owner_principal_id=$5 AND t.agent_resource_id=$6
		GROUP BY t.id,t.state`, p.TaskID, p.Agent.Namespace, p.Agent.Name, p.Authorization.TenantID, p.Authorization.PrincipalID, p.Authorization.AgentResourceID).Scan(&state, &page.RetainedFloor, &page.HighWater)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	page.Terminal = terminalState(state)
	if p.Resume && page.RetainedFloor > 0 && p.AfterSequence < page.RetainedFloor-1 {
		return page, ErrEventCursorExpired
	}
	through := p.Through
	if through == 0 || through > page.HighWater {
		through = page.HighWater
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id,task_id,sequence,task_version,type,occurred_at,payload_version,payload
		FROM agent_task_events
		WHERE task_id=$1 AND tenant_id=$2 AND owner_principal_id=$3 AND agent_resource_id=$4
		  AND sequence>$5 AND sequence<=$6
		ORDER BY sequence LIMIT $7`, p.TaskID, p.Authorization.TenantID, p.Authorization.PrincipalID, p.Authorization.AgentResourceID, p.AfterSequence, through, p.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e TaskEvent
		if err = rows.Scan(&e.ID, &e.TaskID, &e.Sequence, &e.TaskVersion, &e.Type, &e.OccurredAt, &e.PayloadVersion, &e.Payload); err != nil {
			return nil, err
		}
		e.OccurredAt = e.OccurredAt.UTC()
		page.Events = append(page.Events, e)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("reading task events: %w", err)
	}
	return page, nil
}
