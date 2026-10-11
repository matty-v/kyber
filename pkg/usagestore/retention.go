package usagestore

import (
	"context"
	"fmt"
	"time"
)

type Retention struct {
	DetailDays  int
	DailyMonths int
}

func DefaultRetention() Retention { return Retention{DetailDays: 90, DailyMonths: 13} }

func (r Retention) Validate() error {
	if r.DetailDays < 1 || r.DetailDays > 3650 || r.DailyMonths < 1 || r.DailyMonths > 120 || r.DailyMonths*28 < r.DetailDays {
		return ErrInvalid
	}
	return nil
}

// Prune retains detailed evidence for DetailDays and daily aggregates for
// DailyMonths. Supersession chains are removed from the leaves inward, so a
// correction cannot leave a dangling foreign key. Aggregates already include
// only accepted events and are not recomputed when detail is pruned.
func (s *PostgresStore) Prune(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		return ErrInvalid
	}
	detailCutoff := now.UTC().Add(-time.Duration(s.retention.DetailDays) * 24 * time.Hour)
	dailyCutoff := now.UTC().AddDate(0, -s.retention.DailyMonths, 0).Format("2006-01-02")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting usage retention: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(9101172026)`); err != nil {
		return fmt.Errorf("locking usage sequence: %w", err)
	}
	for {
		result, err := tx.ExecContext(ctx, `DELETE FROM agent_usage_events e WHERE e.observed_at < $1
			AND NOT EXISTS (SELECT 1 FROM agent_usage_events child WHERE child.supersedes_id=e.id)`, detailCutoff)
		if err != nil {
			return fmt.Errorf("pruning usage events: %w", err)
		}
		removed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting pruned usage events: %w", err)
		}
		if removed == 0 {
			break
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_usage_gaps WHERE detected_at < $1`, detailCutoff); err != nil {
		return fmt.Errorf("pruning usage gaps: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_usage_daily WHERE day < $1`, dailyCutoff); err != nil {
		return fmt.Errorf("pruning daily usage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_usage_cursors WHERE updated_at < $1`, now.UTC().AddDate(0, -s.retention.DailyMonths, 0)); err != nil {
		return fmt.Errorf("pruning usage cursors: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing usage retention: %w", err)
	}
	return nil
}
