package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func TestLogAlertSink_Fire(t *testing.T) {
	sink := NewLogAlertSink()
	ctx := context.Background()

	tests := []struct {
		name  string
		alert Alert
	}{
		{
			name: "info alert",
			alert: Alert{
				Severity: "info",
				Kind:     "agent",
				Name:     "dave",
				Reason:   "Running",
			},
		},
		{
			name: "warning alert",
			alert: Alert{
				Severity: "warning",
				Kind:     "agent",
				Name:     "dave",
				Reason:   "Failed",
				Details:  map[string]string{"restartCount": "3"},
			},
		},
		{
			name: "critical alert",
			alert: Alert{
				Severity: "critical",
				Kind:     "machine",
				Name:     "worker-1",
				Reason:   "Preempted",
				Details:  map[string]string{"zone": "us-central1-a"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := sink.Fire(ctx, tc.alert); err != nil {
				t.Errorf("LogAlertSink.Fire returned error: %v", err)
			}
		})
	}
}

func TestWebhookAlertSink_Fire_Success(t *testing.T) {
	var received webhookPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("unmarshal request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewWebhookAlertSink(srv.URL)
	alert := Alert{
		Severity: "warning",
		Kind:     "agent",
		Name:     "dave",
		Reason:   "Failed",
		Details:  map[string]string{"phase": "Failed"},
	}

	if err := sink.Fire(context.Background(), alert); err != nil {
		t.Fatalf("WebhookAlertSink.Fire returned error: %v", err)
	}

	if received.Severity != "warning" {
		t.Errorf("expected severity=warning, got %q", received.Severity)
	}
	if received.Kind != "agent" {
		t.Errorf("expected kind=agent, got %q", received.Kind)
	}
	if received.Name != "dave" {
		t.Errorf("expected name=dave, got %q", received.Name)
	}
	if received.Reason != "Failed" {
		t.Errorf("expected reason=Failed, got %q", received.Reason)
	}
	if received.Details["phase"] != "Failed" {
		t.Errorf("expected details.phase=Failed, got %q", received.Details["phase"])
	}
	if received.Timestamp == "" {
		t.Error("expected non-empty timestamp")
	}
}

func TestWebhookAlertSink_Fire_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a 500 from the webhook endpoint.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sink := NewWebhookAlertSink(srv.URL)
	// Should NOT return an error even on 5xx — webhook failures are non-fatal.
	if err := sink.Fire(context.Background(), Alert{Severity: "warning", Kind: "agent", Name: "x", Reason: "test"}); err != nil {
		t.Errorf("WebhookAlertSink.Fire should not return error on 5xx, got: %v", err)
	}
}

func TestWebhookAlertSink_Fire_Unreachable(t *testing.T) {
	sink := NewWebhookAlertSink("http://127.0.0.1:19998") // nothing listening
	// Must not return an error — network failures are non-fatal.
	if err := sink.Fire(context.Background(), Alert{Severity: "critical", Kind: "machine", Name: "x", Reason: "Preempted"}); err != nil {
		t.Errorf("WebhookAlertSink.Fire should not return error on network failure, got: %v", err)
	}
}

func TestCompositeAlertSink_Fire(t *testing.T) {
	var logFired bool
	var webhookFired bool

	logSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookFired = true
		w.WriteHeader(http.StatusOK)
	}))
	defer logSrv.Close()

	type countingSink struct{ LogAlertSink }
	_ = countingSink{} // compile check

	webhookSink := NewWebhookAlertSink(logSrv.URL)
	logSink := &struct {
		LogAlertSink
		fired *bool
	}{fired: &logFired}
	// Use a custom wrapper that sets the flag.
	type flagSink struct {
		AlertSink
		flag *bool
	}
	wrappedLog := &flagSink{AlertSink: NewLogAlertSink(), flag: &logFired}

	composite := NewCompositeSink(wrappedLog, webhookSink)

	if err := composite.Fire(context.Background(), Alert{
		Severity: "warning",
		Kind:     "agent",
		Name:     "dave",
		Reason:   "Failed",
	}); err != nil {
		t.Fatalf("CompositeAlertSink.Fire returned error: %v", err)
	}

	// The webhook server was hit.
	if !webhookFired {
		t.Error("expected webhook to be fired")
	}
	_ = logSink // unused otherwise
}

// --------------------------------------------------------------------------
// kyber#586 — fail-loud sink seam + receiver contract + URL redaction.
// --------------------------------------------------------------------------

// AC: the sink-construction decision (configured vs unconfigured) is unit-testable
// in isolation, without a live cluster.
func TestBuildAlertSink(t *testing.T) {
	t.Run("unconfigured: empty URL yields the bare LogAlertSink floor", func(t *testing.T) {
		sink, configured := BuildAlertSink("")
		if configured {
			t.Fatal("expected configured=false for empty URL")
		}
		if _, ok := sink.(*LogAlertSink); !ok {
			t.Fatalf("expected *LogAlertSink floor, got %T", sink)
		}
	})

	t.Run("configured: a URL yields a composite (log floor + webhook)", func(t *testing.T) {
		sink, configured := BuildAlertSink("https://hooks.example.com/x")
		if !configured {
			t.Fatal("expected configured=true for a non-empty URL")
		}
		if _, ok := sink.(*CompositeAlertSink); !ok {
			t.Fatalf("expected *CompositeAlertSink, got %T", sink)
		}
	})
}

// AC: a tokened/secret webhook URL is never written to logs — redaction keeps host only.
func TestRedactWebhookURL(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"token in path + query", "https://hooks.example.com/services/T00/SECRETXYZ?token=abc123", "hooks.example.com"},
		{"userinfo + port", "https://user:pass@host.example.com:8443/path", "host.example.com:8443"},
		{"malformed", "://nope", "[redacted]"},
		{"empty", "", "[redacted]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactWebhookURL(c.in)
			if got != c.want {
				t.Fatalf("redactWebhookURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	// Belt-and-suspenders: the secret must not survive redaction.
	red := redactWebhookURL("https://hooks.example.com/services/SECRETXYZ?token=abc123")
	if strings.Contains(red, "SECRETXYZ") || strings.Contains(red, "abc123") {
		t.Fatalf("redaction leaked a secret: %q", red)
	}
}

// AC: the URL (which can embed a token) is never logged, INCLUDING on delivery
// failure. net/http wraps transport errors in *url.Error whose string embeds the
// full URL — so the error itself, not just the log field, must be sanitized.
func TestWebhookAlertSink_Fire_NeverLogsTokenedURL(t *testing.T) {
	var logged strings.Builder
	logger := funcr.New(func(prefix, args string) { logged.WriteString(args + "\n") }, funcr.Options{})
	ctx := log.IntoContext(context.Background(), logger)

	// Nothing listening on this port → forces the POST-failure path.
	const url = "https://127.0.0.1:19997/hook/SECRET-TOKEN-9f?key=topsecret"
	sink := NewWebhookAlertSink(url)
	_ = sink.Fire(ctx, Alert{Severity: "warning", Kind: "agent", Name: "agent-r2-d2", Reason: "SidecarOOMRestart"})

	out := logged.String()
	if out == "" {
		t.Fatal("expected a failure log line, got none")
	}
	for _, secret := range []string{"SECRET-TOKEN-9f", "topsecret", "/hook", "key=topsecret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("delivery-failure log leaked %q; full log:\n%s", secret, out)
		}
	}
	// The host:port is non-secret and useful for diagnosis — it should be present.
	if !strings.Contains(out, "127.0.0.1:19997") {
		t.Fatalf("expected the redacted host in the log for diagnosis; full log:\n%s", out)
	}
}

// Receiver contract: the documented Phase-C SidecarOOMRestart payload carries every
// AC-named field over the stable webhookPayload envelope. Locks the contract shape
// that docs/operator/telemetry.md publishes for a receiver to build against.
func TestWebhookAlertSink_PhaseCContractShape(t *testing.T) {
	var received webhookPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewWebhookAlertSink(srv.URL)
	alert := Alert{
		Severity: "warning",
		Kind:     "agent",
		Name:     "agent-r2-d2",
		Reason:   "SidecarOOMRestart",
		Details: map[string]string{
			"sidecar":      "transcript-tailer",
			"restartCount": "4",
			"condition":    "flapping",
			"threshold":    "3",
		},
	}
	if err := sink.Fire(context.Background(), alert); err != nil {
		t.Fatalf("Fire returned error: %v", err)
	}

	if received.Severity != "warning" || received.Reason != "SidecarOOMRestart" || received.Name != "agent-r2-d2" {
		t.Errorf("envelope fields wrong: %+v", received)
	}
	if received.Timestamp == "" {
		t.Error("expected RFC3339 timestamp")
	}
	for k, want := range map[string]string{
		"sidecar": "transcript-tailer", "restartCount": "4", "condition": "flapping", "threshold": "3",
	} {
		if received.Details[k] != want {
			t.Errorf("details[%q] = %q, want %q", k, received.Details[k], want)
		}
	}
}
