package api

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/matty-v/kyber/pkg/runtimedetect"
)

// validateModelValue checks a model id an operator is about to write
// (fleet default, set-model, create) against the model catalogs the
// platform can already see, and returns a non-empty human-readable
// rejection message when the id is provably unknown.
//
// Why write-time validation exists at all: the only other check is the
// boot-time probe inside the agent pod, which runs long after the write
// and (before kyber's modelprobe rework) failed open — an invalid
// fleet-default model made every new agent fail silently while the
// platform showed green (canary regression 2026-08-22).
//
// The check is deliberately fail-open in the directions that matter:
//
//   - empty value → valid ("harness default").
//   - value without the provider prefix ("sonnet", "opus" aliases the
//     CLI accepts) → valid; only prefixed ids are catalog-checkable.
//   - no catalog data available (detection disabled, poller hasn't run,
//     agent not yet reporting) → valid; we cannot prove anything.
//   - force=true on the request → skip entirely. Escape hatch for a
//     brand-new model the hourly poller hasn't seen yet.
//
// It rejects only when at least one catalog has entries for the runtime
// AND none of them contain the id. agentName, when non-empty, adds that
// agent's authenticated catalog to the accepted set (entitlements can
// differ per account).
func (s *Server) validateModelValue(ctx context.Context, runtime, model, agentName string) string {
	if model == "" {
		return ""
	}
	base := strings.TrimSuffix(model, "[1m]")

	var prefix string
	switch runtime {
	case "codex":
		prefix = "gpt-"
	default:
		prefix = "claude-"
	}
	if !strings.HasPrefix(base, prefix) {
		return ""
	}

	known := make(map[string]bool)
	if s.RuntimeDetectCache != nil {
		if snap, err := s.RuntimeDetectCache.Get(ctx); err == nil && snap != nil {
			models := snap.Models
			if runtime == "codex" {
				models = snap.CodexModels
			}
			for _, m := range models {
				known[m.ID] = true
			}
		}
		if agentName != "" {
			if catalogs, ok := s.RuntimeDetectCache.(runtimedetect.AgentCatalogCache); ok {
				if models, err := catalogs.GetAgentModels(ctx, agentName); err == nil {
					for _, m := range models {
						known[m.ID] = true
					}
				}
			}
		}
	}
	if len(known) == 0 {
		return ""
	}
	if known[base] {
		return ""
	}

	ids := make([]string, 0, len(known))
	for id := range known {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > 8 {
		ids = ids[:8]
	}
	return fmt.Sprintf(
		"model %q is not in any model catalog this cluster can see (known: %s). A typo here fails every agent turn while the agent still reports healthy. Pass \"force\": true to write it anyway (e.g. a model newer than the last detection poll).",
		model, strings.Join(ids, ", "))
}
