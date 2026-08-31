package taskstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

type PostgresStore struct {
	db     *sql.DB
	limits Limits
}

func NewPostgresStore(db *sql.DB, limits Limits) (*PostgresStore, error) {
	if db == nil {
		return nil, ErrInvalid
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &PostgresStore{db: db, limits: limits}, nil
}

func (s *PostgresStore) Migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []string{
		`CREATE TABLE IF NOT EXISTS kyber_task_schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp())`,
		`CREATE TABLE IF NOT EXISTS agent_tasks (
			id TEXT PRIMARY KEY, agent_namespace TEXT NOT NULL, agent_name TEXT NOT NULL,
			created_by TEXT NOT NULL, prompt TEXT NOT NULL, correlation TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL, failure_code TEXT NOT NULL DEFAULT '', response TEXT NOT NULL DEFAULT '',
			version BIGINT NOT NULL DEFAULT 1, created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL, deadline_at TIMESTAMPTZ NOT NULL,
			retain_until TIMESTAMPTZ NOT NULL, completed_at TIMESTAMPTZ,
			CHECK (state IN ('queued','dispatched','completed','failed')))`,
		`CREATE TABLE IF NOT EXISTS agent_task_dispatches (
			task_id TEXT PRIMARY KEY REFERENCES agent_tasks(id) ON DELETE CASCADE,
			status TEXT NOT NULL, lease_owner TEXT, lease_until TIMESTAMPTZ, attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at TIMESTAMPTZ NOT NULL, attempt_started_at TIMESTAMPTZ,
			attempt_token TEXT NOT NULL DEFAULT '', receipt_id TEXT NOT NULL DEFAULT '',
			receipt_runtime TEXT NOT NULL DEFAULT '', receipt_session_id TEXT NOT NULL DEFAULT '', receipt_turn_id TEXT NOT NULL DEFAULT '',
			last_error_code TEXT NOT NULL DEFAULT '', updated_at TIMESTAMPTZ NOT NULL,
			CHECK (status IN ('pending','leased','attempting','receipt_pending','delivered','closed')))`,
		`CREATE TABLE IF NOT EXISTS agent_task_idempotency (
			created_by TEXT NOT NULL, agent_namespace TEXT NOT NULL, agent_name TEXT NOT NULL,
			idempotency_key TEXT NOT NULL, request_hash TEXT NOT NULL,
			task_id TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
			PRIMARY KEY(created_by,agent_namespace,agent_name,idempotency_key))`,
		`ALTER TABLE agent_task_dispatches ADD COLUMN IF NOT EXISTS receipt_runtime TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_task_dispatches ADD COLUMN IF NOT EXISTS receipt_session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_task_dispatches ADD COLUMN IF NOT EXISTS receipt_turn_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS agent_tasks_agent_created_idx ON agent_tasks(agent_namespace,agent_name,created_at DESC,id DESC)`,
		`CREATE INDEX IF NOT EXISTS agent_tasks_agent_state_created_idx ON agent_tasks(agent_namespace,agent_name,state,created_at DESC,id DESC)`,
		`CREATE INDEX IF NOT EXISTS agent_tasks_retain_idx ON agent_tasks(retain_until)`,
		`CREATE INDEX IF NOT EXISTS agent_tasks_deadline_idx ON agent_tasks(deadline_at) WHERE state IN ('queued','dispatched')`,
		`CREATE INDEX IF NOT EXISTS agent_task_dispatch_claim_idx ON agent_task_dispatches(status,next_attempt_at)`,
		`INSERT INTO kyber_task_schema_migrations(version) VALUES (1) ON CONFLICT DO NOTHING`,
	}
	for _, q := range statements {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("task migration: %w", err)
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) Create(ctx context.Context, p CreateParams) (*CreateResult, error) {
	if err := validateCreate(s.limits, p); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Serialize creates per agent so the outstanding limit is exact even when
	// different callers hit different control-plane replicas concurrently.
	// PostgreSQL text parameters cannot contain NUL bytes. Kubernetes namespace
	// and name values cannot contain '/', so it is also an unambiguous lane key.
	lane := p.Agent.Namespace + "/" + p.Agent.Name
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lane); err != nil {
		return nil, err
	}
	if p.IdempotencyKey != "" {
		var hash, id string
		err = tx.QueryRowContext(ctx, `SELECT request_hash,task_id FROM agent_task_idempotency WHERE created_by=$1 AND agent_namespace=$2 AND agent_name=$3 AND idempotency_key=$4`, p.CreatedBy, p.Agent.Namespace, p.Agent.Name, p.IdempotencyKey).Scan(&hash, &id)
		if err == nil {
			if hash != p.RequestHash {
				return nil, ErrIdempotencyConflict
			}
			t, err := getTaskTx(ctx, tx, p.Agent, id)
			if err != nil {
				return nil, err
			}
			if err = tx.Commit(); err != nil {
				return nil, err
			}
			return &CreateResult{Task: t, Replay: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	var retained, outstanding int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_tasks`).Scan(&retained); err != nil {
		return nil, err
	}
	if retained >= s.limits.MaxRetained {
		return nil, ErrCapacity
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_tasks WHERE agent_namespace=$1 AND agent_name=$2 AND state IN ('queued','dispatched')`, p.Agent.Namespace, p.Agent.Name).Scan(&outstanding); err != nil {
		return nil, err
	}
	if outstanding >= s.limits.MaxOutstanding {
		return nil, ErrOutstandingLimit
	}
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	now = now.UTC()
	deadline := p.DeadlineAt.UTC()
	if deadline.IsZero() {
		deadline = now.Add(s.limits.DefaultDeadline)
	}
	if deadline.After(now.Add(s.limits.MaxDeadline)) {
		deadline = now.Add(s.limits.MaxDeadline)
	}
	if !deadline.After(now) {
		return nil, ErrInvalid
	}
	t := &Task{ID: p.ID, AgentNamespace: p.Agent.Namespace, AgentName: p.Agent.Name, CreatedBy: p.CreatedBy, Prompt: p.Prompt, Correlation: p.Correlation, State: StateQueued, Version: 1, CreatedAt: now, UpdatedAt: now, DeadlineAt: deadline, RetainUntil: deadline.Add(s.limits.Retention)}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_tasks(id,agent_namespace,agent_name,created_by,prompt,correlation,state,version,created_at,updated_at,deadline_at,retain_until) VALUES($1,$2,$3,$4,$5,$6,'queued',1,$7,$7,$8,$9)`, t.ID, t.AgentNamespace, t.AgentName, t.CreatedBy, t.Prompt, t.Correlation, now, deadline, t.RetainUntil)
	if err != nil {
		var pe *pq.Error
		if errors.As(err, &pe) && pe.Code == "23505" {
			return nil, ErrConflict
		}
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_task_dispatches(task_id,status,next_attempt_at,updated_at) VALUES($1,'pending',$2,$2)`, t.ID, now); err != nil {
		return nil, err
	}
	if p.IdempotencyKey != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO agent_task_idempotency(created_by,agent_namespace,agent_name,idempotency_key,request_hash,task_id) VALUES($1,$2,$3,$4,$5,$6)`, p.CreatedBy, p.Agent.Namespace, p.Agent.Name, p.IdempotencyKey, p.RequestHash, t.ID); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &CreateResult{Task: t}, nil
}

type rowScanner interface{ Scan(...any) error }

func scanTask(r rowScanner) (*Task, error) {
	t := &Task{}
	var completed sql.NullTime
	err := r.Scan(&t.ID, &t.AgentNamespace, &t.AgentName, &t.CreatedBy, &t.Prompt, &t.Correlation, &t.State, &t.FailureCode, &t.Response, &t.Version, &t.CreatedAt, &t.UpdatedAt, &t.DeadlineAt, &t.RetainUntil, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if completed.Valid {
		v := completed.Time
		t.CompletedAt = &v
	}
	return t, nil
}

const selectTask = `SELECT id,agent_namespace,agent_name,created_by,prompt,correlation,state,failure_code,response,version,created_at,updated_at,deadline_at,retain_until,completed_at FROM agent_tasks`

func getTaskTx(ctx context.Context, tx *sql.Tx, a AgentRef, id string) (*Task, error) {
	return scanTask(tx.QueryRowContext(ctx, selectTask+` WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3`, id, a.Namespace, a.Name))
}
func (s *PostgresStore) Get(ctx context.Context, a AgentRef, id string) (*Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, selectTask+` WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3`, id, a.Namespace, a.Name))
}

func (s *PostgresStore) List(ctx context.Context, p ListParams) (*Page, error) {
	limit := p.Limit
	if limit == 0 {
		limit = s.limits.DefaultListPage
	}
	if limit < 1 || limit > s.limits.MaxListPage {
		return nil, ErrInvalid
	}
	var before time.Time
	var beforeID string
	var err error
	if p.Cursor != "" {
		before, beforeID, err = decodeCursor(p.Cursor, p.Agent, p.State)
		if err != nil {
			return nil, err
		}
	}
	args := []any{p.Agent.Namespace, p.Agent.Name}
	where := ` WHERE agent_namespace=$1 AND agent_name=$2`
	n := 3
	if p.State != "" {
		where += fmt.Sprintf(" AND state=$%d", n)
		args = append(args, p.State)
		n++
	}
	if !before.IsZero() {
		where += fmt.Sprintf(" AND (created_at,id)<($%d,$%d)", n, n+1)
		args = append(args, before, beforeID)
		n += 2
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, selectTask+where+fmt.Sprintf(" ORDER BY created_at DESC,id DESC LIMIT $%d", n), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]*Task, 0, limit+1)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, t)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	page := &Page{}
	if len(items) > limit {
		page.Tasks = items[:limit]
		page.NextCursor, err = encodeCursor(p.Agent, p.State, page.Tasks[len(page.Tasks)-1])
		if err != nil {
			return nil, err
		}
	} else {
		page.Tasks = items
	}
	return page, nil
}

func (s *PostgresStore) transition(ctx context.Context, a AgentRef, id string, v int64, query string, args ...any) (*Task, error) {
	all := []any{id, a.Namespace, a.Name, v}
	all = append(all, args...)
	res, err := s.db.ExecContext(ctx, query, all...)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 1 {
		return nil, nil
	}
	current, err := s.Get(ctx, a, id)
	if err != nil {
		return nil, err
	}
	return current, ErrConflict
}
func (s *PostgresStore) MarkDispatched(ctx context.Context, a AgentRef, id string, v int64) error {
	current, err := s.transition(ctx, a, id, v, `UPDATE agent_tasks SET state='dispatched',version=version+1,updated_at=clock_timestamp() WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3 AND version=$4 AND state='queued'`)
	if errors.Is(err, ErrConflict) && current.State == StateDispatched {
		return nil
	}
	return err
}
func (s *PostgresStore) Fail(ctx context.Context, a AgentRef, id string, v int64, code FailureCode) error {
	current, err := s.transition(ctx, a, id, v, `UPDATE agent_tasks SET state='failed',failure_code=$5,version=version+1,updated_at=clock_timestamp(),completed_at=clock_timestamp() WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3 AND version=$4 AND state IN ('queued','dispatched')`, code)
	if errors.Is(err, ErrConflict) && current.State == StateFailed && current.FailureCode == code {
		return nil
	}
	return err
}
func (s *PostgresStore) Complete(ctx context.Context, a AgentRef, id string, v int64, response string) error {
	if len([]byte(response)) > s.limits.MaxResponseBytes {
		return ErrResponseTooLarge
	}
	current, err := s.transition(ctx, a, id, v, `UPDATE agent_tasks SET state='completed',response=$5,version=version+1,updated_at=clock_timestamp(),completed_at=clock_timestamp() WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3 AND version=$4 AND state='dispatched'`, response)
	if errors.Is(err, ErrConflict) && current.State == StateCompleted && current.Response == response {
		return nil
	}
	return err
}
