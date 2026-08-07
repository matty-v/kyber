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
