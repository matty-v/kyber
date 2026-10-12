package usagestore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

var ErrReadSnapshotExpired = errors.New("usagestore: read snapshot expired or changed")
var ErrReadSnapshotTooLarge = errors.New("usagestore: aggregate read snapshot too large")

const maxDailySnapshotRows = 20000

// ReadFilter is an already-authorized, immutable Agent-lifetime query. The
// caller must resolve current Agent UIDs before passing it to this package.
type ReadFilter struct {
	AgentUIDs                                 []string
	Start, End                                time.Time
	Runtime, Model, RouteID, UpstreamProvider string
	ProviderUnknown                           bool
}

func (f ReadFilter) validate() error {
	if len(f.AgentUIDs) == 0 || len(f.AgentUIDs) > 1000 || f.Start.IsZero() ||
		f.End.IsZero() || !f.Start.Before(f.End) ||
		f.End.Sub(f.Start) > 400*24*time.Hour {
		return ErrInvalid
	}
	return nil
}

type EventPage struct {
	Events         []Event   `json:"events"`
	NextIngestedAt time.Time `json:"-"`
	NextID         string    `json:"-"`
	HasMore        bool      `json:"-"`
	GapCount       int64     `json:"gapCount"`
}

func (s *PostgresStore) CreateEventSnapshot(ctx context.Context, owner string, queryHash [32]byte) (string, int64, int64, error) {
	if owner == "" {
		return "", 0, 0, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return "", 0, 0, fmt.Errorf("starting event read snapshot: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(9101172026)`); err != nil {
		return "", 0, 0, fmt.Errorf("locking event read snapshot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_usage_read_snapshots WHERE expires_at<clock_timestamp()`); err != nil {
		return "", 0, 0, fmt.Errorf("pruning read snapshots: %w", err)
	}
	var eventSequence, gapSequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(sequence),0) FROM agent_usage_events`).Scan(&eventSequence); err != nil {
		return "", 0, 0, fmt.Errorf("reading event horizon: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(sequence),0) FROM agent_usage_gaps`).Scan(&gapSequence); err != nil {
		return "", 0, 0, fmt.Errorf("reading gap horizon: %w", err)
	}
	id := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_usage_read_snapshots(id,owner_key,query_hash,event_sequence,gap_sequence,expires_at)
		VALUES($1,$2,$3,$4,$5,clock_timestamp()+interval '1 hour')`, id, owner, queryHash[:], eventSequence, gapSequence); err != nil {
		return "", 0, 0, fmt.Errorf("saving event read snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", 0, 0, fmt.Errorf("committing event read snapshot: %w", err)
	}
	return id, eventSequence, gapSequence, nil
}

func (s *PostgresStore) EventSnapshot(ctx context.Context, id, owner string, queryHash [32]byte) (int64, int64, error) {
	if _, err := uuid.Parse(id); err != nil {
		return 0, 0, ErrInvalid
	}
	var storedOwner string
	var storedHash []byte
	var eventSequence, gapSequence int64
	err := s.db.QueryRowContext(ctx, `SELECT owner_key,query_hash,event_sequence,gap_sequence
		FROM agent_usage_read_snapshots WHERE id=$1 AND expires_at>clock_timestamp()`, id).Scan(&storedOwner, &storedHash, &eventSequence, &gapSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrReadSnapshotExpired
	}
	if err != nil {
		return 0, 0, fmt.Errorf("reading event read snapshot: %w", err)
	}
	if storedOwner != owner || !bytes.Equal(storedHash, queryHash[:]) {
		return 0, 0, ErrReadSnapshotExpired
	}
	return eventSequence, gapSequence, nil
}

// ListEvents reads only rows committed under the pinned sequence horizons.
// Superseded events are hidden only when the correction was already committed
// at that horizon, so a later correction never mutates an older page.
func (s *PostgresStore) ListEvents(ctx context.Context, f ReadFilter, eventSequence, gapSequence int64,
	afterTime time.Time, afterID string, limit int) (EventPage, error) {
	if err := f.validate(); err != nil || eventSequence < 0 || gapSequence < 0 || limit < 1 || limit > 500 {
		return EventPage{}, ErrInvalid
	}
	page := EventPage{Events: make([]Event, 0, limit)}
	rows, err := s.db.QueryContext(ctx, `SELECT e.document,e.ingested_at,e.id FROM agent_usage_events e
		WHERE e.sequence<=$1 AND e.agent_uid=ANY($2) AND e.observed_at>=$3 AND e.observed_at<$4
		AND ($5='' OR e.runtime=$5) AND ($6='' OR e.document->>'model'=$6)
		AND ($7='' OR e.document->>'routeID'=$7)
		AND ($8='' AND NOT $9 OR ($9 AND e.document->>'observedUpstreamProvider' IS NULL)
			OR (NOT $9 AND $8<>'' AND e.document->>'observedUpstreamProvider'=$8))
		AND ($10='' OR (e.ingested_at,e.id)>($11,NULLIF($10,'')::uuid))
		AND NOT EXISTS (SELECT 1 FROM agent_usage_events correction
			WHERE correction.supersedes_id=e.id AND correction.sequence<=$1)
		ORDER BY e.ingested_at,e.id LIMIT $12`,
		eventSequence, pq.Array(f.AgentUIDs), f.Start, f.End, f.Runtime, f.Model,
		f.RouteID, f.UpstreamProvider, f.ProviderUnknown, afterID, afterTime, limit+1)
	if err != nil {
		return EventPage{}, fmt.Errorf("listing usage events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var ingestedAt time.Time
		var id string
		if err := rows.Scan(&raw, &ingestedAt, &id); err != nil {
			return EventPage{}, fmt.Errorf("scanning usage event: %w", err)
		}
		if len(page.Events) == limit {
			page.HasMore = true
			break
		}
		var event Event
		if err := json.Unmarshal(raw, &event); err != nil {
			return EventPage{}, fmt.Errorf("decoding usage event: %w", err)
		}
		page.Events = append(page.Events, event)
		page.NextIngestedAt, page.NextID = ingestedAt.UTC(), id
	}
	if err := rows.Err(); err != nil {
		return EventPage{}, fmt.Errorf("reading usage events: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM agent_usage_gaps
		WHERE sequence<=$1 AND agent_uid=ANY($2) AND detected_at>=$3 AND detected_at<$4
		AND ($5='' OR runtime=$5)`, gapSequence, pq.Array(f.AgentUIDs), f.Start, f.End, f.Runtime).Scan(&page.GapCount); err != nil {
		return EventPage{}, fmt.Errorf("counting usage gaps: %w", err)
	}
	return page, nil
}

// CreateDailySnapshot freezes matching aggregate documents under the same
// writer lock used by ingest and correction. Pagination then reads immutable
// rows, even after a later correction changes the live daily table.
func (s *PostgresStore) CreateDailySnapshot(ctx context.Context, owner string, queryHash [32]byte, f ReadFilter) (string, error) {
	if err := f.validate(); err != nil || owner == "" {
		return "", ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("starting daily usage snapshot: %w", err)
	}
	defer tx.Rollback()
	// DeleteAgent takes this lock and invalidates all read snapshots. Hold it
	// through materialization so a deletion cannot race a stale snapshot insert.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(9101172026)`); err != nil {
		return "", fmt.Errorf("locking daily usage snapshot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_usage_read_snapshots WHERE expires_at<clock_timestamp()`); err != nil {
		return "", fmt.Errorf("pruning read snapshots: %w", err)
	}
	id := uuid.NewString()
	var gapCount int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_usage_gaps WHERE agent_uid=ANY($1)
		AND detected_at >= $2 AND detected_at < $3 AND ($4='' OR runtime=$4)`,
		pq.Array(f.AgentUIDs), f.Start, f.End, f.Runtime).Scan(&gapCount); err != nil {
		return "", fmt.Errorf("counting daily usage gaps: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_usage_read_snapshots(id,owner_key,query_hash,gap_count,expires_at)
		VALUES($1,$2,$3,$4,clock_timestamp()+interval '1 hour')`, id, owner, queryHash[:], gapCount); err != nil {
		return "", fmt.Errorf("creating daily usage snapshot: %w", err)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO agent_usage_read_snapshot_rows(snapshot_id,ordinal,document)
		SELECT $1,row_number() OVER (ORDER BY d.day,d.agent_uid,d.runtime,d.model,d.configured_route,d.route_id,d.upstream_provider_known,d.upstream_provider),d.document
		FROM (SELECT * FROM agent_usage_daily WHERE agent_uid=ANY($2) AND day >= $3::date AND day < $4::date
		AND ($5='' OR runtime=$5) AND ($6='' OR model=$6) AND ($7='' OR route_id=$7)
		AND ($8='' AND NOT $9 OR ($9 AND NOT upstream_provider_known)
			OR (NOT $9 AND $8<>'' AND upstream_provider_known AND upstream_provider=$8))
		ORDER BY day,agent_uid,runtime,model,configured_route,route_id,upstream_provider_known,upstream_provider LIMIT 20001) d`,
		id, pq.Array(f.AgentUIDs), f.Start.UTC().Format("2006-01-02"), f.End.UTC().Format("2006-01-02"),
		f.Runtime, f.Model, f.RouteID, f.UpstreamProvider, f.ProviderUnknown)
	if err != nil {
		return "", fmt.Errorf("materializing daily usage snapshot: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("counting daily usage rows: %w", err)
	}
	if count > maxDailySnapshotRows {
		return "", ErrReadSnapshotTooLarge
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("committing daily usage snapshot: %w", err)
	}
	return id, nil
}

type DailyPage struct {
	Rows        []DailyAggregate `json:"rows"`
	GapCount    int64            `json:"gapCount"`
	NextOrdinal int64            `json:"-"`
	HasMore     bool             `json:"-"`
}

func (s *PostgresStore) ListDailySnapshot(ctx context.Context, id, owner string, queryHash [32]byte, after int64, limit int) (DailyPage, error) {
	if _, err := uuid.Parse(id); err != nil || owner == "" || after < 0 || limit < 1 || limit > 500 {
		return DailyPage{}, ErrInvalid
	}
	var storedOwner string
	var storedHash []byte
	var gapCount int64
	err := s.db.QueryRowContext(ctx, `SELECT owner_key,query_hash,gap_count FROM agent_usage_read_snapshots
		WHERE id=$1 AND expires_at>clock_timestamp()`, id).Scan(&storedOwner, &storedHash, &gapCount)
	if errors.Is(err, sql.ErrNoRows) {
		return DailyPage{}, ErrReadSnapshotExpired
	}
	if err != nil {
		return DailyPage{}, fmt.Errorf("reading daily snapshot owner: %w", err)
	}
	if storedOwner != owner || !bytes.Equal(storedHash, queryHash[:]) {
		return DailyPage{}, ErrReadSnapshotExpired
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ordinal,document FROM agent_usage_read_snapshot_rows
		WHERE snapshot_id=$1 AND ordinal>$2 ORDER BY ordinal LIMIT $3`, id, after, limit+1)
	if err != nil {
		return DailyPage{}, fmt.Errorf("reading daily usage snapshot: %w", err)
	}
	defer rows.Close()
	page := DailyPage{Rows: make([]DailyAggregate, 0, limit), GapCount: gapCount}
	for rows.Next() {
		var ordinal int64
		var raw []byte
		if err := rows.Scan(&ordinal, &raw); err != nil {
			return DailyPage{}, fmt.Errorf("scanning daily usage: %w", err)
		}
		if len(page.Rows) == limit {
			page.HasMore = true
			break
		}
		var aggregate DailyAggregate
		if err := json.Unmarshal(raw, &aggregate); err != nil {
			return DailyPage{}, fmt.Errorf("decoding daily usage: %w", err)
		}
		page.Rows = append(page.Rows, aggregate)
		page.NextOrdinal = ordinal
	}
	if err := rows.Err(); err != nil {
		return DailyPage{}, fmt.Errorf("iterating daily usage: %w", err)
	}
	return page, nil
}
