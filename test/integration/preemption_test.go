//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/briefstore"
)

// TestPreemptionNotice_Integration validates the full preemption flow:
// preemption notice → handler invoked → brief written with preemption context → round-trip via API.
func TestPreemptionNotice_Integration(t *testing.T) {
	store := briefstore.NewMemoryStore()

	// Track preemption notices.
	var notifiedMachine string
	var notifiedInstance string
	srv := api.NewInternalServer(store, api.WithPreemptionHandler(func(machine, instance string) {
		notifiedMachine = machine
		notifiedInstance = instance
	}))

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// POST preemption notice.
	body := `{"timestamp":"2026-04-13T12:00:00Z","instanceId":"kyber-testvm"}`
	resp, err := http.Post(
		ts.URL+"/internal/machines/prod-worker/preemption-notice",
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST preemption notice: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if notifiedMachine != "prod-worker" {
		t.Errorf("machine: got %q, want %q", notifiedMachine, "prod-worker")
	}
	if notifiedInstance != "kyber-testvm" {
		t.Errorf("instance: got %q, want %q", notifiedInstance, "kyber-testvm")
	}

	// Write a preemption brief and verify round-trip.
	brief := &briefstore.Brief{
		Version:       1,
		AgentName:     "test-agent",
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		ShutdownType:  briefstore.ShutdownTypePreemption,
		RestartReason: briefstore.RestartReasonPreemption,
		Metadata: briefstore.BriefMetadata{
			PreviousModel: "sonnet",
			RestartCount:  0,
			PreemptionContext: &briefstore.PreemptionContext{
				InstanceId:    "kyber-testvm",
				Zone:          "us-central1-a",
				Timestamp:     "2026-04-13T12:00:00Z",
				GracefulDrain: true,
			},
		},
	}
	if err := store.Put(t.Context(), "test-agent", brief); err != nil {
		t.Fatalf("put brief: %v", err)
	}

	// Fetch via API.
	resp, err = http.Get(ts.URL + "/internal/agents/test-agent/session-brief")
	if err != nil {
		t.Fatalf("GET brief: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("brief status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Decode and verify preemption context.
	var got briefstore.Brief
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode brief: %v", err)
	}
	if got.ShutdownType != briefstore.ShutdownTypePreemption {
		t.Errorf("ShutdownType: got %q, want %q", got.ShutdownType, briefstore.ShutdownTypePreemption)
	}
	if got.RestartReason != briefstore.RestartReasonPreemption {
		t.Errorf("RestartReason: got %q, want %q", got.RestartReason, briefstore.RestartReasonPreemption)
	}
	if got.Metadata.PreemptionContext == nil {
		t.Fatal("PreemptionContext: got nil")
	}
	if got.Metadata.PreemptionContext.InstanceId != "kyber-testvm" {
		t.Errorf("InstanceId: got %q, want %q", got.Metadata.PreemptionContext.InstanceId, "kyber-testvm")
	}
	if got.Metadata.PreemptionContext.Zone != "us-central1-a" {
		t.Errorf("Zone: got %q, want %q", got.Metadata.PreemptionContext.Zone, "us-central1-a")
	}
	if !got.Metadata.PreemptionContext.GracefulDrain {
		t.Error("GracefulDrain: got false, want true")
	}
}
