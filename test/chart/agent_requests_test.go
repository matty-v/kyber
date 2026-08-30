package chart

import (
	"strings"
	"testing"
)

func TestAgentRequestsDefaultOnAndCanBeDisabled(t *testing.T) {
	rendered := helmTemplate(t)
	if !strings.Contains(rendered, "KYBER_AGENT_REQUESTS_ENABLED: \"true\"") {
		t.Fatal("agent requests must render enabled by default")
	}

	disabled := helmTemplate(t, "api.agentRequests.enabled=false")
	if !strings.Contains(disabled, "KYBER_AGENT_REQUESTS_ENABLED: \"false\"") {
		t.Fatal("api.agentRequests.enabled=false must disable agent requests")
	}
}
