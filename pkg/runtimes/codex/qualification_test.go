package codex

import "testing"

func TestOpenRouterEvidenceDoesNotClaimUnobservedWorkflow(t *testing.T) {
	d := (&runtime{}).Descriptor()
	if len(d.RouteModelEvidence) != 2 {
		t.Fatalf("route evidence entries = %d, want 2", len(d.RouteModelEvidence))
	}
	for _, e := range d.RouteModelEvidence {
		if e.Route != "openrouter-responses" || e.HarnessVersion != "0.162.1" || e.Upstream != "unknown" || e.EvidenceURL == "" {
			t.Fatalf("incomplete route evidence for %q: %+v", e.Model, e)
		}
		switch e.Model {
		case "openai/gpt-4.1-mini":
			if e.Outcomes["task-receipt"] != "pass" || e.Outcomes["task-cancel"] != "fail" || e.Outcomes["shell"] != "untested" || e.Outcomes["jobs"] != "untested" {
				t.Fatalf("GPT pilot overclaims workflow coverage: %+v", e.Outcomes)
			}
		case "cohere/north-mini-code:free":
			if e.Outcomes["task-receipt"] != "fail" || e.Outcomes["task-cancel"] != "fail" {
				t.Fatalf("Cohere pilot lost task failures: %+v", e.Outcomes)
			}
		default:
			t.Fatalf("unexpected evidence model %q", e.Model)
		}
	}
}
