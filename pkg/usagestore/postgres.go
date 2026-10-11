package usagestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// PostgresStore is the durable evidence store. A single transaction lock
// serializes commits so a future pagination snapshot can pin a sequence
// without a lower-sequence transaction committing after that snapshot.
type PostgresStore struct{ db *sql.DB }

func NewPostgresStore(db *sql.DB) (*PostgresStore, error) {
	if db == nil {
		return nil, ErrUnavailable
	}
	return &PostgresStore{db: db}, nil
}

func (s *PostgresStore) Migrate(ctx context.Context) error {
	const schema = `
	CREATE TABLE IF NOT EXISTS agent_usage_events (
		id UUID PRIMARY KEY,
		sequence BIGSERIAL UNIQUE,
		agent_uid TEXT NOT NULL,
		agent_name TEXT NOT NULL,
		runtime TEXT NOT NULL,
		source_identity TEXT NOT NULL,
		observed_at TIMESTAMPTZ NOT NULL,
		ingested_at TIMESTAMPTZ NOT NULL,
		supersedes_id UUID REFERENCES agent_usage_events(id),
		payload_hash BYTEA NOT NULL,
		document JSONB NOT NULL,
		UNIQUE(agent_uid,runtime,source_identity)
	);
	CREATE UNIQUE INDEX IF NOT EXISTS agent_usage_events_one_correction
		ON agent_usage_events(supersedes_id) WHERE supersedes_id IS NOT NULL;
	CREATE INDEX IF NOT EXISTS agent_usage_events_agent_time
		ON agent_usage_events(agent_uid,observed_at,id);
	CREATE INDEX IF NOT EXISTS agent_usage_events_ingested
		ON agent_usage_events(ingested_at,id);
	CREATE TABLE IF NOT EXISTS agent_usage_gaps (
		id UUID PRIMARY KEY,
		sequence BIGSERIAL UNIQUE,
		agent_uid TEXT NOT NULL,
		runtime TEXT NOT NULL,
		source_format TEXT NOT NULL,
		source_identity TEXT NOT NULL,
		detected_at TIMESTAMPTZ NOT NULL,
		reason TEXT NOT NULL,
		UNIQUE(agent_uid,runtime,source_identity)
	);
	CREATE INDEX IF NOT EXISTS agent_usage_gaps_agent_time
		ON agent_usage_gaps(agent_uid,detected_at);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrating usage evidence: %w", err)
	}
	return nil
}

// Ingest inserts one immutable event. Retrying the same source identity and
// payload returns the original acknowledgment. A different payload under the
// same identity is a conflict that the caller must surface as a coverage gap.
func (s *PostgresStore) Ingest(ctx context.Context, input Event) (Event, bool, error) {
	if err := input.Validate(); err != nil {
		return Event{}, false, err
	}
	input.ObservedAt = input.ObservedAt.UTC()
	if input.SupersedesID != "" {
		if _, err := uuid.Parse(input.SupersedesID); err != nil {
			return Event{}, false, ErrInvalid
		}
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		return Event{}, false, fmt.Errorf("marshaling usage input: %w", err)
	}
	hash := sha256.Sum256(canonical)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, false, fmt.Errorf("starting usage transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(9101172026)`); err != nil {
		return Event{}, false, fmt.Errorf("locking usage sequence: %w", err)
	}
	var existingRaw, existingHash []byte
	err = tx.QueryRowContext(ctx, `SELECT document,payload_hash FROM agent_usage_events WHERE agent_uid=$1 AND runtime=$2 AND source_identity=$3`,
		input.AgentUID, input.Runtime, input.SourceIdentity).Scan(&existingRaw, &existingHash)
	if err == nil {
		if !bytes.Equal(existingHash, hash[:]) {
			return Event{}, false, ErrConflict
		}
		var existing Event
		if err := json.Unmarshal(existingRaw, &existing); err != nil {
			return Event{}, false, fmt.Errorf("decoding existing usage event: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return Event{}, false, fmt.Errorf("committing usage retry: %w", err)
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Event{}, false, fmt.Errorf("checking usage identity: %w", err)
	}
	if input.SupersedesID != "" {
		var oldAgentUID string
		err = tx.QueryRowContext(ctx, `SELECT agent_uid FROM agent_usage_events WHERE id=$1`, input.SupersedesID).Scan(&oldAgentUID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && oldAgentUID != input.AgentUID) {
			return Event{}, false, ErrInvalid
		}
		if err != nil {
			return Event{}, false, fmt.Errorf("checking superseded usage event: %w", err)
		}
	}
	input.ID = uuid.NewString()
	input.IngestedAt = time.Now().UTC()
	document, err := json.Marshal(input)
	if err != nil {
		return Event{}, false, fmt.Errorf("marshaling usage event: %w", err)
	}
	var supersedes any
	if input.SupersedesID != "" {
		supersedes = input.SupersedesID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_usage_events
		(id,agent_uid,agent_name,runtime,source_identity,observed_at,ingested_at,supersedes_id,payload_hash,document)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		input.ID, input.AgentUID, input.Agent, input.Runtime, input.SourceIdentity, input.ObservedAt, input.IngestedAt, supersedes, hash[:], document)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return Event{}, false, ErrConflict
		}
		return Event{}, false, fmt.Errorf("inserting usage event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Event{}, false, fmt.Errorf("committing usage event: %w", err)
	}
	return input, true, nil
}

// RecordGap is idempotent for one source identity. It records detection, not
// an inferred number of lost calls.
func (s *PostgresStore) RecordGap(ctx context.Context, gap Gap) error {
	if err := gap.Validate(); err != nil {
		return err
	}
	gap.DetectedAt = gap.DetectedAt.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting usage gap transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(9101172026)`); err != nil {
		return fmt.Errorf("locking usage sequence: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_usage_gaps
		(id,agent_uid,runtime,source_format,source_identity,detected_at,reason)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT(agent_uid,runtime,source_identity) DO NOTHING`,
		uuid.NewString(), gap.AgentUID, gap.Runtime, gap.SourceFormat, gap.SourceIdentity, gap.DetectedAt, gap.Reason)
	if err != nil {
		return fmt.Errorf("recording usage gap: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing usage gap: %w", err)
	}
	return nil
}

// Snapshot pins both committed sequence horizons. Every writer takes the same
// transaction lock, so a later commit cannot appear below either bound.
func (s *PostgresStore) Snapshot(ctx context.Context) (eventSequence, gapSequence int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("starting usage snapshot: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(9101172026)`); err != nil {
		return 0, 0, fmt.Errorf("locking usage sequence: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(sequence),0) FROM agent_usage_events`).Scan(&eventSequence); err != nil {
		return 0, 0, fmt.Errorf("reading usage event horizon: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(sequence),0) FROM agent_usage_gaps`).Scan(&gapSequence); err != nil {
		return 0, 0, fmt.Errorf("reading usage gap horizon: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("committing usage snapshot: %w", err)
	}
	return eventSequence, gapSequence, nil
}

// DeleteAgent erases one immutable Agent lifetime, even if its name has since
// been reused. Corrections are deleted before their referenced originals.
func (s *PostgresStore) DeleteAgent(ctx context.Context, agentUID string) error {
	if agentUID == "" || len(agentUID) > 128 {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting usage deletion: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(9101172026)`); err != nil {
		return fmt.Errorf("locking usage sequence: %w", err)
	}
	// A correction can itself be corrected. Removing leaves from the chain
	// first handles every depth without assuming a fixed number of revisions.
	for {
		result, err := tx.ExecContext(ctx, `DELETE FROM agent_usage_events e WHERE e.agent_uid=$1
			AND NOT EXISTS (SELECT 1 FROM agent_usage_events child WHERE child.supersedes_id=e.id)`, agentUID)
		if err != nil {
			return fmt.Errorf("deleting usage events: %w", err)
		}
		removed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting deleted usage events: %w", err)
		}
		if removed == 0 {
			break
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_usage_gaps WHERE agent_uid=$1`, agentUID); err != nil {
		return fmt.Errorf("deleting usage gaps: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing usage deletion: %w", err)
	}
	return nil
}
