package agent

import (
	"context"
	"fmt"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ensureSlackBinding makes enabling Slack through the Agent CRD sufficient to
// receive events. It is idempotent and never overwrites an operator-authored
// binding; the comms API remains free to replace the generated one explicitly.
func (r *AgentReconciler) ensureSlackBinding(ctx context.Context, agent *kyberv1.Agent) error {
	if !agent.Spec.Secrets.SlackEnabled {
		return nil
	}
	for i, b := range agent.Spec.InboundBindings {
		if b.Name == SlackInboundBindingName {
			if IsLegacySlackDefaultAction(b.Action) {
				patch := client.MergeFrom(agent.DeepCopy())
				agent.Spec.InboundBindings[i] = SlackInboundBinding(b.ExistingSecret, DefaultSlackAction())
				if err := r.Patch(ctx, agent, patch); err != nil {
					return fmt.Errorf("upgrading Slack inbound binding: %w", err)
				}
			}
			return nil
		}
	}
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.InboundBindings = append(agent.Spec.InboundBindings,
		SlackInboundBinding(agent.Name+"-slack", DefaultSlackAction()))
	if err := r.Patch(ctx, agent, patch); err != nil {
		return fmt.Errorf("ensuring Slack inbound binding: %w", err)
	}
	return nil
}
