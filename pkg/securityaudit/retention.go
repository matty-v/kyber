package securityaudit

import (
	"context"
	"log/slog"
	"strconv"
	"time"
)

type RetentionPolicy struct {
	Days       int
	MaxRecords int64
}

func DefaultRetention() RetentionPolicy { return RetentionPolicy{Days: 30, MaxRecords: 1_000_000} }

// ParseRetention returns only bounded error categories, never a possibly
// sensitive mistyped environment value.
func ParseRetention(days, records string) (RetentionPolicy, error) {
	p := DefaultRetention()
	var err error
	if days != "" {
		p.Days, err = strconv.Atoi(days)
		if err != nil {
			return p, ErrInvalid
		}
	}
	if records != "" {
		p.MaxRecords, err = strconv.ParseInt(records, 10, 64)
		if err != nil {
			return p, ErrInvalid
		}
	}
	return p, p.Validate()
}

func (p RetentionPolicy) Validate() error {
	if p.Days < 1 || p.Days > 365 || p.MaxRecords < 1000 || p.MaxRecords > 100_000_000 {
		return ErrInvalid
	}
	return nil
}

type RetentionPruner interface {
	EnforceRetention(context.Context, string, time.Time, int64, int) (int64, error)
}

// SweepRetention is bounded by batches and the caller's context, leaving normal
// operator requests room to run. Row limits bound event growth under sustained
// denied traffic; the later history API must report actual retained coverage.
func SweepRetention(ctx context.Context, p RetentionPruner, namespace string, policy RetentionPolicy, now time.Time) (int64, error) {
	if p == nil {
		return 0, ErrUnavailable
	}
	if err := policy.Validate(); err != nil {
		return 0, err
	}
	if !namespaceName.MatchString(namespace) || now.IsZero() {
		return 0, ErrInvalid
	}
	cutoff := now.UTC().Add(-time.Duration(policy.Days) * 24 * time.Hour)
	var total int64
	for batch := 0; batch < 100; batch++ {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := p.EnforceRetention(ctx, namespace, cutoff, policy.MaxRecords, 1000)
		total += n
		if err != nil {
			return total, err
		}
		if n < 1000 {
			return total, nil
		}
	}
	return total, nil
}

// RunRetention runs until shutdown. A failure is visible and retried on the
// next tick, never represented as successful cleanup. Multiple replicas may
// safely prune bounded batches from the same installation.
func RunRetention(ctx context.Context, p RetentionPruner, namespace string, policy RetentionPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if !namespaceName.MatchString(namespace) {
		return ErrInvalid
	}
	if p == nil {
		return ErrUnavailable
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		sweepCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := SweepRetention(sweepCtx, p, namespace, policy, time.Now())
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("security history retention unavailable", "namespace", namespace)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
