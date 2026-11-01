package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/reasoningeffort"
	"github.com/matty-v/kyber/pkg/runtimedetect"
	"github.com/matty-v/kyber/pkg/runtimes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestNativeImageProofSurvivesControlPlaneUpdateAndSupportsDigests(t *testing.T) {
	for _, protocol := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(protocol), func(t *testing.T) {
			s, a, cache, c := nativeEffortFixture(t)
			ctx := context.Background()
			image := "ghcr.io/matty-v/kyber-codex@sha256:" + strings.Repeat("b", 64)
			s.RuntimeImages["codex"] = image
			var pod corev1.Pod
			key := types.NamespacedName{Namespace: fdNS, Name: a.Status.PodName}
			if err := s.K8sClient.Get(ctx, key, &pod); err != nil {
				t.Fatal(err)
			}
			pod.Spec.Containers[0].Image = image
			if err := s.K8sClient.Update(ctx, &pod); err != nil {
				t.Fatal(err)
			}
			server := NewInternalServer(briefstore.NewMemoryStore(), WithKubeClient(s.K8sClient, fdNS))
			server.SetRuntimeDetectCache(cache)
			body, _ := json.Marshal(map[string]any{"runtime": "codex", "podUID": c.PodUID, "nativeEffortProtocol": protocol, "models": c.Models})
			rr := httptest.NewRecorder()
			server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/internal/agents/wedge/runtime-catalog", bytes.NewReader(body)))
			if rr.Code != 204 {
				t.Fatalf("report=%d %s", rr.Code, rr.Body.String())
			}
			if err := s.K8sClient.Get(ctx, types.NamespacedName{Namespace: fdNS, Name: a.Name}, a); err != nil {
				t.Fatal(err)
			}
			s.BuildSHA = strings.Repeat("3", 40) // A subsequent control-plane-only upgrade.
			options, _ := s.agentEffortOptions(ctx, a, a.Spec.Model, a.Spec.RuntimeVersion, false)
			if (options.State == "supported") != (protocol == 2) {
				t.Fatalf("protocol=%d options=%+v", protocol, options)
			}
			if protocol == 2 && a.Status.EffortNativeRuntimeImage != image {
				t.Fatal("native image proof missing")
			}
		})
	}
}

// A new registered harness must make an explicit native-effort decision here.
func TestNativeEffortCoverageForEveryRegisteredHarness(t *testing.T) {
	cases := map[string]struct{ version, model, source string }{
		"codex":       {"0.166.0", "gpt-6.1-terra", "codex-model-list"},
		"claude-code": {"2.1.316", "claude-opus-5-5", "claude-sdk-initialize"},
		"hermes":      {"0.21.0", "openai/gpt-5.5", "hermes-native-catalog"},
	}
	for _, descriptor := range runtimes.Descriptors() {
		tc, exists := cases[descriptor.ID]
		if !exists {
			t.Fatalf("registered harness %s needs explicit effort coverage", descriptor.ID)
		}
		t.Run(descriptor.ID, func(t *testing.T) {
			s, a, cache, c := nativeEffortFixture(t)
			ctx := context.Background()
			a.Spec.Runtime, a.Spec.Model = descriptor.ID, tc.model
			if descriptor.ID == "hermes" {
				a.Spec.RuntimeVersion = ""
			}
			if err := s.K8sClient.Update(ctx, a); err != nil {
				t.Fatal(err)
			}
			a.Status.Runtime.Runtime, a.Status.Runtime.InstalledVersion = descriptor.ID, tc.version
			if err := s.K8sClient.Status().Update(ctx, a); err != nil {
				t.Fatal(err)
			}
			s.RuntimeImages[descriptor.ID] = "ghcr.io/matty-v/kyber-" + descriptor.ID + ":" + s.BuildSHA
			var pod corev1.Pod
			if err := s.K8sClient.Get(ctx, types.NamespacedName{Namespace: fdNS, Name: a.Status.PodName}, &pod); err != nil {
				t.Fatal(err)
			}
			pod.Spec.Containers[0].Image = s.RuntimeImages[descriptor.ID]
			if err := s.K8sClient.Update(ctx, &pod); err != nil {
				t.Fatal(err)
			}
			c.Runtime, c.Version = descriptor.ID, tc.version
			c.Models = []runtimedetect.Model{{ID: tc.model, ReasoningEffort: &runtimedetect.ReasoningEffortEvidence{Source: tc.source, HarnessVersion: tc.version, Levels: []string{"low", "high"}}}}
			if err := cache.PutEffortCatalog(ctx, a.Name, c); err != nil {
				t.Fatal(err)
			}
			got := effortTestRequest(s, http.MethodGet, "/api/v1/agents/wedge/effort-options", "")
			var options effortOptionsResponse
			if err := json.Unmarshal(got.Body.Bytes(), &options); err != nil {
				t.Fatal(err)
			}
			if got.Code != 200 || options.State != "supported" || len(options.Levels) != 2 {
				t.Fatalf("native options: %d %s", got.Code, got.Body.String())
			}
			for _, level := range []string{"high", ""} {
				body, _ := json.Marshal(map[string]string{"effort": level})
				got = effortTestRequest(s, http.MethodPost, "/api/v1/agents/wedge/set-effort", string(body))
				if got.Code != 200 {
					t.Fatalf("set/reset %q: %d %s", level, got.Code, got.Body.String())
				}
			}
		})
	}
}

func TestSeparateEffortCatalogDoesNotReplacePickerOrRefreshOldEvidence(t *testing.T) {
	for _, mode := range []string{"valid", "expired", "future", "missing-pod", "wrong-source"} {
		t.Run(mode, func(t *testing.T) {
			s, a, cache, c := nativeEffortFixture(t)
			server := NewInternalServer(briefstore.NewMemoryStore(), WithKubeClient(s.K8sClient, fdNS))
			server.SetRuntimeDetectCache(cache)
			picker := []runtimedetect.Model{{ID: "picker-model"}}
			if err := cache.PutAgentModels(context.Background(), a.Name, "codex", picker); err != nil {
				t.Fatal(err)
			}
			observed := time.Now().Add(-time.Minute)
			if mode == "expired" {
				observed = time.Now().Add(-3 * time.Hour)
			}
			if mode == "future" {
				observed = time.Now().Add(time.Hour)
			}
			if mode == "missing-pod" {
				c.PodUID = ""
			}
			if mode == "wrong-source" {
				c.Models[0].ReasoningEffort.Source = "claude-sdk-initialize"
			}
			body, _ := json.Marshal(map[string]any{"runtime": "codex", "podUID": c.PodUID, "nativeEffortProtocol": 2, "effortModels": c.Models, "observedAt": observed})
			rr := httptest.NewRecorder()
			server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/internal/agents/"+a.Name+"/runtime-catalog", bytes.NewReader(body)))
			want := 400
			if mode == "valid" {
				want = 204
			}
			if rr.Code != want {
				t.Fatalf("status=%d want=%d body=%s", rr.Code, want, rr.Body.String())
			}
			models, err := cache.GetAgentModels(context.Background(), a.Name, "codex")
			if err != nil || len(models) != 1 || models[0].ID != "picker-model" {
				t.Fatal("native evidence changed picker")
			}
			if mode == "valid" {
				got, err := cache.GetEffortCatalog(context.Background(), a.Name)
				if err != nil || !got.ObservedAt.Equal(observed) {
					t.Fatal("source observation time was refreshed")
				}
			}
		})
	}
}

func TestNativeEffortTransportAcrossHarnessesWithoutPickerContext(t *testing.T) {
	for runtime, source := range map[string]string{"codex": "codex-model-list", "claude-code": "claude-sdk-initialize", "hermes": "hermes-native-catalog"} {
		t.Run(runtime, func(t *testing.T) {
			s, a, cache, c := nativeEffortFixture(t)
			ctx := context.Background()
			a.Spec.Runtime = runtime
			if err := s.K8sClient.Update(ctx, a); err != nil {
				t.Fatal(err)
			}
			a.Status.Runtime.Runtime = runtime
			if err := s.K8sClient.Status().Update(ctx, a); err != nil {
				t.Fatal(err)
			}
			var pod corev1.Pod
			if err := s.K8sClient.Get(ctx, types.NamespacedName{Namespace: fdNS, Name: a.Status.PodName}, &pod); err != nil {
				t.Fatal(err)
			}
			image := "ghcr.io/matty-v/kyber-" + runtime + ":" + strings.Repeat("b", 40)
			pod.Spec.Containers[0].Image = image
			if err := s.K8sClient.Update(ctx, &pod); err != nil {
				t.Fatal(err)
			}
			c.Models[0].ReasoningEffort.Source = source
			server := NewInternalServer(briefstore.NewMemoryStore(), WithKubeClient(s.K8sClient, fdNS))
			server.SetRuntimeDetectCache(cache)
			body, _ := json.Marshal(map[string]any{"runtime": runtime, "podUID": c.PodUID, "nativeEffortProtocol": 2, "effortModels": c.Models, "observedAt": time.Now().Add(-time.Second)})
			rr := httptest.NewRecorder()
			server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/internal/agents/"+a.Name+"/runtime-catalog", bytes.NewReader(body)))
			if rr.Code != 204 {
				t.Fatalf("native transport: %d %s", rr.Code, rr.Body.String())
			}
			got, err := cache.GetEffortCatalog(ctx, a.Name)
			if err != nil || got.Runtime != runtime || len(got.Models) != 1 {
				t.Fatal("missing bound native evidence")
			}
			if err := s.K8sClient.Get(ctx, types.NamespacedName{Namespace: fdNS, Name: a.Name}, a); err != nil {
				t.Fatal(err)
			}
			if a.Status.EffortNativeRuntimeImage != image {
				t.Fatal("native image proof was not retained")
			}
			if _, err := cache.GetAgentModels(ctx, a.Name, runtime); err == nil {
				t.Fatal("context-free native metadata leaked into picker")
			}
		})
	}
}

func TestNativePrereleaseAdmissionRemainsLaunchable(t *testing.T) {
	s, a, cache, c := nativeEffortFixture(t)
	ctx := context.Background()
	version := "0.167.0-alpha.1"
	a.Spec.RuntimeVersion = version
	if err := s.K8sClient.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	a.Status.Runtime.InstalledVersion, a.Status.Runtime.RequestedVersion = version, version
	if err := s.K8sClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	c.Version, c.Models[0].ReasoningEffort.HarnessVersion = version, version
	if err := cache.PutEffortCatalog(ctx, a.Name, c); err != nil {
		t.Fatal(err)
	}
	rr := effortTestRequest(s, http.MethodPost, "/api/v1/agents/wedge/set-effort", `{"effort":"high"}`)
	if rr.Code != 200 {
		t.Fatalf("native prerelease rejected: %d %s", rr.Code, rr.Body.String())
	}
	if err := s.K8sClient.Get(ctx, types.NamespacedName{Namespace: fdNS, Name: a.Name}, a); err != nil {
		t.Fatal(err)
	}
	if err := reasoningeffort.ValidateLaunch(a.Spec.Runtime, a.Spec.RuntimeVersion, a.Spec.Model, false, a.Spec.Effort); err != nil {
		t.Fatalf("API admitted an unlaunchable native tuple: %v", err)
	}
}
