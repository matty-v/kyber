package agent

import (
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"testing"
)

func TestSlackInboundBinding(t *testing.T) {
	b := SlackInboundBinding("barf-slack", DefaultSlackAction())
	if b.Name != "slack" || b.ExistingSecret != "barf-slack" || b.SignaturePrefix != "sha256=" {
		t.Fatalf("unexpected binding: %+v", b)
	}
	if len(b.Fields) < 5 {
		t.Fatalf("binding has too few fields: %+v", b.Fields)
	}
}

func TestLegacySlackDefaultAction(t *testing.T) {
	old := "Someone messaged you on Slack (details below). Reply conversationally using the kyber-slack MCP reply tool with channel_id and thread_ts. Keep replies concise."
	if !IsLegacySlackDefaultAction(old) {
		t.Fatal("old generated action was not recognized")
	}
	if IsLegacySlackDefaultAction("custom") {
		t.Fatal("custom action was treated as generated")
	}
}

func TestAppendSlackSidecar(t *testing.T) {
	spec := corev1.PodSpec{}
	AppendSlackSidecar(&spec, SlackSidecarConfig{AgentName: "barf", Image: "slack:local", ExistingSecret: "barf-slack"})
	if len(spec.Containers) != 1 || spec.Containers[0].Name != SlackSidecarContainerName {
		t.Fatalf("sidecar not injected: %+v", spec.Containers)
	}
	seen := map[string]bool{}
	for _, e := range spec.Containers[0].Env {
		seen[e.Name] = true
	}
	for _, name := range []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "SLACK_ALLOWED_USER_IDS", "SLACK_ALLOWED_CHANNEL_IDS", "KYBER_SLACK_MCP_ADDR"} {
		if !seen[name] {
			t.Errorf("missing %s", name)
		}
	}
}

func TestSlackEnabledFieldIsIndependent(t *testing.T) {
	a := kyberv1.Agent{}
	a.Spec.Secrets.SlackEnabled = true
	if !a.Spec.Secrets.SlackEnabled {
		t.Fatal("SlackEnabled did not persist")
	}
}
