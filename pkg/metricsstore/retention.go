package metricsstore

import (
	"context"
	"log/slog"
	"time"
)

// Evictable is implemented by stores that support evicting old time-series points.
type Evictable interface {
	EvictBefore(ctx context.Context, olderThanTs int64) error
}

// StartRetentionWorker starts a background goroutine that calls store.EvictBefore
// every interval, removing points older than retentionSeconds from the store.
// The goroutine exits when ctx is cancelled.
func StartRetentionWorker(ctx context.Context, store Evictable, retentionSeconds int64, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cutoff := time.Now().Unix() - retentionSeconds
				if err := store.EvictBefore(ctx, cutoff); err != nil {
					slog.Warn("metricsstore retention: eviction failed", "err", err)
				}
			}
		}
	}()
}
