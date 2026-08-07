package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// makeRotatedSecret returns a corev1.Secret carrying both current + previous
// HMAC bytes, plus a webhook-secret-prev-rotated-at timestamp.
func makeRotatedSecret(name string, current, prev []byte, rotatedAt time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kyber-system"},
		Data: map[string][]byte{
			"webhook-secret":                current,
			"webhook-secret-prev":           prev,
			"webhook-secret-prev-rotated-at": []byte(rotatedAt.UTC().Format(time.RFC3339)),
		},
	}
}

// TestInbound_RotationGrace_AcceptsPrev: a request signed with the previous
// secret is accepted while the rotation grace window is active.
func TestInbound_RotationGrace_AcceptsPrev(t *testing.T) {
	const current = "newsecret"
	const prev = "oldsecret"
	body := validPushBody()
	// Signed with the OLD secret.
	sig := signBody([]byte(prev), body, "sha256=")

	// rotated-at = 1 hour ago: well within the 24h grace.
	rotatedAt := time.Now().Add(-1 * time.Hour)

	h := buildInboundHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		makeRotatedSecret("dave-github-hmac", []byte(current), []byte(prev), rotatedAt),
	)

	rr := postInbound(t, h, "dave", "github", body, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "push",
		"Content-Type":        "application/json",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 (accepted via prev secret), got %d: %s",
			rr.Code, rr.Body.String())
	}

	// Wait for the queue worker to deliver the job — confirms acceptance.
	select {
	case <-h.jobDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("queue handler never fired")
	}
	if got := len(h.recordedJobs()); got != 1 {
		t.Errorf("want 1 dispatched job, got %d", got)
	}
}

// TestInbound_RotationGrace_AcceptsCurrent: confirms current still works
// when prev is also present.
func TestInbound_RotationGrace_AcceptsCurrent(t *testing.T) {
	const current = "newsecret"
	const prev = "oldsecret"
	body := validPushBody()
	// Signed with the CURRENT secret.
	sig := signBody([]byte(current), body, "sha256=")

	rotatedAt := time.Now().Add(-1 * time.Hour)

	h := buildInboundHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		makeRotatedSecret("dave-github-hmac", []byte(current), []byte(prev), rotatedAt),
	)

	rr := postInbound(t, h, "dave", "github", body, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "push",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	select {
	case <-h.jobDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("queue handler never fired")
	}
}

// TestInbound_RotationGrace_RejectsPrevAfterExpiry: a prev secret older
// than the grace window is no longer accepted.
func TestInbound_RotationGrace_RejectsPrevAfterExpiry(t *testing.T) {
	const current = "newsecret"
	const prev = "oldsecret"
	body := validPushBody()
	sig := signBody([]byte(prev), body, "sha256=") // signed with old.

	// rotated-at = 25 hours ago: outside the 24h grace window.
	rotatedAt := time.Now().Add(-25 * time.Hour)

	h := buildInboundHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		makeRotatedSecret("dave-github-hmac", []byte(current), []byte(prev), rotatedAt),
	)

	rr := postInbound(t, h, "dave", "github", body, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "push",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 (prev expired), got %d: %s", rr.Code, rr.Body.String())
	}
	assertUnauthorizedBody(t, rr.Body.String())
}

// TestInbound_RotationGrace_NoTimestampRejectsPrev: prev key present but no
// rotated-at timestamp → prev is ignored, request is rejected.
func TestInbound_RotationGrace_NoTimestampRejectsPrev(t *testing.T) {
	const current = "newsecret"
	const prev = "oldsecret"
	body := validPushBody()
	sig := signBody([]byte(prev), body, "sha256=")

	// Note: no rotated-at key in Data.
	h := buildInboundHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "dave-github-hmac", Namespace: "kyber-system",
			},
			Data: map[string][]byte{
				"webhook-secret":      []byte(current),
				"webhook-secret-prev": []byte(prev),
			},
		},
	)

	rr := postInbound(t, h, "dave", "github", body, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "push",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 (no rotated-at → prev ignored), got %d: %s",
			rr.Code, rr.Body.String())
	}
	// Sanity-check the canonical 401 body shape (no info leak).
	if !strings.Contains(rr.Body.String(), `"unauthorized"`) {
		t.Errorf("expected unauthorized body, got %s", rr.Body.String())
	}
}
