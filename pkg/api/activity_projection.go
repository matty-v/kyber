package api

import (
	"time"

	"github.com/matty-v/kyber/pkg/activity"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// The public projection never exposes observer epochs, native handles or
// runtime content, and never promotes legacy state into a current-work lease.
func admittedWorkView(agent *kyberv1.Agent, now time.Time) *agentWorkViewResponse {
	if agent.Status.Activity == nil || agent.Status.Activity.Work == nil {
		return nil
	}
	work := agent.Status.Activity.Work
	if work.AgentUID != string(agent.UID) || work.Sequence <= 0 || work.ObservedAt.IsZero() {
		return nil
	}
	view := activity.Project(activity.Input{
		Running:     agent.Status.Phase == kyberv1.AgentPhaseRunning,
		StopPending: agent.Spec.DesiredPhase != "" && agent.Spec.DesiredPhase != kyberv1.AgentPhaseRunning,
		ContactAt:   work.ReceivedAt.Time,
		Inventory:   &activity.Inventory{ObservedAt: work.ObservedAt.Time, LastWorkedAt: work.LastWorkedAt.Time, Foreground: int(work.Foreground), Background: int(work.Background), Complete: work.Complete, NeedsInput: work.NeedsInput},
	}, now)
	result := &agentWorkViewResponse{State: string(view.State), Reason: view.Reason, ServerTime: now.UTC().Format(time.RFC3339Nano), ObservedAt: work.ObservedAt.UTC().Format(time.RFC3339Nano), ForegroundCount: view.Foreground, BackgroundCount: view.Background, NeedsInput: view.NeedsInput}
	if !view.ExpiresAt.IsZero() {
		result.ExpiresAt = view.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if !view.LastWorkedAt.IsZero() {
		result.LastWorkedAt = view.LastWorkedAt.UTC().Format(time.RFC3339Nano)
	}
	if !work.StateSince.IsZero() {
		result.StateSince = work.StateSince.UTC().Format(time.RFC3339Nano)
	}
	return result
}
