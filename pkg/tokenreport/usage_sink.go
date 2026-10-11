package tokenreport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPUsageSink sends content-free source evidence through the localhost
// status sidecar. A 2xx reply means the control plane committed the event and
// cursor; any other result leaves the collector's local cursor unchanged.
type HTTPUsageSink struct {
	Client           *http.Client
	SidecarURL       string
	AgentUID         string
	RouteFingerprint string
	Runtime          string
}

func (s *HTTPUsageSink) AcceptUsage(ctx context.Context, evidence UsageEvidence, cursor UsageAckCursor) error {
	if evidence.Runtime != s.Runtime {
		return ErrIncompleteUsage
	}
	return s.post(ctx, "usage-event", UsageEventRequest{AgentUID: s.AgentUID, RouteFingerprint: s.RouteFingerprint, Evidence: evidence, Cursor: cursor})
}

func (s *HTTPUsageSink) AcceptGap(ctx context.Context, sourceIdentity, reason string, detectedAt time.Time, cursor UsageAckCursor) error {
	return s.post(ctx, "usage-gap", UsageGapRequest{AgentUID: s.AgentUID, RouteFingerprint: s.RouteFingerprint,
		Runtime: s.Runtime, SourceIdentity: sourceIdentity, Reason: reason, DetectedAt: detectedAt, Cursor: cursor})
}

func (s *HTTPUsageSink) post(ctx context.Context, path string, body any) error {
	if s.AgentUID == "" || s.RouteFingerprint == "" || s.Runtime == "" {
		return ErrIncompleteUsage
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding usage evidence: %w", err)
	}
	base := s.SidecarURL
	if base == "" {
		base = "http://127.0.0.1:8091"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/"+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("building usage request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("posting usage evidence: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("usage evidence rejected: HTTP %d", response.StatusCode)
	}
	return nil
}
