package metrics

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// FleetSummary holds current-moment counts derived from agent CRD status fields.
type FleetSummary struct {
	Total   int `json:"total"`
	Working int `json:"working"`
	Idle    int `json:"idle"`
	Offline int `json:"offline"`
}

// AgentLastActive describes the most recent heartbeat and activity state for one agent.
type AgentLastActive struct {
	Name              string    `json:"name"`
	State             string    `json:"state"`
	LastHeartbeatAt   *time.Time `json:"lastHeartbeatAt,omitempty"`
	SecondsSinceHeartbeat *int  `json:"secondsSinceHeartbeat,omitempty"`
	Stale             bool      `json:"stale"`
}

// GetFleetSummary reads all agent CRDs in namespace and returns current-moment counts.
func GetFleetSummary(ctx context.Context, k8s client.Client, namespace string) (FleetSummary, error) {
	agents := &kyberv1.AgentList{}
	if err := k8s.List(ctx, agents, client.InNamespace(namespace)); err != nil {
		return FleetSummary{}, err
	}

	var summary FleetSummary
	summary.Total = len(agents.Items)
	for i := range agents.Items {
		a := &agents.Items[i]
		switch a.Status.Phase {
		case kyberv1.AgentPhaseRunning:
			if a.Status.Activity != nil && a.Status.Activity.State == "working" {
				summary.Working++
			} else {
				summary.Idle++
			}
		case kyberv1.AgentPhaseStopped,
			kyberv1.AgentPhaseFailed, kyberv1.AgentPhaseDeleted:
			summary.Offline++
		default:
			// Creating, Starting, Stopping, Restarting, etc. — count as idle
			summary.Idle++
		}
	}
	return summary, nil
}

// GetLastActive reads all agent CRDs and returns a last-active entry per agent.
func GetLastActive(ctx context.Context, k8s client.Client, namespace string, cfg Config) ([]AgentLastActive, error) {
	agents := &kyberv1.AgentList{}
	if err := k8s.List(ctx, agents, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	threshold := cfg.stalenessThreshold()
	now := time.Now().UTC()
	out := make([]AgentLastActive, 0, len(agents.Items))

	for i := range agents.Items {
		a := &agents.Items[i]
		entry := AgentLastActive{
			Name:  a.Name,
			State: string(a.Status.Phase),
		}
		if a.Status.Activity != nil && a.Status.Activity.State != "" {
			entry.State = a.Status.Activity.State
		}
		if a.Status.Activity != nil && a.Status.Activity.LastHeartbeatAt != nil {
			t := a.Status.Activity.LastHeartbeatAt.Time.UTC()
			entry.LastHeartbeatAt = &t
			secs := int(now.Sub(t).Seconds())
			entry.SecondsSinceHeartbeat = &secs
			entry.Stale = secs > threshold
		} else {
			entry.Stale = true
		}
		out = append(out, entry)
	}
	return out, nil
}
