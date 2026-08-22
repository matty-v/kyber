package briefstore_test

import (
	"encoding/json"
	"testing"

	"github.com/matty-v/kyber/pkg/briefstore"
)

// TestPreemptionContext_RoundTripJSON verifies that a Brief containing a
// PreemptionContext in Metadata survives a JSON marshal/unmarshal round-trip
// with all fields intact.
func TestPreemptionContext_RoundTripJSON(t *testing.T) {
	original := &briefstore.Brief{
		Version:       1,
		AgentName:     "dave",
		Timestamp:     "2026-04-12T10:00:00Z",
		ShutdownType:  briefstore.ShutdownTypePreemption,
		RestartReason: briefstore.RestartReasonPreemption,
		LastActivity:  "handling preemption notice",
		Metadata: briefstore.BriefMetadata{
			PreviousModel: "claude-sonnet-4",
			UptimeSeconds: 1200,
			RestartCount:  2,
			PreemptionContext: &briefstore.PreemptionContext{
				InstanceId:    "projects/myproject/zones/us-central1-a/instances/kyber-node-1",
				Zone:          "us-central1-a",
				Timestamp:     "2026-04-12T10:00:00Z",
				GracefulDrain: true,
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var got briefstore.Brief
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if got.ShutdownType != briefstore.ShutdownTypePreemption {
		t.Errorf("ShutdownType: got %q, want %q", got.ShutdownType, briefstore.ShutdownTypePreemption)
	}
	if got.RestartReason != briefstore.RestartReasonPreemption {
		t.Errorf("RestartReason: got %q, want %q", got.RestartReason, briefstore.RestartReasonPreemption)
	}
	if got.Metadata.PreemptionContext == nil {
		t.Fatal("Metadata.PreemptionContext: got nil, want non-nil")
	}

	pc := got.Metadata.PreemptionContext
	wantPC := original.Metadata.PreemptionContext
	if pc.InstanceId != wantPC.InstanceId {
		t.Errorf("PreemptionContext.InstanceId: got %q, want %q", pc.InstanceId, wantPC.InstanceId)
	}
	if pc.Zone != wantPC.Zone {
		t.Errorf("PreemptionContext.Zone: got %q, want %q", pc.Zone, wantPC.Zone)
	}
	if pc.Timestamp != wantPC.Timestamp {
		t.Errorf("PreemptionContext.Timestamp: got %q, want %q", pc.Timestamp, wantPC.Timestamp)
	}
	if pc.GracefulDrain != wantPC.GracefulDrain {
		t.Errorf("PreemptionContext.GracefulDrain: got %v, want %v", pc.GracefulDrain, wantPC.GracefulDrain)
	}
}

// TestPreemptionContext_OmittedWhenNil verifies that a Brief without a
// PreemptionContext serializes without the "preemptionContext" key, and that
// deserializing such JSON leaves Metadata.PreemptionContext as nil.
func TestPreemptionContext_OmittedWhenNil(t *testing.T) {
	original := &briefstore.Brief{
		Version:       1,
		AgentName:     "chewie",
		Timestamp:     "2026-04-12T10:00:00Z",
		ShutdownType:  briefstore.ShutdownTypePlanned,
		RestartReason: briefstore.RestartReasonOperator,
		Metadata:      briefstore.BriefMetadata{},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// The key should be absent from the JSON output.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal raw: %v", err)
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(raw["metadata"], &meta); err != nil {
		t.Fatalf("json.Unmarshal metadata: %v", err)
	}
	if _, ok := meta["preemptionContext"]; ok {
		t.Error("expected preemptionContext key to be absent when nil, but it was present")
	}

	var got briefstore.Brief
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal Brief: %v", err)
	}
	if got.Metadata.PreemptionContext != nil {
		t.Errorf("Metadata.PreemptionContext: got %+v, want nil", got.Metadata.PreemptionContext)
	}
}

// TestShutdownTypeConstants verifies all expected ShutdownType constant values.
func TestShutdownTypeConstants(t *testing.T) {
	cases := []struct {
		name  string
		got   string
		want  string
	}{
		{"Planned", briefstore.ShutdownTypePlanned, "planned"},
		{"Unplanned", briefstore.ShutdownTypeUnplanned, "unplanned"},
		{"Preemption", briefstore.ShutdownTypePreemption, "preemption"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("ShutdownType%s = %q, want %q", tc.name, tc.got, tc.want)
			}
		})
	}
}

// TestRestartReasonConstants verifies all expected RestartReason constant values.
func TestRestartReasonConstants(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"FirstBoot", briefstore.RestartReasonFirstBoot, "first_boot"},
		{"Operator", briefstore.RestartReasonOperator, "operator"},
		{"Crash", briefstore.RestartReasonCrash, "crash"},
		{"Preemption", briefstore.RestartReasonPreemption, "preemption"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("RestartReason%s = %q, want %q", tc.name, tc.got, tc.want)
			}
		})
	}
}
