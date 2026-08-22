package api

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// inboundGatePollInterval is how often WaitAgentRunning re-reads the
// Agent CR while holding an inbound job.
const inboundGatePollInterval = 3 * time.Second

// WaitAgentRunning blocks until the named agent's status.phase is
// Running, the timeout elapses, or ctx is cancelled. Used by the inbound
// dispatch worker to hold a job while the agent is mid-restart instead
// of delivering into the outgoing pod.
//
// Why: a message that arrives during a pod roll (set-model, secret
// update, operator restart — a ~30-60s window) used to be exec'd into
// the TERMINATING pod, whose dying session answered it; the reply was
// then lost with the pod (canary regression 2026-08-22, probe answered
// by the old model mid-roll). The Agent CR's phase is the right gate:
// it flips away from Running at the start of the roll and back only
// when the replacement pod is Ready.
//
// The wait holds only this agent's queue slot — inbound workers are
// per-agent goroutines, so other agents' deliveries are unaffected.
// On timeout the caller should still attempt delivery (preserves the
// pre-gate behavior for phases that never return to Running; the exec
// itself fails cleanly when there is no pod).
func (s *Server) WaitAgentRunning(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	for {
		agent := &kyberv1.Agent{}
		if err := s.K8sClient.Get(ctx, key, agent); err != nil {
			return fmt.Errorf("inbound gate: agent lookup: %w", err)
		}
		if agent.Status.Phase == kyberv1.AgentPhaseRunning {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("inbound gate: agent %q still %q after %s", name, agent.Status.Phase, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(inboundGatePollInterval):
		}
	}
}
