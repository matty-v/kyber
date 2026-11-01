package api

import (
	"context"
	"time"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/reasoningeffort"
	"github.com/matty-v/kyber/pkg/runtimedetect"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func (s *Server) agentEffortOptions(ctx context.Context, a *kyberv1.Agent, model, version string, custom bool) (reasoningeffort.Options, string) {
	fallback := reasoningeffort.Lookup(a.Spec.Runtime, version, model, custom)
	if a.Spec.Runtime != "codex" || custom || a.UID == "" {
		return fallback, version
	}
	// A new control plane cannot promise native validation in an older image.
	if !reasoningeffort.SourceImageSupported("codex", s.RuntimeImages["codex"], s.BuildSHA) {
		if fallback.State != "supported" {
			fallback.Reason = "Update the Codex runtime image to enable effort for current models."
		}
		return fallback, version
	}
	unknown := reasoningeffort.Options{State: "unknown", Levels: []string{}, Reason: "Waiting for this agent's model information. Retry after its runtime starts."}
	cache, ok := s.RuntimeDetectCache.(runtimedetect.EffortCatalogCache)
	if !ok || a.UID == "" || a.Status.PodName == "" {
		return unknown, version
	}
	c, err := cache.GetEffortCatalog(ctx, a.Name)
	if err != nil || c.AgentUID != string(a.UID) || c.Runtime != "codex" || c.Version == "" || c.Version != a.Status.Runtime.InstalledVersion || a.Status.Runtime.Runtime != "codex" || c.ObservedAt.IsZero() || c.ObservedAt.After(time.Now()) || time.Since(c.ObservedAt) > runtimedetect.EffortCatalogMaxAge {
		return unknown, version
	}
	if version == "latest" {
		if a.Status.Runtime.RequestedVersion != "latest" || a.Status.Runtime.RequestedSatisfied == nil || !*a.Status.Runtime.RequestedSatisfied {
			return unknown, version
		}
	} else if version != c.Version {
		return unknown, version
	}
	pod := &corev1.Pod{}
	if s.K8sClient.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: a.Status.PodName}, pod) != nil || string(pod.UID) != c.PodUID || pod.DeletionTimestamp != nil {
		return unknown, version
	}
	owned := false
	for _, o := range pod.OwnerReferences {
		if o.Kind == "Agent" && o.UID == a.UID && o.Controller != nil && *o.Controller {
			owned = true
		}
	}
	if !owned {
		return unknown, version
	}
	imageMatches := false
	for _, container := range pod.Spec.Containers {
		if container.Name == "agent" && container.Image == s.RuntimeImages["codex"] {
			imageMatches = true
		}
	}
	if !imageMatches {
		return unknown, version
	}
	for _, m := range c.Models {
		if m.ID != model {
			continue
		}
		e := m.ReasoningEffort
		if e == nil || e.Validate("codex") != nil || e.HarnessVersion != c.Version {
			return unknown, c.Version
		}
		return reasoningeffort.Options{State: "supported", Levels: append([]string{}, e.Levels...), Source: "codex-model-list/" + c.Version}, c.Version
	}
	return unknown, c.Version
}
