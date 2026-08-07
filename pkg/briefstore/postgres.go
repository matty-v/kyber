package briefstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	_ "github.com/lib/pq" // PostgreSQL driver
)

// PostgresStore is a BriefStore backed by PostgreSQL.
//
// Schema (migration owned by a separate task — TBD):
//
//	CREATE TABLE session_briefs (
//	  agent_name TEXT PRIMARY KEY,
//	  brief_json JSONB NOT NULL,
//	  updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now()
//	);
//
// NOTE: This implementation is a scaffold for B2. It is not connected to a real
// database in the B2 deployment — the control plane uses MemoryStore for now.
// TODO(B-postgres): wire a real connection string and run migrations before enabling.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore returns a PostgresStore using the provided *sql.DB.
// The caller is responsible for opening and closing the database connection.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// Put upserts the brief for agentName in the session_briefs table.
func (s *PostgresStore) Put(ctx context.Context, agentName string, brief *Brief) error {
	data, err := json.Marshal(brief)
	if err != nil {
		return fmt.Errorf("marshaling brief: %w", err)
	}

	const query = `
		INSERT INTO session_briefs (agent_name, brief_json, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (agent_name)
		DO UPDATE SET brief_json = EXCLUDED.brief_json, updated_at = now()
	`
	if _, err := s.db.ExecContext(ctx, query, agentName, data); err != nil {
		return fmt.Errorf("upserting brief for %q: %w", agentName, err)
	}
	return nil
}

// Get retrieves the brief for agentName, returning ErrBriefNotFound if the row does not exist.
func (s *PostgresStore) Get(ctx context.Context, agentName string) (*Brief, error) {
	const query = `SELECT brief_json FROM session_briefs WHERE agent_name = $1`

	var rawJSON string
	if err := s.db.QueryRowContext(ctx, query, agentName).Scan(&rawJSON); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrBriefNotFound
		}
		return nil, fmt.Errorf("querying brief for %q: %w", agentName, err)
	}

	var brief Brief
	if err := json.Unmarshal([]byte(rawJSON), &brief); err != nil {
		return nil, fmt.Errorf("unmarshaling brief for %q: %w", agentName, err)
	}
	return &brief, nil
}

// Migrate creates the session_briefs table if it does not already exist.
// Safe to call on every startup — uses CREATE TABLE IF NOT EXISTS.
func (s *PostgresStore) Migrate(ctx context.Context) error {
	const schema = `
	CREATE TABLE IF NOT EXISTS session_briefs (
		agent_name TEXT PRIMARY KEY,
		brief_json JSONB NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);`
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

// Delete removes the brief for agentName. Returns nil if the row did not exist.
func (s *PostgresStore) Delete(ctx context.Context, agentName string) error {
	const query = `DELETE FROM session_briefs WHERE agent_name = $1`
	if _, err := s.db.ExecContext(ctx, query, agentName); err != nil {
		return fmt.Errorf("deleting brief for %q: %w", agentName, err)
	}
	return nil
}
