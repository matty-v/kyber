package nodeagent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPreemptionWatcher_DetectsPreemption(t *testing.T) {
	// Fake metadata server returns TRUE
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Error("missing Metadata-Flavor header")
		}
		fmt.Fprint(w, "TRUE")
	}))
	defer server.Close()

	notified := make(chan struct{}, 1)
	w := &PreemptionWatcher{
		MetadataURL:  server.URL + "/computeMetadata/v1/instance/preempted",
		PollInterval: 50 * time.Millisecond,
		OnPreemption: func() { notified <- struct{}{} },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go w.Run(ctx)

	select {
	case <-notified:
		// Success
	case <-ctx.Done():
		t.Fatal("preemption not detected within timeout")
	}
}

func TestPreemptionWatcher_IgnoresNonPreemption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "FALSE")
	}))
	defer server.Close()

	notified := make(chan struct{}, 1)
	w := &PreemptionWatcher{
		MetadataURL:  server.URL + "/computeMetadata/v1/instance/preempted",
		PollInterval: 50 * time.Millisecond,
		OnPreemption: func() { notified <- struct{}{} },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	go w.Run(ctx)

	select {
	case <-notified:
		t.Fatal("should not have detected preemption")
	case <-ctx.Done():
		// Expected — no false positive
	}
}

func TestEC2InterruptionSourceUsesIMDSv2AndDetectsNotice(t *testing.T) {
	var sawToken bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest/api/token":
			if r.Method != "PUT" || r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
				t.Error("invalid token request")
			}
			sawToken = true
			fmt.Fprint(w, "token")
		case "/latest/meta-data/spot/instance-action":
			if r.Header.Get("X-aws-ec2-metadata-token") != "token" {
				t.Error("missing IMDSv2 token")
			}
			fmt.Fprint(w, `{"action":"terminate"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if !(EC2InterruptionSource{BaseURL: server.URL}).Interrupted(context.Background(), server.Client()) {
		t.Fatal("notice not detected")
	}
	if !sawToken {
		t.Fatal("token endpoint not called")
	}
}

func TestEC2InterruptionSourceNoNoticeAndUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest/api/token" {
			fmt.Fprint(w, "token")
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	if (EC2InterruptionSource{BaseURL: server.URL}).Interrupted(context.Background(), server.Client()) {
		t.Fatal("false notice")
	}
	if (EC2InterruptionSource{BaseURL: "http://127.0.0.1:1"}).Interrupted(context.Background(), &http.Client{Timeout: 20 * time.Millisecond}) {
		t.Fatal("unavailable IMDS reported notice")
	}
}

func TestEC2InterruptionSourceRejectsMissingToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusForbidden) }))
	defer server.Close()
	if (EC2InterruptionSource{BaseURL: server.URL}).Interrupted(context.Background(), server.Client()) {
		t.Fatal("token failure reported notice")
	}
}
