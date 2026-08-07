package briefstore

import (
	"context"
	"time"
)

// LogInfoFunc matches logr.Logger.Info and slog-style "msg, k=v" signatures,
// so callers can pass either without adapter glue.
type LogInfoFunc func(msg string, keysAndValues ...any)

// MigrateWithRetry runs migrate in a bounded retry loop. It returns nil on the
// first successful attempt. If migrate keeps failing, it retries every interval
// until total elapses (or ctx is cancelled), then returns the last error.
//
// This exists because the control-plane pod can come up before Postgres is
// accepting connections (e.g. during ArgoCD rolling syncs). A single-shot
// migrate that silently falls back to in-memory means briefs written during
// that pod's lifetime don't persist. See issue #60.
//
// Each attempt is logged via logInfo so operators can see progress in kubectl
// logs. logInfo may be nil — in that case, retries are silent.
func MigrateWithRetry(ctx context.Context, migrate func(context.Context) error, total, interval time.Duration, logInfo LogInfoFunc) error {
	deadline := time.Now().Add(total)
	attempt := 0
	var lastErr error
	for {
		attempt++
		err := migrate(ctx)
		if err == nil {
			if logInfo != nil && attempt > 1 {
				logInfo("BriefStore migrate: succeeded", "attempt", attempt)
			}
			return nil
		}
		lastErr = err
		if logInfo != nil {
			logInfo("BriefStore migrate: attempt failed, will retry",
				"attempt", attempt, "error", err, "nextIn", interval)
		}
		if time.Now().Add(interval).After(deadline) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
