package nodeagent

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultMetadataURL  = "http://metadata.google.internal/computeMetadata/v1/instance/preempted"
	DefaultPollInterval = 5 * time.Second
)

// PreemptionWatcher polls the GCE metadata endpoint for a preemption signal.
// When the instance is about to be preempted, it calls OnPreemption once and exits.
type PreemptionWatcher struct {
	MetadataURL  string
	PollInterval time.Duration
	OnPreemption func()
}

// Run polls the metadata endpoint at PollInterval until the context is cancelled
// or a preemption signal is detected.
func (w *PreemptionWatcher) Run(ctx context.Context) {
	url := w.MetadataURL
	if url == "" {
		url = DefaultMetadataURL
	}
	interval := w.PollInterval
	if interval == 0 {
		interval = DefaultPollInterval
	}

	client := &http.Client{Timeout: 3 * time.Second}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w.isPreempted(ctx, client, url) {
				if w.OnPreemption != nil {
					w.OnPreemption()
				}
				return // Only fire once
			}
		}
	}
}

func (w *PreemptionWatcher) isPreempted(ctx context.Context, client *http.Client, url string) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}

	return strings.TrimSpace(string(body)) == "TRUE"
}
