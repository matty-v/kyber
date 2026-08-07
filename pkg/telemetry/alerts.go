package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Alert carries context about a notable platform event.
type Alert struct {
	// Severity is one of "info", "warning", "critical".
	Severity string
	// Kind is the resource type that triggered the alert, e.g. "agent" or "machine".
	Kind string
	// Name is the resource name.
	Name string
	// Reason is a short machine-readable reason code, e.g. "Failed", "Preempted".
	Reason string
	// Details holds optional structured context (phase transitions, counts, etc.).
	Details map[string]string
}

// AlertSink consumes alerts. Multiple sinks can be composed via CompositeAlertSink.
type AlertSink interface {
	Fire(ctx context.Context, alert Alert) error
}

// --------------------------------------------------------------------------
// LogAlertSink — always active; emits structured log entries via controller-runtime.
// --------------------------------------------------------------------------

// LogAlertSink writes every alert to the controller-runtime structured log.
// It never returns an error — log failures are silently swallowed to avoid
// disrupting the reconcile loop.
type LogAlertSink struct{}

// NewLogAlertSink returns a LogAlertSink ready to use.
func NewLogAlertSink() *LogAlertSink { return &LogAlertSink{} }

// Fire logs the alert. For warning/critical severity it uses logger.Error (which
// includes a stack hint in development mode). Info alerts use logger.Info.
func (s *LogAlertSink) Fire(ctx context.Context, alert Alert) error {
	logger := log.FromContext(ctx)
	fields := []any{
		"severity", alert.Severity,
		"kind", alert.Kind,
		"name", alert.Name,
		"reason", alert.Reason,
	}
	for k, v := range alert.Details {
		fields = append(fields, k, v)
	}

	switch alert.Severity {
	case "warning", "critical":
		logger.Error(nil, "platform alert", fields...)
	default:
		logger.Info("platform alert", fields...)
	}
	return nil
}

// --------------------------------------------------------------------------
// WebhookAlertSink — optional; POSTs JSON to a configurable URL.
// --------------------------------------------------------------------------

// webhookPayload is the JSON body sent to the webhook URL.
type webhookPayload struct {
	Timestamp string            `json:"timestamp"`
	Severity  string            `json:"severity"`
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Reason    string            `json:"reason"`
	Details   map[string]string `json:"details,omitempty"`
}

// WebhookAlertSink POSTs alert payloads to a URL. Failure is logged and does
// not return an error — a broken webhook must not stall reconciliation.
type WebhookAlertSink struct {
	URL    string
	Client *http.Client
}

// NewWebhookAlertSink constructs a WebhookAlertSink with a 5-second timeout.
func NewWebhookAlertSink(url string) *WebhookAlertSink {
	return &WebhookAlertSink{
		URL:    url,
		Client: &http.Client{Timeout: 5 * time.Second},
	}
}

// Fire POSTs the alert as JSON to w.URL. Errors are logged but not returned.
func (w *WebhookAlertSink) Fire(ctx context.Context, alert Alert) error {
	logger := log.FromContext(ctx)

	payload := webhookPayload{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Severity:  alert.Severity,
		Kind:      alert.Kind,
		Name:      alert.Name,
		Reason:    alert.Reason,
		Details:   alert.Details,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		logger.Error(err, "alert webhook: failed to marshal payload (non-fatal)",
			"kind", alert.Kind, "name", alert.Name)
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		// The URL can embed an operator secret (#586) — log only the host, and
		// sanitize the error (net/http wraps with *url.Error, which embeds the URL).
		logger.Error(sanitizeWebhookErr(err), "alert webhook: failed to build request (non-fatal)",
			"webhookHost", redactWebhookURL(w.URL))
		return nil
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.Client.Do(req)
	if err != nil {
		logger.Error(sanitizeWebhookErr(err), "alert webhook: POST failed (non-fatal)",
			"webhookHost", redactWebhookURL(w.URL))
		return nil
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Error(fmt.Errorf("HTTP %d", resp.StatusCode),
			"alert webhook: non-2xx response (non-fatal)", "webhookHost", redactWebhookURL(w.URL))
	}
	return nil
}

// redactWebhookURL returns only the host[:port] of a webhook URL, dropping the
// path, query, and userinfo — any of which can carry the operator secret the URL
// embeds (#586). It never returns the secret; on a parse failure or hostless URL
// it returns a fixed "[redacted]" label.
func redactWebhookURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[redacted]"
	}
	return u.Host
}

// sanitizeWebhookErr strips the (possibly tokened) URL from a transport error so
// it is safe to log. net/http wraps transport failures in *url.Error, whose
// Error() string embeds the full request URL; its wrapped error carries only the
// underlying cause (host/dial/TLS), never the path or query.
func sanitizeWebhookErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// BuildAlertSink constructs the control-plane alert sink from the webhook URL.
// With a URL it returns a composite of the always-on LogAlertSink floor plus a
// WebhookAlertSink, and configured=true. With an empty URL it returns the bare
// LogAlertSink floor and configured=false — the caller is expected to emit a loud
// startup warning in that case (alerts are log-only, not delivered). This is a
// pure, cluster-free seam so the configured/unconfigured decision is unit-testable.
func BuildAlertSink(webhookURL string) (AlertSink, bool) {
	if webhookURL == "" {
		return NewLogAlertSink(), false
	}
	return NewCompositeSink(NewLogAlertSink(), NewWebhookAlertSink(webhookURL)), true
}

// --------------------------------------------------------------------------
// CompositeAlertSink — fan-out to multiple sinks.
// --------------------------------------------------------------------------

// CompositeAlertSink fans an alert out to multiple sinks in order.
// Each sink's error is logged; the composite always returns nil so callers
// don't need to handle sink-level failures.
type CompositeAlertSink struct {
	sinks []AlertSink
}

// NewCompositeSink constructs a CompositeAlertSink from the provided sinks.
func NewCompositeSink(sinks ...AlertSink) *CompositeAlertSink {
	return &CompositeAlertSink{sinks: sinks}
}

// Fire delivers the alert to every configured sink.
func (c *CompositeAlertSink) Fire(ctx context.Context, alert Alert) error {
	logger := log.FromContext(ctx)
	for _, s := range c.sinks {
		if err := s.Fire(ctx, alert); err != nil {
			logger.Error(err, "alert sink error (non-fatal)",
				"sink", fmt.Sprintf("%T", s))
		}
	}
	return nil
}
