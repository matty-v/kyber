package archivejob

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// PostgresStore is the durable Store. The whole job is a JSONB document; the
// columns beside it exist for indexing, the one-active-job-per-agent unique
// index, and the version check.
type PostgresStore struct {
	db *sql.DB
}

func NewPostgresStore(db *sql.DB) *PostgresStore { return &PostgresStore{db: db} }

// Migrate creates the table and indexes. Safe on every startup.
func (s *PostgresStore) Migrate(ctx context.Context) error {
	const schema = `
	CREATE TABLE IF NOT EXISTS agent_archive_jobs (
		id TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		agent TEXT NOT NULL,
		state TEXT NOT NULL,
		version BIGINT NOT NULL,
		expires_at TIMESTAMPTZ,
		created_at TIMESTAMPTZ NOT NULL,
		job JSONB NOT NULL
	);
	DROP INDEX IF EXISTS agent_archive_jobs_one_active;
	CREATE UNIQUE INDEX IF NOT EXISTS agent_archive_jobs_one_active_per_agent
		ON agent_archive_jobs (kind, agent) WHERE state IN ('queued', 'running') AND agent <> '';
	CREATE INDEX IF NOT EXISTS agent_archive_jobs_by_agent
		ON agent_archive_jobs (kind, agent, created_at DESC);`
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

func (s *PostgresStore) Create(ctx context.Context, j *Job, maxActive int) error {
	j.Version = 1
	doc, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("marshaling job: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting job transaction: %w", err)
	}
	defer tx.Rollback()
	if maxActive > 0 {
		// Serialize creators so the installation-wide count cannot be raced
		// past; readers and the worker's updates are unaffected.
		if _, err := tx.ExecContext(ctx, `LOCK TABLE agent_archive_jobs IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return fmt.Errorf("locking job table: %w", err)
		}
		var active int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM agent_archive_jobs WHERE state IN ('queued', 'running')`).Scan(&active); err != nil {
			return fmt.Errorf("counting active jobs: %w", err)
		}
		if active >= maxActive {
			var sameAgent bool
			_ = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agent_archive_jobs
				WHERE kind = $1 AND agent = $2 AND agent <> '' AND state IN ('queued', 'running'))`, j.Kind, j.Agent).Scan(&sameAgent)
			if sameAgent {
				return ErrActiveJob
			}
			return ErrConcurrency
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_archive_jobs
		(id, kind, agent, state, version, expires_at, created_at, job)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		j.ID, j.Kind, j.Agent, j.State, j.Version, j.ExpiresAt, j.CreatedAt, doc)
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return ErrActiveJob
	}
	if err != nil {
		return fmt.Errorf("inserting job: %w", err)
	}
	return tx.Commit()
}

func scanJobs(rows *sql.Rows) ([]*Job, error) {
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scanning job: %w", err)
		}
		var j Job
		if err := json.Unmarshal(raw, &j); err != nil {
			return nil, fmt.Errorf("decoding job: %w", err)
		}
		out = append(out, &j)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Get(ctx context.Context, id string) (*Job, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT job FROM agent_archive_jobs WHERE id = $1`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading job: %w", err)
	}
	var j Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("decoding job: %w", err)
	}
	return &j, nil
}

func (s *PostgresStore) List(ctx context.Context, kind Kind, agent string, limit int) ([]*Job, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT job FROM agent_archive_jobs
		WHERE kind = $1 AND ($2 = '' OR agent = $2) ORDER BY created_at DESC LIMIT $3`, kind, agent, limit)
	if err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}
	return scanJobs(rows)
}

func (s *PostgresStore) ListActive(ctx context.Context, now time.Time) ([]*Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job FROM agent_archive_jobs
		WHERE state IN ('queued', 'running') OR (state = 'completed' AND expires_at <= $1)
		ORDER BY created_at`, now)
	if err != nil {
		return nil, fmt.Errorf("listing active jobs: %w", err)
	}
	return scanJobs(rows)
}

func (s *PostgresStore) Update(ctx context.Context, j *Job) error {
	prev := j.Version
	j.Version++
	j.UpdatedAt = time.Now().UTC()
	doc, err := json.Marshal(j)
	if err != nil {
		j.Version = prev
		return fmt.Errorf("marshaling job: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE agent_archive_jobs
		SET state = $1, version = $2, expires_at = $3, job = $4
		WHERE id = $5 AND version = $6`, j.State, j.Version, j.ExpiresAt, doc, j.ID, prev)
	if err != nil {
		j.Version = prev
		return fmt.Errorf("updating job: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		j.Version = prev
		if _, err := s.Get(ctx, j.ID); errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return ErrConflict
	}
	return nil
}
