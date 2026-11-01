package api

import (
	"bytes"
	"context"
	"encoding/json"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/runtimedetect"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func nativeEffortFixture(t *testing.T) (*Server, *kyberv1.Agent, *runtimedetect.MemoryCache, runtimedetect.EffortCatalog) {
	t.Helper()
	ctx := context.Background()
	cache := runtimedetect.NewMemoryCache()
	s := newSetModelServer(t, cache)
	a := effortTestAgent(t, s, kyberv1.AgentPhaseRunning, "")
	a.UID = "agent-current"
	a.Spec.Runtime = "codex"
	a.Spec.RuntimeVersion = "latest"
	a.Spec.Model = "gpt-6.1-terra"
	if err := s.K8sClient.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	yes := true
	a.Status.PodName = "agent-wedge"
	a.Status.Runtime = kyberv1.AgentRuntimeStatus{Runtime: "codex", InstalledVersion: "0.166.0", RequestedVersion: "latest", RequestedSatisfied: &yes}
	if err := s.K8sClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: a.Status.PodName, Namespace: fdNS, UID: "pod-current", OwnerReferences: []metav1.OwnerReference{{Kind: "Agent", Name: a.Name, UID: a.UID, Controller: &yes}}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: s.RuntimeImages["codex"]}}}}
	if err := s.K8sClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	c := runtimedetect.EffortCatalog{AgentUID: string(a.UID), PodUID: string(pod.UID), Runtime: "codex", Version: "0.166.0", ObservedAt: time.Now(), Models: []runtimedetect.Model{{ID: a.Spec.Model, ReasoningEffort: &runtimedetect.ReasoningEffortEvidence{Source: "codex-model-list", HarnessVersion: "0.166.0", Levels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}}}}}
	if err := cache.PutEffortCatalog(ctx, a.Name, c); err != nil {
		t.Fatal(err)
	}
	return s, a, cache, c
}

func TestNativeLatestEffortOptionsAndMutation(t *testing.T) {
	s, _, _, _ := nativeEffortFixture(t)
	rr := effortTestRequest(s, http.MethodGet, "/api/v1/agents/wedge/effort-options", "")
	var options effortOptionsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &options); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 200 || options.State != "supported" || options.RuntimeVersion != "0.166.0" || len(options.Levels) != 6 {
		t.Fatalf("options=%s", rr.Body.String())
	}
	rr = effortTestRequest(s, http.MethodPost, "/api/v1/agents/wedge/set-effort", `{"effort":"ultra"}`)
	if rr.Code != 200 {
		t.Fatalf("mutation=%d %s", rr.Code, rr.Body.String())
	}
	var got kyberv1.Agent
	_ = s.K8sClient.Get(context.Background(), types.NamespacedName{Namespace: fdNS, Name: "wedge"}, &got)
	if got.Spec.Effort != "ultra" || got.Spec.RuntimeVersion != "latest" || got.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
		t.Fatalf("unexpected spec: %+v", got.Spec)
	}
}

func TestNativeEffortRejectsStaleUnboundOrChangedEvidence(t *testing.T) {
	for _, mode := range []string{"agent", "pod", "version", "expired", "future", "model", "empty", "missing", "custom", "failed-install", "target-version", "old-image"} {
		t.Run(mode, func(t *testing.T) {
			s, a, cache, c := nativeEffortFixture(t)
			switch mode {
			case "agent":
				c.AgentUID = "old-agent"
			case "pod":
				c.PodUID = "old-pod"
			case "version":
				c.Version = "0.165.0"
			case "expired":
				c.ObservedAt = time.Now().Add(-3 * time.Hour)
			case "future":
				c.ObservedAt = time.Now().Add(time.Hour)
			case "model":
				c.Models[0].ID = "another-model"
			case "empty":
				c.Models[0].ReasoningEffort.Levels = []string{}
			case "missing":
				c.Models[0].ReasoningEffort = nil
			case "custom":
				a.Spec.Inference = &kyberv1.AgentInference{}
			case "failed-install":
				no := false
				a.Status.Runtime.RequestedSatisfied = &no
			case "target-version":
				a.Spec.RuntimeVersion = "0.167.0"
			case "old-image":
				s.RuntimeImages["codex"] = "ghcr.io/matty-v/kyber-codex:latest"
			}
			if err := cache.PutEffortCatalog(context.Background(), a.Name, c); err != nil {
				t.Fatal(err)
			}
			options, _ := s.agentEffortOptions(context.Background(), a, a.Spec.Model, a.Spec.RuntimeVersion, a.Spec.Inference != nil)
			if len(options.Levels) != 0 {
				t.Fatalf("stale evidence offered %v", options.Levels)
			}
		})
	}
}

func TestNativeCatalogRequiresCurrentOwnedPodAndVersion(t *testing.T) {
	for _, mode := range []string{"valid", "old-pod", "old-version"} {
		t.Run(mode, func(t *testing.T) {
			s, a, cache, c := nativeEffortFixture(t)
			server := NewInternalServer(briefstore.NewMemoryStore(), WithKubeClient(s.K8sClient, fdNS))
			server.SetRuntimeDetectCache(cache)
			if mode == "old-pod" {
				c.PodUID = "old-pod"
			}
			if mode == "old-version" {
				c.Models[0].ReasoningEffort.HarnessVersion = "0.165.0"
			}
			body, _ := json.Marshal(map[string]any{"runtime": "codex", "podUID": c.PodUID, "models": c.Models})
			req := httptest.NewRequest(http.MethodPost, "/internal/agents/"+a.Name+"/runtime-catalog", bytes.NewReader(body))
			rr := httptest.NewRecorder()
			server.Handler().ServeHTTP(rr, req)
			want := http.StatusNoContent
			if mode != "valid" {
				want = http.StatusConflict
			}
			if rr.Code != want {
				t.Fatalf("status=%d expected=%d body=%s", rr.Code, want, rr.Body.String())
			}
			if mode != "valid" {
				if _, err := cache.GetAgentModels(context.Background(), a.Name, "codex"); err == nil {
					t.Fatal("rejected report changed picker catalog")
				}
			}

		})
	}
}
