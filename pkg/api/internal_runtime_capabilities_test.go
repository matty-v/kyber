package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRuntimeCapabilitiesRejectsStaleAndUndeclaredEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		code   int
	}{
		{"current", func(map[string]any) {}, 204},
		{"old pod", func(m map[string]any) { m["podUID"] = "old" }, 409},
		{"wrong runtime", func(m map[string]any) { m["runtime"] = "claude-code" }, 409},
		{"unknown runtime", func(m map[string]any) { m["runtime"] = "unregistered" }, 400},
		{"undeclared", func(m map[string]any) { m["features"] = map[string]bool{"magic": true} }, 400},
		{"future contract", func(m map[string]any) { m["contractVersion"] = "2" }, 400},
		{"extra field", func(m map[string]any) { m["secret"] = "must-not-be-accepted" }, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = kyberv1.AddToScheme(scheme)
			agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "test", Generation: 3}}
			agent.Spec.Runtime = "codex"
			agent.Status.PodName = "agent-fixture"
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-fixture", Namespace: "test", UID: types.UID("current")}}
			client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent).WithObjects(agent, pod).Build()
			server := &InternalServer{k8sClient: client, namespace: "test"}
			body := map[string]any{"runtime": "codex", "contractVersion": "1.0", "installedVersion": "fixture-version", "podUID": "current", "agentGeneration": 3, "features": map[string]bool{"job-turn-hooks": true}}
			tc.mutate(body)
			raw, _ := json.Marshal(body)
			req := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
			w := httptest.NewRecorder()
			server.handleRuntimeCapabilities(w, req, "fixture")
			if w.Code != tc.code {
				t.Fatalf("status %d want %d: %s", w.Code, tc.code, w.Body.String())
			}
			got := &kyberv1.Agent{}
			if err := client.Get(req.Context(), types.NamespacedName{Namespace: "test", Name: "fixture"}, got); err != nil {
				t.Fatal(err)
			}
			if (got.Status.Runtime.Capabilities != nil) != (tc.code == 204) {
				t.Fatal("unexpected observation mutation")
			}
		})
	}
}

func TestRuntimeEffortImageProofRequiresCurrentOwnedPod(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprint(owned), func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = kyberv1.AddToScheme(scheme)
			agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "test", UID: types.UID("agent-current"), Generation: 3}}
			agent.Spec.Runtime = "codex"
			agent.Status.PodName = "agent-fixture"
			image := "ghcr.io/matty-v/kyber-codex:1111111111111111111111111111111111111111"
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-fixture", Namespace: "test", UID: types.UID("pod-current")}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: image}}}}
			if owned {
				pod.OwnerReferences = []metav1.OwnerReference{{Kind: "Agent", Name: agent.Name, UID: agent.UID}}
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent).WithObjects(agent, pod).Build()
			server := &InternalServer{k8sClient: c, usageAgentReader: c, namespace: "test"}
			body := map[string]any{"runtime": "codex", "contractVersion": "1.0", "installedVersion": "0.146.0", "podUID": "pod-current", "agentGeneration": 3, "features": map[string]bool{"reasoning-effort": true}}
			data, _ := json.Marshal(body)
			req := httptest.NewRequest(http.MethodPost, "/runtime-capabilities", bytes.NewReader(data))
			rr := httptest.NewRecorder()
			server.handleRuntimeCapabilities(rr, req, "fixture")
			if rr.Code != 204 {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			if err := c.Get(req.Context(), types.NamespacedName{Namespace: "test", Name: "fixture"}, agent); err != nil {
				t.Fatal(err)
			}
			if (agent.Status.EffortRuntimeImage == image) != owned {
				t.Fatalf("owned=%v image proof=%q", owned, agent.Status.EffortRuntimeImage)
			}
		})
	}
}

type capabilityRacingReader struct {
	client.Reader
	onPodRead func()
}

func (r *capabilityRacingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := r.Reader.Get(ctx, key, obj, opts...)
	if _, ok := obj.(*corev1.Pod); err == nil && ok && r.onPodRead != nil {
		hook := r.onPodRead
		r.onPodRead = nil
		hook()
	}
	return err
}

func TestRuntimeEffortReportCannotCommitAfterAgentChanges(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = kyberv1.AddToScheme(scheme)
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "test", UID: types.UID("agent-current"), Generation: 3}}
	agent.Spec.Runtime = "codex"
	agent.Status.PodName = "agent-fixture"
	image := "ghcr.io/matty-v/kyber-codex:1111111111111111111111111111111111111111"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-fixture", Namespace: "test", UID: types.UID("pod-current"), OwnerReferences: []metav1.OwnerReference{{Kind: "Agent", Name: agent.Name, UID: agent.UID}}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: image}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent).WithObjects(agent, pod).Build()
	key := types.NamespacedName{Namespace: "test", Name: "fixture"}
	reader := &capabilityRacingReader{Reader: c, onPodRead: func() {
		var current kyberv1.Agent
		if err := c.Get(context.Background(), key, &current); err != nil {
			t.Fatal(err)
		}
		current.Spec.Model = "changed-after-read"
		if err := c.Update(context.Background(), &current); err != nil {
			t.Fatal(err)
		}
	}}
	server := &InternalServer{k8sClient: c, usageAgentReader: reader, namespace: "test"}
	body := map[string]any{"runtime": "codex", "contractVersion": "1.0", "installedVersion": "0.146.0", "podUID": "pod-current", "agentGeneration": 3, "features": map[string]bool{"reasoning-effort": true}}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/runtime-capabilities", bytes.NewReader(data))
	rr := httptest.NewRecorder()
	server.handleRuntimeCapabilities(rr, req, "fixture")
	if rr.Code != 503 {
		t.Fatalf("stale report status=%d body=%s", rr.Code, rr.Body.String())
	}
	if err := c.Get(context.Background(), key, agent); err != nil {
		t.Fatal(err)
	}
	if agent.Status.EffortRuntimeImage != "" || agent.Status.Runtime.Capabilities != nil {
		t.Fatal("stale report committed capabilities/image proof")
	}
}
