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

type InterruptionSource interface {
	Interrupted(context.Context, *http.Client) bool
}

type GCEInterruptionSource struct{ MetadataURL string }

func (s GCEInterruptionSource) Interrupted(ctx context.Context, client *http.Client) bool {
	url := s.MetadataURL
	if url == "" {
		url = DefaultMetadataURL
	}
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
	return err == nil && strings.TrimSpace(string(body)) == "TRUE"
}

type EC2InterruptionSource struct{ BaseURL string }

func (s EC2InterruptionSource) Interrupted(ctx context.Context, client *http.Client) bool {
	base := strings.TrimRight(s.BaseURL, "/")
	if base == "" {
		base = "http://169.254.169.254"
	}
	tokenReq, err := http.NewRequestWithContext(ctx, "PUT", base+"/latest/api/token", nil)
	if err != nil {
		return false
	}
	tokenReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	tokenResp, err := client.Do(tokenReq)
	if err != nil {
		return false
	}
	token, readErr := io.ReadAll(tokenResp.Body)
	tokenResp.Body.Close()
	if readErr != nil || tokenResp.StatusCode != http.StatusOK || len(strings.TrimSpace(string(token))) == 0 {
		return false
	}
	for _, path := range []string{"/latest/meta-data/spot/instance-action", "/latest/meta-data/events/recommendations/rebalance"} {
		req, _ := http.NewRequestWithContext(ctx, "GET", base+path, nil)
		req.Header.Set("X-aws-ec2-metadata-token", strings.TrimSpace(string(token)))
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
	}
	return false
}

// PreemptionWatcher polls the GCE metadata endpoint for a preemption signal.
// When the instance is about to be preempted, it calls OnPreemption once and exits.
type PreemptionWatcher struct {
	MetadataURL  string
	PollInterval time.Duration
	OnPreemption func()
	Source       InterruptionSource
}

// Run polls the metadata endpoint at PollInterval until the context is cancelled
// or a preemption signal is detected.
func (w *PreemptionWatcher) Run(ctx context.Context) {
	source := w.Source
	if source == nil {
		source = GCEInterruptionSource{MetadataURL: w.MetadataURL}
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
			if source.Interrupted(ctx, client) {
				if w.OnPreemption != nil {
					w.OnPreemption()
				}
				return // Only fire once
			}
		}
	}
}

func (w *PreemptionWatcher) isPreempted(ctx context.Context, client *http.Client, url string) bool {
	return GCEInterruptionSource{MetadataURL: url}.Interrupted(ctx, client)
}
