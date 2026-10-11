package usagestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// TokenAggregate has an exact integer partial sum. Sum is null when no
// accepted event reported the class; UnknownEvents makes partial sums visible
// rather than presenting them as complete totals.
type TokenAggregate struct {
	Sum            *int64 `json:"sum"`
	ReportedEvents int64  `json:"reportedEvents"`
	UnknownEvents  int64  `json:"unknownEvents"`
}

type DailyAggregate struct {
	AgentUID                 string         `json:"agentUID"`
	Day                      string         `json:"day"`
	Runtime                  string         `json:"runtime"`
	Model                    string         `json:"model"`
	ConfiguredRoute          string         `json:"configuredRoute"`
	RouteID                  string         `json:"routeID"`
	ObservedUpstreamProvider *string        `json:"observedUpstreamProvider"`
	EventCount               int64          `json:"eventCount"`
	InputUncached            TokenAggregate `json:"inputUncached"`
	Output                   TokenAggregate `json:"output"`
	CacheRead                TokenAggregate `json:"cacheRead"`
	CacheWrite               TokenAggregate `json:"cacheWrite"`
}

func (a *TokenAggregate) apply(value *int64, sign int64) error {
	if value == nil {
		count, ok := checkedAdd(a.UnknownEvents, sign)
		if !ok || count < 0 {
			return ErrConflict
		}
		a.UnknownEvents = count
		return nil
	}
	count, ok := checkedAdd(a.ReportedEvents, sign)
	if !ok || count < 0 {
		return ErrConflict
	}
	base := int64(0)
	if a.Sum != nil {
		base = *a.Sum
	}
	sum, ok := checkedAdd(base, sign**value)
	if !ok || sum < 0 {
		return ErrConflict
	}
	a.ReportedEvents = count
	if count == 0 {
		a.Sum = nil
	} else {
		a.Sum = &sum
	}
	return nil
}

func checkedAdd(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

func (a *DailyAggregate) apply(e Event, sign int64) error {
	count, ok := checkedAdd(a.EventCount, sign)
	if !ok || count < 0 {
		return ErrConflict
	}
	a.EventCount = count
	for _, pair := range []struct {
		aggregate *TokenAggregate
		value     *int64
	}{
		{&a.InputUncached, e.InputUncached}, {&a.Output, e.Output}, {&a.CacheRead, e.CacheRead}, {&a.CacheWrite, e.CacheWrite},
	} {
		if err := pair.aggregate.apply(pair.value, sign); err != nil {
			return err
		}
	}
	return nil
}

func applyDaily(ctx context.Context, tx *sql.Tx, e Event, sign int64) error {
	day := e.ObservedAt.UTC().Format("2006-01-02")
	known, provider := false, ""
	if e.ObservedUpstreamProvider != nil {
		known, provider = true, *e.ObservedUpstreamProvider
	}
	args := []any{e.AgentUID, day, e.Runtime, e.Model, e.ConfiguredRoute, e.RouteID, known, provider}
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT document FROM agent_usage_daily WHERE
		agent_uid=$1 AND day=$2 AND runtime=$3 AND model=$4 AND configured_route=$5 AND route_id=$6
		AND upstream_provider_known=$7 AND upstream_provider=$8`, args...).Scan(&raw)
	var aggregate DailyAggregate
	if errors.Is(err, sql.ErrNoRows) {
		if sign < 0 {
			return fmt.Errorf("subtracting missing usage aggregate: %w", ErrConflict)
		}
		aggregate = DailyAggregate{AgentUID: e.AgentUID, Day: day, Runtime: e.Runtime, Model: e.Model,
			ConfiguredRoute: e.ConfiguredRoute, RouteID: e.RouteID, ObservedUpstreamProvider: e.ObservedUpstreamProvider}
	} else if err != nil {
		return fmt.Errorf("reading usage aggregate: %w", err)
	} else if err := json.Unmarshal(raw, &aggregate); err != nil {
		return fmt.Errorf("decoding usage aggregate: %w", err)
	}
	if err := aggregate.apply(e, sign); err != nil {
		return fmt.Errorf("applying usage aggregate: %w", err)
	}
	if aggregate.EventCount == 0 {
		_, err = tx.ExecContext(ctx, `DELETE FROM agent_usage_daily WHERE
			agent_uid=$1 AND day=$2 AND runtime=$3 AND model=$4 AND configured_route=$5 AND route_id=$6
			AND upstream_provider_known=$7 AND upstream_provider=$8`, args...)
		if err != nil {
			return fmt.Errorf("deleting empty usage aggregate: %w", err)
		}
		return nil
	}
	document, err := json.Marshal(aggregate)
	if err != nil {
		return fmt.Errorf("marshaling usage aggregate: %w", err)
	}
	if len(raw) == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO agent_usage_daily
			(agent_uid,day,runtime,model,configured_route,route_id,upstream_provider_known,upstream_provider,document)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, append(args, document)...)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE agent_usage_daily SET document=$9 WHERE
			agent_uid=$1 AND day=$2 AND runtime=$3 AND model=$4 AND configured_route=$5 AND route_id=$6
			AND upstream_provider_known=$7 AND upstream_provider=$8`, append(args, document)...)
	}
	if err != nil {
		return fmt.Errorf("saving usage aggregate: %w", err)
	}
	return nil
}
