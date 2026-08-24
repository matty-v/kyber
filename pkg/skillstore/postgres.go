package skillstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	_ "github.com/lib/pq" // PostgreSQL driver

	"github.com/matty-v/kyber/pkg/skillscan"
)

// PostgresStore is the durable Store, used whenever KYBER_POSTGRES_URL is set.
// It shares the database the session-brief store already uses; the table is
// created by Migrate at control-plane startup.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore returns a PostgresStore over an already-open database. The
// caller owns the connection's lifecycle.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// Migrate creates the agent_skills table if it does not exist. Safe on every
// startup.
func (s *PostgresStore) Migrate(ctx context.Context) error {
	const schema = `
	CREATE TABLE IF NOT EXISTS agent_skills (
		agent_name TEXT PRIMARY KEY,
		report_json JSONB NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);`
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

// Put upserts the agent's most recent report.
func (s *PostgresStore) Put(ctx context.Context, agentName string, report *skillscan.Report) error {
	if report == nil {
		return errNilReport
	}
	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshaling skill report: %w", err)
	}
	const query = `
		INSERT INTO agent_skills (agent_name, report_json, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (agent_name)
		DO UPDATE SET report_json = EXCLUDED.report_json, updated_at = now()
	`
	if _, err := s.db.ExecContext(ctx, query, agentName, data); err != nil {
		return fmt.Errorf("upserting skill report for %q: %w", agentName, err)
	}
	return nil
}

// Get returns the stored report, or ErrNotFound when the agent has never
// reported.
func (s *PostgresStore) Get(ctx context.Context, agentName string) (*skillscan.Report, error) {
	const query = `SELECT report_json FROM agent_skills WHERE agent_name = $1`
	var raw string
	if err := s.db.QueryRowContext(ctx, query, agentName).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("querying skill report for %q: %w", agentName, err)
	}
	var rep skillscan.Report
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		return nil, fmt.Errorf("unmarshaling skill report for %q: %w", agentName, err)
	}
	return &rep, nil
}

// Delete removes the agent's report. Deleting a row that does not exist is not
// an error.
func (s *PostgresStore) Delete(ctx context.Context, agentName string) error {
	const query = `DELETE FROM agent_skills WHERE agent_name = $1`
	if _, err := s.db.ExecContext(ctx, query, agentName); err != nil {
		return fmt.Errorf("deleting skill report for %q: %w", agentName, err)
	}
	return nil
}
