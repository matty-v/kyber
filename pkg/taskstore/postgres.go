package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
		`ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS progress_message TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS progress_percent SMALLINT`,
		`ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS progress_updated_at TIMESTAMPTZ`,
		`ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS cancel_requested_at TIMESTAMPTZ`,
		`ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS cancel_requested_by TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS cancel_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS cancel_deadline_at TIMESTAMPTZ`,
		`ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS cancel_acknowledged_at TIMESTAMPTZ`,
		`ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS cancel_ack_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_tasks DROP CONSTRAINT IF EXISTS agent_tasks_state_check`,
		`ALTER TABLE agent_tasks ADD CONSTRAINT agent_tasks_state_check CHECK (state IN ('queued','dispatched','input_required','auth_required','canceling','canceled','completed','failed','rejected'))`,
		`CREATE TABLE IF NOT EXISTS agent_task_cancel_idempotency (created_by TEXT NOT NULL, agent_namespace TEXT NOT NULL, agent_name TEXT NOT NULL, idempotency_key TEXT NOT NULL, request_hash TEXT NOT NULL, task_id TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE, applied BOOLEAN NOT NULL DEFAULT false, PRIMARY KEY(created_by,agent_namespace,agent_name,idempotency_key))`,
		`ALTER TABLE agent_task_cancel_idempotency ADD COLUMN IF NOT EXISTS applied BOOLEAN NOT NULL DEFAULT false`,
		`CREATE TABLE IF NOT EXISTS agent_task_cancel_deliveries (task_id TEXT PRIMARY KEY REFERENCES agent_tasks(id) ON DELETE CASCADE, attempt_id TEXT NOT NULL, status TEXT NOT NULL, adapter_mode TEXT NOT NULL, delivery_count INTEGER NOT NULL DEFAULT 0, next_delivery_at TIMESTAMPTZ NOT NULL, lease_owner TEXT, lease_until TIMESTAMPTZ, acknowledgment_id TEXT NOT NULL DEFAULT '', last_safe_error TEXT NOT NULL DEFAULT '', updated_at TIMESTAMPTZ NOT NULL, CHECK(status IN ('pending','delivering','notified','interrupted','acknowledged','closed')), CHECK(adapter_mode IN ('notify_only','exact_interrupt')))`,
		`DO $$ BEGIN ALTER TABLE agent_tasks ADD CONSTRAINT agent_tasks_progress_percent_check CHECK (progress_percent IS NULL OR progress_percent BETWEEN 0 AND 100); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`CREATE TABLE IF NOT EXISTS agent_task_results (id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', content_digest TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, UNIQUE(task_id,name))`,
		`CREATE TABLE IF NOT EXISTS agent_task_objects (id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE, object_key TEXT NOT NULL UNIQUE, status TEXT NOT NULL, filename TEXT NOT NULL, media_type TEXT NOT NULL, size_bytes BIGINT, sha256 TEXT, scan_status TEXT NOT NULL DEFAULT 'not_configured', lease_owner TEXT, lease_until TIMESTAMPTZ, deletion_attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(), last_error TEXT NOT NULL DEFAULT '', retain_until TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL, ready_at TIMESTAMPTZ, CHECK(status IN ('pending','ready','deleting')))`,
		`ALTER TABLE agent_task_objects ADD COLUMN IF NOT EXISTS lease_owner TEXT`,
		`ALTER TABLE agent_task_objects ADD COLUMN IF NOT EXISTS deletion_attempts INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE agent_task_objects ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()`,
		`ALTER TABLE agent_task_objects ADD COLUMN IF NOT EXISTS last_error TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE IF NOT EXISTS agent_task_result_parts (id TEXT PRIMARY KEY, result_id TEXT NOT NULL REFERENCES agent_task_results(id) ON DELETE CASCADE, ordinal INTEGER NOT NULL, kind TEXT NOT NULL, text_value TEXT, json_value JSONB, object_id TEXT REFERENCES agent_task_objects(id) ON DELETE SET NULL, UNIQUE(result_id,ordinal), CHECK(kind IN ('text','json','file')))`,
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='agent_task_result_parts_object_id_fkey' AND conrelid='agent_task_result_parts'::regclass AND confdeltype='n') THEN ALTER TABLE agent_task_result_parts DROP CONSTRAINT IF EXISTS agent_task_result_parts_object_id_fkey; ALTER TABLE agent_task_result_parts ADD CONSTRAINT agent_task_result_parts_object_id_fkey FOREIGN KEY (object_id) REFERENCES agent_task_objects(id) ON DELETE SET NULL; END IF; END $$`,
		`CREATE TABLE IF NOT EXISTS agent_task_updates (task_id TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE, sequence BIGINT NOT NULL, update_id TEXT NOT NULL, kind TEXT NOT NULL, safe_summary JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(task_id,sequence), UNIQUE(task_id,update_id), CHECK(kind IN ('progress','result','completed')))`,
		`CREATE TABLE IF NOT EXISTS agent_task_interactions (id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE, attempt_id TEXT NOT NULL, type TEXT NOT NULL, status TEXT NOT NULL, question TEXT NOT NULL, options JSONB, schema JSONB, authorization_flow TEXT NOT NULL DEFAULT '', response JSONB, created_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL, answered_at TIMESTAMPTZ, CHECK(type IN ('text','choice','confirm','json','authorization')), CHECK(status IN ('paused','answered','consumed','expired')))`,
		`CREATE TABLE IF NOT EXISTS agent_task_authorization_flows (id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE, interaction_id TEXT NOT NULL, created_by TEXT NOT NULL, connection_kind TEXT NOT NULL, status TEXT NOT NULL, connection_reference TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL, completed_at TIMESTAMPTZ, UNIQUE(task_id,interaction_id), CHECK(status IN ('pending','completed','expired')))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS agent_task_one_live_interaction_idx ON agent_task_interactions(task_id) WHERE status IN ('paused','answered')`,
		`CREATE TABLE IF NOT EXISTS agent_task_messages (task_id TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE, sequence BIGINT NOT NULL, role TEXT NOT NULL, kind TEXT NOT NULL, text_value TEXT NOT NULL DEFAULT '', data JSONB, created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(task_id,sequence))`,
		`CREATE TABLE IF NOT EXISTS agent_task_interaction_idempotency (responded_by TEXT NOT NULL, task_id TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE, interaction_id TEXT NOT NULL REFERENCES agent_task_interactions(id) ON DELETE CASCADE, idempotency_key TEXT NOT NULL, request_hash TEXT NOT NULL, PRIMARY KEY(responded_by,task_id,interaction_id,idempotency_key))`,
		`CREATE INDEX IF NOT EXISTS agent_tasks_agent_created_idx ON agent_tasks(agent_namespace,agent_name,created_at DESC,id DESC)`,
		`CREATE INDEX IF NOT EXISTS agent_tasks_agent_state_created_idx ON agent_tasks(agent_namespace,agent_name,state,created_at DESC,id DESC)`,
		`CREATE INDEX IF NOT EXISTS agent_tasks_retain_idx ON agent_tasks(retain_until)`,
		`DROP INDEX IF EXISTS agent_tasks_deadline_idx`,
		`CREATE INDEX IF NOT EXISTS agent_tasks_deadline_idx ON agent_tasks(deadline_at) WHERE state IN ('queued','dispatched','input_required','auth_required')`,
		`CREATE INDEX IF NOT EXISTS agent_task_cancel_delivery_idx ON agent_task_cancel_deliveries(status,next_delivery_at,lease_until)`,
		`CREATE INDEX IF NOT EXISTS agent_task_dispatch_claim_idx ON agent_task_dispatches(status,next_attempt_at)`,
		`CREATE INDEX IF NOT EXISTS agent_task_object_cleanup_idx ON agent_task_objects(status,next_attempt_at,lease_until)`,
		`INSERT INTO kyber_task_schema_migrations(version) VALUES (1),(2),(3),(4) ON CONFLICT DO NOTHING`,
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
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_tasks WHERE agent_namespace=$1 AND agent_name=$2 AND state IN ('queued','dispatched','input_required','auth_required','canceling')`, p.Agent.Namespace, p.Agent.Name).Scan(&outstanding); err != nil {
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_task_messages(task_id,sequence,role,kind,text_value,created_at) VALUES($1,1,'caller','task_instruction',$2,$3)`, t.ID, t.Prompt, now); err != nil {
		return nil, err
	}
	t.Messages = []Message{{Sequence: 1, Role: "caller", Kind: "task_instruction", Text: t.Prompt, CreatedAt: now}}
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
	var completed, progressAt, cancelRequestedAt, cancelDeadlineAt, cancelAcknowledgedAt sql.NullTime
	var progressPercent sql.NullInt64
	var progressMessage, cancelRequestedBy, cancelReason, cancelAckSource string
	err := r.Scan(&t.ID, &t.AgentNamespace, &t.AgentName, &t.CreatedBy, &t.Prompt, &t.Correlation, &t.State, &t.FailureCode, &t.Response, &t.Version, &t.CreatedAt, &t.UpdatedAt, &t.DeadlineAt, &t.RetainUntil, &completed, &progressMessage, &progressPercent, &progressAt, &cancelRequestedAt, &cancelRequestedBy, &cancelReason, &cancelDeadlineAt, &cancelAcknowledgedAt, &cancelAckSource)
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
	if progressAt.Valid {
		t.Progress = &Progress{Message: progressMessage, UpdatedAt: progressAt.Time.UTC()}
		if progressPercent.Valid {
			p := int(progressPercent.Int64)
			t.Progress.Percent = &p
		}
	}
	if cancelRequestedAt.Valid {
		c := &Cancellation{RequestedAt: cancelRequestedAt.Time.UTC(), RequestedBy: cancelRequestedBy, Reason: cancelReason, Status: "requested", Scope: "future_task_work"}
		if cancelDeadlineAt.Valid {
			c.DeadlineAt = cancelDeadlineAt.Time.UTC()
		}
		if cancelAcknowledgedAt.Valid {
			v := cancelAcknowledgedAt.Time.UTC()
			c.AcknowledgedAt, c.Status = &v, "acknowledged"
		}
		c.AckSource = cancelAckSource
		t.Cancellation = c
	}
	return t, nil
}

const selectTask = `SELECT id,agent_namespace,agent_name,created_by,prompt,correlation,state,failure_code,response,version,created_at,updated_at,deadline_at,retain_until,completed_at,progress_message,progress_percent,progress_updated_at,cancel_requested_at,cancel_requested_by,cancel_reason,cancel_deadline_at,cancel_acknowledged_at,cancel_ack_source FROM agent_tasks`

func getTaskTx(ctx context.Context, tx *sql.Tx, a AgentRef, id string) (*Task, error) {
	return scanTask(tx.QueryRowContext(ctx, selectTask+` WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3`, id, a.Namespace, a.Name))
}
func (s *PostgresStore) Get(ctx context.Context, a AgentRef, id string) (*Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, selectTask+` WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3 AND (state IN ('queued','dispatched','canceling') OR retain_until>=clock_timestamp())`, id, a.Namespace, a.Name))
	if err != nil {
		return nil, err
	}
	t.Results, err = loadResults(ctx, s.db, id)
	if err == nil {
		err = loadConversation(ctx, s.db, t)
	}
	return t, err
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
	where := ` WHERE agent_namespace=$1 AND agent_name=$2 AND (state IN ('queued','dispatched','canceling') OR retain_until>=clock_timestamp())`
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
	if err = rows.Close(); err != nil {
		return nil, err
	}
	for _, t := range items {
		t.Results, err = loadResults(ctx, s.db, t.ID)
		if err == nil {
			err = loadConversation(ctx, s.db, t)
		}
		if err != nil {
			return nil, err
		}
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

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadResults(ctx context.Context, q queryer, taskID string) ([]Result, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.id,r.name,r.description,r.content_digest,r.created_at,p.id,p.kind,p.text_value,p.json_value,o.id,o.filename,o.media_type,o.size_bytes,o.sha256,o.scan_status FROM agent_task_results r JOIN agent_task_result_parts p ON p.result_id=r.id LEFT JOIN agent_task_objects o ON o.id=p.object_id WHERE r.task_id=$1 ORDER BY r.created_at,r.id,p.ordinal`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Result
	index := map[string]int{}
	for rows.Next() {
		var r Result
		var p ResultPart
		var text, rawJSON, oid, filename, media, sha, scan sql.NullString
		var size sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Name, &r.Description, &r.ContentDigest, &r.CreatedAt, &p.ID, &p.Kind, &text, &rawJSON, &oid, &filename, &media, &size, &sha, &scan); err != nil {
			return nil, err
		}
		if text.Valid {
			p.Text = text.String
		}
		if rawJSON.Valid {
			p.JSON = []byte(rawJSON.String)
		}
		if oid.Valid {
			p.File = &FileMetadata{ObjectID: oid.String, Filename: filename.String, MediaType: media.String, SizeBytes: size.Int64, SHA256: sha.String, ScanStatus: scan.String}
		}
		i, ok := index[r.ID]
		if !ok {
			i = len(out)
			index[r.ID] = i
			out = append(out, r)
		}
		out[i].Parts = append(out[i].Parts, p)
	}
	return out, rows.Err()
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
	current, err := s.transition(ctx, a, id, v, `UPDATE agent_tasks SET state='failed',failure_code=$5,version=version+1,updated_at=clock_timestamp(),completed_at=clock_timestamp() WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3 AND version=$4 AND state IN ('queued','dispatched','canceling')`, code)
	if errors.Is(err, ErrConflict) && current.State == StateFailed && current.FailureCode == code {
		return nil
	}
	return err
}
func (s *PostgresStore) Complete(ctx context.Context, a AgentRef, id string, v int64, response string) error {
	if len([]byte(response)) > s.limits.MaxResponseBytes {
		return ErrResponseTooLarge
	}
	if response != "" {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		t, err := scanTask(tx.QueryRowContext(ctx, selectTask+` WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3 FOR UPDATE`, id, a.Namespace, a.Name))
		if err != nil {
			return err
		}
		if t.State == StateCompleted && t.Response == response {
			return nil
		}
		if t.Version != v || (t.State != StateDispatched && t.State != StateCanceling) {
			return ErrConflict
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_task_results WHERE task_id=$1`, id).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			sum := sha256.Sum256([]byte(id))
			rid := "result_" + hex.EncodeToString(sum[:16])
			r := Result{ID: rid, Name: "response", Parts: []ResultPart{{ID: rid + "_part_0", Kind: PartText, Text: response}}}
			if _, err = tx.ExecContext(ctx, `INSERT INTO agent_task_results(id,task_id,name,content_digest,created_at) VALUES($1,$2,'response',$3,clock_timestamp())`, rid, id, resultDigest(r)); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO agent_task_result_parts(id,result_id,ordinal,kind,text_value) VALUES($1,$2,0,'text',$3)`, r.Parts[0].ID, rid, response); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET state='completed',response=$2,version=version+1,updated_at=clock_timestamp(),completed_at=clock_timestamp() WHERE id=$1`, id, response); err != nil {
			return err
		}
		return tx.Commit()
	}
	current, err := s.transition(ctx, a, id, v, `UPDATE agent_tasks SET state='completed',response=$5,version=version+1,updated_at=clock_timestamp(),completed_at=clock_timestamp() WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3 AND version=$4 AND state IN ('dispatched','canceling')`, response)
	if errors.Is(err, ErrConflict) && current.State == StateCompleted && current.Response == response {
		return nil
	}
	return err
}

func (s *PostgresStore) ReportProgress(ctx context.Context, a AgentRef, id, attemptID string, u ProgressUpdate) (*Progress, bool, error) {
	if err := validateProgress(s.limits, attemptID, u); err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var state State
	var token string
	if err = tx.QueryRowContext(ctx, `SELECT t.state,d.attempt_token FROM agent_tasks t JOIN agent_task_dispatches d ON d.task_id=t.id WHERE t.id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3 FOR UPDATE OF t`, id, a.Namespace, a.Name).Scan(&state, &token); errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	} else if err != nil {
		return nil, false, err
	}
	if token == "" || token != attemptID {
		return nil, false, ErrInvalidAttempt
	}
	var prior []byte
	var created time.Time
	err = tx.QueryRowContext(ctx, `SELECT safe_summary,created_at FROM agent_task_updates WHERE task_id=$1 AND update_id=$2`, id, u.UpdateID).Scan(&prior, &created)
	if err == nil {
		var old struct {
			Message string `json:"message"`
			Percent *int   `json:"percent,omitempty"`
			Digest  string `json:"digest"`
		}
		if json.Unmarshal(prior, &old) != nil || old.Digest != progressDigest(u) {
			return nil, false, ErrUpdateConflict
		}
		return &Progress{Message: old.Message, Percent: old.Percent, UpdatedAt: created.UTC()}, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	if state != StateDispatched {
		return nil, false, ErrConflict
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_task_updates WHERE task_id=$1 AND kind='progress'`, id).Scan(&count); err != nil {
		return nil, false, err
	}
	if count >= s.limits.MaxProgressUpdates {
		return nil, false, ErrUpdateLimit
	}
	summary, _ := json.Marshal(struct {
		Message string `json:"message"`
		Percent *int   `json:"percent,omitempty"`
		Digest  string `json:"digest"`
	}{u.Message, u.Percent, progressDigest(u)})
	var now time.Time
	err = tx.QueryRowContext(ctx, `WITH seq AS (SELECT COALESCE(max(sequence),0)+1 n FROM agent_task_updates WHERE task_id=$1) INSERT INTO agent_task_updates(task_id,sequence,update_id,kind,safe_summary,created_at) SELECT $1,n,$2,'progress',$3,clock_timestamp() FROM seq RETURNING created_at`, id, u.UpdateID, summary).Scan(&now)
	if err != nil {
		return nil, false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET progress_message=$2,progress_percent=$3,progress_updated_at=$4,updated_at=$4,version=version+1 WHERE id=$1`, id, u.Message, u.Percent, now)
	if err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return &Progress{Message: u.Message, Percent: cloneInt(u.Percent), UpdatedAt: now.UTC()}, false, nil
}

func (s *PostgresStore) PublishResult(ctx context.Context, a AgentRef, id, attemptID string, r Result) (*Result, bool, error) {
	if err := validateResult(s.limits, attemptID, r); err != nil {
		return nil, false, err
	}
	digest := resultDigest(r)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var state State
	var token string
	var retainUntil time.Time
	if err = tx.QueryRowContext(ctx, `SELECT t.state,d.attempt_token,t.retain_until FROM agent_tasks t JOIN agent_task_dispatches d ON d.task_id=t.id WHERE t.id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3 FOR UPDATE OF t`, id, a.Namespace, a.Name).Scan(&state, &token, &retainUntil); errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	} else if err != nil {
		return nil, false, err
	}
	if token == "" || token != attemptID {
		return nil, false, ErrInvalidAttempt
	}
	var oldDigest string
	err = tx.QueryRowContext(ctx, `SELECT content_digest FROM agent_task_results WHERE task_id=$1 AND id=$2`, id, r.ID).Scan(&oldDigest)
	if err == nil {
		if oldDigest != digest {
			return nil, false, ErrResultConflict
		}
		old, er := loadResults(ctx, tx, id)
		if er != nil {
			return nil, false, er
		}
		for i := range old {
			if old[i].ID == r.ID {
				return &old[i], true, nil
			}
		}
		return nil, false, ErrResultConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	if state != StateDispatched {
		return nil, false, ErrConflict
	}
	var count int
	var fileBytes int64
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_task_results WHERE task_id=$1`, id).Scan(&count); err != nil {
		return nil, false, err
	}
	if count >= s.limits.MaxResults {
		return nil, false, ErrResultLimit
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(size_bytes),0) FROM agent_task_objects WHERE task_id=$1 AND status='ready'`, id).Scan(&fileBytes); err != nil {
		return nil, false, err
	}
	for _, p := range r.Parts {
		if p.File != nil {
			fileBytes += p.File.SizeBytes
		}
	}
	if fileBytes > s.limits.MaxTaskFileBytes {
		return nil, false, ErrResultTooLarge
	}
	var now time.Time
	if err = tx.QueryRowContext(ctx, `INSERT INTO agent_task_results(id,task_id,name,description,content_digest,created_at) VALUES($1,$2,$3,$4,$5,clock_timestamp()) RETURNING created_at`, r.ID, id, r.Name, r.Description, digest).Scan(&now); err != nil {
		var pe *pq.Error
		if errors.As(err, &pe) && pe.Code == "23505" {
			return nil, false, ErrResultConflict
		}
		return nil, false, err
	}
	for i, p := range r.Parts {
		var textValue any
		var jsonValue any
		var objectID any
		if p.Kind == PartText {
			textValue = p.Text
		}
		if p.Kind == PartJSON {
			jsonValue = []byte(p.JSON)
		}
		if p.File != nil {
			res, updateErr := tx.ExecContext(ctx, `UPDATE agent_task_objects SET status='ready',filename=$3,media_type=$4,size_bytes=$5,sha256=$6,scan_status=$7,retain_until=$8,ready_at=clock_timestamp(),lease_owner=NULL,lease_until=NULL WHERE id=$1 AND task_id=$2 AND status='pending'`, p.File.ObjectID, id, p.File.Filename, p.File.MediaType, p.File.SizeBytes, p.File.SHA256, p.File.ScanStatus, retainUntil)
			if updateErr != nil {
				return nil, false, updateErr
			}
			if changed, _ := res.RowsAffected(); changed != 1 {
				return nil, false, ErrConflict
			}
			objectID = p.File.ObjectID
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO agent_task_result_parts(id,result_id,ordinal,kind,text_value,json_value,object_id) VALUES($1,$2,$3,$4,$5,$6,$7)`, p.ID, r.ID, i, p.Kind, textValue, jsonValue, objectID); err != nil {
			return nil, false, err
		}
	}
	summary, _ := json.Marshal(map[string]any{"resultId": r.ID, "name": r.Name, "digest": digest})
	if _, err = tx.ExecContext(ctx, `WITH seq AS (SELECT COALESCE(max(sequence),0)+1 n FROM agent_task_updates WHERE task_id=$1) INSERT INTO agent_task_updates(task_id,sequence,update_id,kind,safe_summary,created_at) SELECT $1,n,$2,'result',$3,clock_timestamp() FROM seq`, id, r.ID, summary); err != nil {
		return nil, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET updated_at=clock_timestamp(),version=version+1 WHERE id=$1`, id); err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	r.ContentDigest = digest
	r.CreatedAt = now.UTC()
	out := cloneResults([]Result{r})[0]
	return &out, false, nil
}

// PrepareFileUpload records an object before any bytes are sent to the object
// provider. A crashed upload is therefore discoverable and is reclaimed after
// its short lease expires.
func (s *PostgresStore) PrepareFileUpload(ctx context.Context, a AgentRef, taskID, attemptID string, f PendingFile) error {
	if attemptID == "" || f.ObjectID == "" || f.ResultID == "" || strings.TrimSpace(f.Name) == "" || f.Filename == "" || f.MediaType == "" || f.SizeBytes < 0 || f.SizeBytes > s.limits.MaxFileBytes || len([]byte(f.Name)) > s.limits.MaxResultNameBytes || len([]byte(f.Filename)) > s.limits.MaxFilenameBytes {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state State
	var token string
	var retainUntil time.Time
	if err = tx.QueryRowContext(ctx, `SELECT t.state,d.attempt_token,t.retain_until FROM agent_tasks t JOIN agent_task_dispatches d ON d.task_id=t.id WHERE t.id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3 FOR UPDATE OF t`, taskID, a.Namespace, a.Name).Scan(&state, &token, &retainUntil); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if token == "" || token != attemptID {
		return ErrInvalidAttempt
	}
	if state != StateDispatched {
		return ErrConflict
	}
	var resultCount, existingID, existingName int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE id=$2),count(*) FILTER (WHERE name=$3 AND id<>$2) FROM agent_task_results WHERE task_id=$1`, taskID, f.ResultID, f.Name).Scan(&resultCount, &existingID, &existingName); err != nil {
		return err
	}
	if existingName != 0 {
		return ErrResultConflict
	}
	if existingID == 0 && resultCount >= s.limits.MaxResults {
		return ErrResultLimit
	}
	var fileBytes int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(size_bytes),0) FROM agent_task_objects WHERE task_id=$1 AND status IN ('pending','ready')`, taskID).Scan(&fileBytes); err != nil {
		return err
	}
	if fileBytes+f.SizeBytes > s.limits.MaxTaskFileBytes {
		return ErrResultTooLarge
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_task_objects(id,task_id,object_key,status,filename,media_type,size_bytes,retain_until,created_at,lease_until,next_attempt_at) VALUES($1,$2,$1,'pending',$3,$4,$5,$6,clock_timestamp(),clock_timestamp()+interval '30 minutes',clock_timestamp())`, f.ObjectID, taskID, f.Filename, f.MediaType, f.SizeBytes, retainUntil)
	if err != nil {
		var pe *pq.Error
		if errors.As(err, &pe) && pe.Code == "23505" {
			return ErrConflict
		}
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) AbandonFileUpload(ctx context.Context, objectID string) error {
	if objectID == "" {
		return ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, `UPDATE agent_task_objects SET status='deleting',lease_owner=NULL,lease_until=NULL,next_attempt_at=clock_timestamp() WHERE id=$1 AND status='pending'`, objectID)
	return err
}
