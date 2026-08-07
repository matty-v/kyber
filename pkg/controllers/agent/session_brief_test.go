package agent

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
)

// makeAgentForBriefTest returns a minimal Agent CRD for brief builder tests.
func makeAgentForBriefTest(name string, model string, restartCount int32, startTime *metav1.Time) *kyberv1.Agent {
	a := &kyberv1.Agent{}
	a.Name = name
	a.Spec.Model = model
	a.Status.RestartCount = restartCount
	a.Status.StartTime = startTime
	return a
}

// TestBuildBrief_FirstBoot verifies the brief built for a brand-new agent (no prior session).
func TestBuildBrief_FirstBoot(t *testing.T) {
	agent := makeAgentForBriefTest("dave", "claude-sonnet-4", 0, nil)
	fixedTime := time.Date(2026, 4, 10, 23, 0, 0, 0, time.UTC)
	input := BriefInput{
		Now:           fixedTime,
		ShutdownType:  "planned",
		RestartReason: "first_boot",
		PreviousModel: "claude-sonnet-4",
	}

	brief := BuildBrief(agent, input)

	if brief.Version != 1 {
		t.Errorf("Version: got %d, want 1", brief.Version)
	}
	if brief.AgentName != "dave" {
		t.Errorf("AgentName: got %q, want %q", brief.AgentName, "dave")
	}
	if brief.ShutdownType != "planned" {
		t.Errorf("ShutdownType: got %q, want %q", brief.ShutdownType, "planned")
	}
	if brief.RestartReason != "first_boot" {
		t.Errorf("RestartReason: got %q, want %q", brief.RestartReason, "first_boot")
	}
	if brief.Metadata.RestartCount != 0 {
		t.Errorf("Metadata.RestartCount: got %d, want 0", brief.Metadata.RestartCount)
	}
	if brief.Metadata.PreviousModel != "claude-sonnet-4" {
		t.Errorf("Metadata.PreviousModel: got %q, want %q", brief.Metadata.PreviousModel, "claude-sonnet-4")
	}
	if len(brief.RecentExchanges) != 0 {
		t.Errorf("RecentExchanges: got %d entries, want 0 on first boot", len(brief.RecentExchanges))
	}
	if len(brief.PendingMessages) != 0 {
		t.Errorf("PendingMessages: got %d entries, want 0 on first boot", len(brief.PendingMessages))
	}
	wantTimestamp := "2026-04-10T23:00:00Z"
	if brief.Timestamp != wantTimestamp {
		t.Errorf("Timestamp: got %q, want %q", brief.Timestamp, wantTimestamp)
	}
}

// TestBuildBrief_PlannedRestart verifies the brief for an operator-triggered restart from Stopped.
func TestBuildBrief_PlannedRestart(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	agent := makeAgentForBriefTest("dave", "claude-sonnet-4", 0, &startTime)
	fixedTime := time.Date(2026, 4, 10, 23, 0, 0, 0, time.UTC)

	input := BriefInput{
		Now:           fixedTime,
		ShutdownType:  "planned",
		RestartReason: "operator",
		LastActivity:  "reviewing the overlay FS spike",
		RecentExchanges: []briefstore.Exchange{
			{Role: "user", Content: "Let's continue.", Timestamp: "2026-04-10T22:00:00Z"},
			{Role: "assistant", Content: "Sure, picking up from the overlay...", Timestamp: "2026-04-10T22:00:10Z"},
		},
		PreviousModel: "claude-sonnet-4",
		UptimeSeconds: 3600,
	}

	brief := BuildBrief(agent, input)

	if brief.ShutdownType != "planned" {
		t.Errorf("ShutdownType: got %q, want %q", brief.ShutdownType, "planned")
	}
	if brief.RestartReason != "operator" {
		t.Errorf("RestartReason: got %q, want %q", brief.RestartReason, "operator")
	}
	if brief.LastActivity != "reviewing the overlay FS spike" {
		t.Errorf("LastActivity: got %q", brief.LastActivity)
	}
	if len(brief.RecentExchanges) != 2 {
		t.Errorf("RecentExchanges: got %d, want 2", len(brief.RecentExchanges))
	}
	if brief.RecentExchanges[0].Role != "user" {
		t.Errorf("RecentExchanges[0].Role: got %q, want %q", brief.RecentExchanges[0].Role, "user")
	}
	if brief.Metadata.UptimeSeconds != 3600 {
		t.Errorf("Metadata.UptimeSeconds: got %d, want 3600", brief.Metadata.UptimeSeconds)
	}
}

// TestBuildBrief_UnplannedCrash verifies the brief for an auto-restart after a crash.
func TestBuildBrief_UnplannedCrash(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-30 * time.Minute))
	agent := makeAgentForBriefTest("dave", "claude-opus-4", 2, &startTime)
	fixedTime := time.Date(2026, 4, 10, 23, 0, 0, 0, time.UTC)

	input := BriefInput{
		Now:           fixedTime,
		ShutdownType:  "unplanned",
		RestartReason: "crash",
		LastActivity:  "running tests on kyber",
		PreviousModel: "claude-opus-4",
		UptimeSeconds: 1800,
	}

	brief := BuildBrief(agent, input)

	if brief.ShutdownType != "unplanned" {
		t.Errorf("ShutdownType: got %q, want %q", brief.ShutdownType, "unplanned")
	}
	if brief.RestartReason != "crash" {
		t.Errorf("RestartReason: got %q, want %q", brief.RestartReason, "crash")
	}
	if brief.Metadata.RestartCount != 2 {
		t.Errorf("Metadata.RestartCount: got %d, want 2", brief.Metadata.RestartCount)
	}
	if brief.Metadata.PreviousModel != "claude-opus-4" {
		t.Errorf("Metadata.PreviousModel: got %q, want %q", brief.Metadata.PreviousModel, "claude-opus-4")
	}
	if len(brief.RecentExchanges) != 0 {
		t.Errorf("RecentExchanges: got %d, want 0 (crash — no state capture)", len(brief.RecentExchanges))
	}
}

// TestBuildBrief_WakeFromSuspended verifies the brief for a wake event with pending messages.
func TestBuildBrief_WakeFromSuspended(t *testing.T) {
	agent := makeAgentForBriefTest("dave", "claude-sonnet-4", 0, nil)
	fixedTime := time.Date(2026, 4, 10, 23, 0, 0, 0, time.UTC)

	input := BriefInput{
		Now:           fixedTime,
		ShutdownType:  "wake",
		RestartReason: "wake",
		PreviousModel: "claude-sonnet-4",
		PendingMessages: []briefstore.PendingMessage{
			{
				Source:    "telegram",
				From:      "1000000001",
				Text:      "Hey dave, quick question",
				Timestamp: "2026-04-10T23:25:00Z",
			},
		},
	}

	brief := BuildBrief(agent, input)

	if brief.ShutdownType != "wake" {
		t.Errorf("ShutdownType: got %q, want %q", brief.ShutdownType, "wake")
	}
	if brief.RestartReason != "wake" {
		t.Errorf("RestartReason: got %q, want %q", brief.RestartReason, "wake")
	}
	if len(brief.PendingMessages) != 1 {
		t.Fatalf("PendingMessages: got %d, want 1", len(brief.PendingMessages))
	}
	if brief.PendingMessages[0].Source != "telegram" {
		t.Errorf("PendingMessages[0].Source: got %q, want %q", brief.PendingMessages[0].Source, "telegram")
	}
	if brief.PendingMessages[0].Text != "Hey dave, quick question" {
		t.Errorf("PendingMessages[0].Text: got %q", brief.PendingMessages[0].Text)
	}
}

// TestBuildBrief_MetadataFields verifies that UptimeSeconds and PreviousModel are passed through.
func TestBuildBrief_MetadataFields(t *testing.T) {
	agent := makeAgentForBriefTest("han", "claude-opus-4", 1, nil)
	fixedTime := time.Date(2026, 4, 10, 23, 0, 0, 0, time.UTC)

	input := BriefInput{
		Now:           fixedTime,
		ShutdownType:  "planned",
		RestartReason: "operator",
		PreviousModel: "claude-opus-4",
		UptimeSeconds: 7200,
	}

	brief := BuildBrief(agent, input)

	if brief.Metadata.PreviousModel != "claude-opus-4" {
		t.Errorf("PreviousModel: got %q, want %q", brief.Metadata.PreviousModel, "claude-opus-4")
	}
	if brief.Metadata.UptimeSeconds != 7200 {
		t.Errorf("UptimeSeconds: got %d, want 7200", brief.Metadata.UptimeSeconds)
	}
	if brief.Metadata.RestartCount != 1 {
		t.Errorf("RestartCount: got %d, want 1", brief.Metadata.RestartCount)
	}
}

// TestBriefInputForEvent_UnknownEvent verifies that briefInputForEvent returns safe defaults
// for events it does not handle (e.g., EventCRDCreated, which goes through ActionCreatePVAndPod
// and never calls writeBrief — but if somehow passed, the default case should not panic).
func TestBriefInputForEvent_UnknownEvent(t *testing.T) {
	agent := makeAgentForBriefTest("dave", "claude-sonnet-4", 0, nil)
	// EventCRDCreated is intentionally not handled — falls through to default.
	input := briefInputForEvent(agent, EventCRDCreated)

	if input.ShutdownType != "unplanned" {
		t.Errorf("ShutdownType: got %q, want %q (default for unhandled event)", input.ShutdownType, "unplanned")
	}
	if input.RestartReason != "unknown" {
		t.Errorf("RestartReason: got %q, want %q (default for unhandled event)", input.RestartReason, "unknown")
	}
	if input.PreviousModel != "claude-sonnet-4" {
		t.Errorf("PreviousModel: got %q, want %q", input.PreviousModel, "claude-sonnet-4")
	}
	if input.UptimeSeconds != 0 {
		t.Errorf("UptimeSeconds: got %d, want 0 (no start time)", input.UptimeSeconds)
	}
	if input.Now.IsZero() {
		t.Error("Now must not be zero — briefInputForEvent must set it")
	}
}

// TestBriefInputForEvent_OperatorRestart verifies briefInputForEvent for EventDesiredRunning.
func TestBriefInputForEvent_OperatorRestart(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	agent := makeAgentForBriefTest("dave", "claude-sonnet-4", 0, &startTime)
	input := briefInputForEvent(agent, EventDesiredRunning)

	if input.ShutdownType != "planned" {
		t.Errorf("ShutdownType: got %q, want %q", input.ShutdownType, "planned")
	}
	if input.RestartReason != "operator" {
		t.Errorf("RestartReason: got %q, want %q", input.RestartReason, "operator")
	}
	// Uptime should be approximately 2 hours (7200s). Allow a 5s tolerance for test timing.
	if input.UptimeSeconds < 7195 || input.UptimeSeconds > 7210 {
		t.Errorf("UptimeSeconds: got %d, want ~7200", input.UptimeSeconds)
	}
}

// TestBriefInputForEvent_Crash verifies briefInputForEvent for EventAutoRestartTriggered.
func TestBriefInputForEvent_Crash(t *testing.T) {
	agent := makeAgentForBriefTest("dave", "claude-opus-4", 1, nil)
	input := briefInputForEvent(agent, EventAutoRestartTriggered)

	if input.ShutdownType != "unplanned" {
		t.Errorf("ShutdownType: got %q, want %q", input.ShutdownType, "unplanned")
	}
	if input.RestartReason != "crash" {
		t.Errorf("RestartReason: got %q, want %q", input.RestartReason, "crash")
	}
}

// TestBriefInputForEvent_Wake verifies briefInputForEvent for EventWakeReceived.
func TestBriefInputForEvent_Wake(t *testing.T) {
	agent := makeAgentForBriefTest("dave", "claude-sonnet-4", 0, nil)
	input := briefInputForEvent(agent, EventWakeReceived)

	if input.ShutdownType != "wake" {
		t.Errorf("ShutdownType: got %q, want %q", input.ShutdownType, "wake")
	}
	if input.RestartReason != "wake" {
		t.Errorf("RestartReason: got %q, want %q", input.RestartReason, "wake")
	}
}

// TestBriefInputForEvent_PodDeleted verifies briefInputForEvent for EventPodDeleted (Restarting phase).
func TestBriefInputForEvent_PodDeleted(t *testing.T) {
	agent := makeAgentForBriefTest("dave", "claude-sonnet-4", 0, nil)
	input := briefInputForEvent(agent, EventPodDeleted)

	if input.ShutdownType != "planned" {
		t.Errorf("ShutdownType: got %q, want %q", input.ShutdownType, "planned")
	}
	if input.RestartReason != "operator" {
		t.Errorf("RestartReason: got %q, want %q", input.RestartReason, "operator")
	}
}
