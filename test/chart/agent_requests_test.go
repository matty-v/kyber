package chart

import (
	"strings"
	"testing"
)

func TestAgentRequestsUseOnlyPerAgentGate(t *testing.T) {
	rendered := helmTemplate(t)
	if strings.Contains(rendered, "KYBER_AGENT_REQUESTS_ENABLED") {
		t.Fatal("stale cluster-wide request gate must not override the per-agent opt-in")
	}
}
