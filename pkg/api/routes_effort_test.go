package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"k8s.io/apimachinery/pkg/types"
)

func effortTestAgent(t *testing.T, s *Server, phase kyberv1.AgentPhase, effort string) *kyberv1.Agent {
	t.Helper()
	s.BuildSHA = "1111111111111111111111111111111111111111"
	s.RuntimeImages = map[string]string{"claude-code": "ghcr.io/matty-v/kyber-claude-code:" + s.BuildSHA, "codex": "ghcr.io/matty-v/kyber-codex:" + s.BuildSHA}
	var agent kyberv1.Agent
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Namespace: fdNS, Name: "wedge"}, &agent); err != nil {
		t.Fatal(err)
	}
	agent.Spec.Model = "claude-opus-4-7"
	agent.Spec.RuntimeVersion = "2.1.119"
	agent.Spec.Effort = effort
	agent.Spec.DesiredPhase = phase
	if err := s.K8sClient.Update(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	agent.Status.Phase = phase
	if err := s.K8sClient.Status().Update(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	return &agent
}
func effortTestRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer model-validation-fixture")
	rr := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(rr, req)
	return rr
}
func TestEffortLifecycleAndReset(t *testing.T) {
	for _, phase := range []kyberv1.AgentPhase{kyberv1.AgentPhaseRunning, kyberv1.AgentPhaseStarting, kyberv1.AgentPhaseRestarting, kyberv1.AgentPhaseStopped, kyberv1.AgentPhaseFailed, kyberv1.AgentPhaseNeedsAuth} {
		t.Run(string(phase), func(t *testing.T) {
			s := newSetModelServer(t, nil)
			effortTestAgent(t, s, phase, "")
			rr := effortTestRequest(s, http.MethodPost, "/api/v1/agents/wedge/set-effort", `{"effort":"high"}`)
			if rr.Code != 200 {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var got kyberv1.Agent
			if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Namespace: fdNS, Name: "wedge"}, &got); err != nil {
				t.Fatal(err)
			}
			want := phase
			if phase == kyberv1.AgentPhaseRunning || phase == kyberv1.AgentPhaseStarting {
				want = kyberv1.AgentPhaseRestarting
			}
			if got.Spec.Effort != "high" || got.Spec.DesiredPhase != want {
				t.Fatalf("effort=%q desired=%q", got.Spec.Effort, got.Spec.DesiredPhase)
			}
			// Default is still a recovery path if a future runtime/model is unqualified.
			got.Spec.RuntimeVersion = "999.0.0"
			if err := s.K8sClient.Update(context.Background(), &got); err != nil {
				t.Fatal(err)
			}
			rr = effortTestRequest(s, http.MethodPost, "/api/v1/agents/wedge/set-effort", `{"effort":""}`)
			if rr.Code != 200 {
				t.Fatalf("reset status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}
func TestEffortRejectsMalformedAndUnsupportedBeforeMutation(t *testing.T) {
	for _, body := range []string{`{}`, `{"effort":null}`, `{"effort":3}`, `{"effort":"invented"}`, `{"effort":"high","force":true}`} {
		t.Run(body, func(t *testing.T) {
			s := newSetModelServer(t, nil)
			before := effortTestAgent(t, s, kyberv1.AgentPhaseStopped, "")
			rr := effortTestRequest(s, http.MethodPost, "/api/v1/agents/wedge/set-effort", body)
			if rr.Code != 400 {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var got kyberv1.Agent
			_ = s.K8sClient.Get(context.Background(), types.NamespacedName{Namespace: fdNS, Name: "wedge"}, &got)
			if got.Spec.Effort != "" || got.ResourceVersion != before.ResourceVersion {
				t.Fatal("invalid request mutated Agent")
			}
		})
	}
}
func TestEffortNoopAndAuditFailure(t *testing.T) {
	s := newSetModelServer(t, nil)
	before := effortTestAgent(t, s, kyberv1.AgentPhaseRunning, "high")
	rr := effortTestRequest(s, http.MethodPost, "/api/v1/agents/wedge/set-effort", `{"effort":"high"}`)
	if rr.Code != 200 {
		t.Fatalf("noop status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got kyberv1.Agent
	_ = s.K8sClient.Get(context.Background(), types.NamespacedName{Namespace: fdNS, Name: "wedge"}, &got)
	if got.ResourceVersion != before.ResourceVersion || got.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Fatal("no-op restarted or mutated Agent")
	}
	s.SecurityAudit = nil
	rr = effortTestRequest(s, http.MethodPost, "/api/v1/agents/wedge/set-effort", `{"effort":"low"}`)
	if rr.Code != 503 {
		t.Fatalf("audit outage status=%d", rr.Code)
	}
	_ = s.K8sClient.Get(context.Background(), types.NamespacedName{Namespace: fdNS, Name: "wedge"}, &got)
	if got.Spec.Effort != "high" {
		t.Fatal("audit outage allowed mutation")
	}
}
func TestEffortModelTransitionCannotBypassWithForce(t *testing.T) {
	s := newSetModelServer(t, nil)
	effortTestAgent(t, s, kyberv1.AgentPhaseStopped, "xhigh")
	rr := effortTestRequest(s, http.MethodPost, "/api/v1/agents/wedge/set-model", `{"model":"claude-sonnet-4-6","force":true}`)
	if rr.Code != 400 {
		t.Fatalf("model transition status=%d body=%s", rr.Code, rr.Body.String())
	}
}
func TestEffortBoundaryExactResources(t *testing.T) {
	s := &Server{Namespace: "kyber-system"}
	for _, target := range []string{"alpha", "beta"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			action := "effort-options"
			if method == http.MethodPost {
				action = "set-effort"
			}
			caller := &Caller{Name: "writer", Scopes: newScopeSet(ScopeLifecycleWrite), AgentResources: newResourceSet("kyber-system/alpha")}
			req := reqWithCaller(caller)
			req.Method = method
			req.URL = httptest.NewRequest(method, "/api/v1/agents/"+target+"/"+action, nil).URL
			rr := httptest.NewRecorder()
			called := false
			s.operatorBoundary(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(rr, req)
			if called != (target == "alpha") {
				t.Fatalf("%s %s dispatched=%v", method, target, called)
			}
		}
	}
}

func TestEffortOptionsAndDedicatedMutationRoute(t *testing.T) {
	s := newSetModelServer(t, nil)
	effortTestAgent(t, s, kyberv1.AgentPhaseStopped, "")
	rr := effortTestRequest(s, http.MethodGet, "/api/v1/agents/wedge/effort-options", "")
	if rr.Code != 200 || !bytes.Contains(rr.Body.Bytes(), []byte(`"state":"supported"`)) {
		t.Fatalf("options status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = effortTestRequest(s, http.MethodPatch, "/api/v1/agents/wedge", `{"effort":"high"}`)
	if rr.Code != 400 {
		t.Fatalf("generic patch silently accepted effort: %d %s", rr.Code, rr.Body.String())
	}
	// Unsupported runtimes must not inherit another provider's fleet defaults or
	// require a Kubernetes read to explain their unsupported capability.
	empty := &Server{FleetDefaultsConfigMapName: "must-not-read"}
	model, version, err := empty.effortTuple(context.Background(), &kyberv1.Agent{Spec: kyberv1.AgentSpec{Runtime: "hermes"}})
	if err != nil || model != "" || version != "" {
		t.Fatalf("unsupported runtime inherited defaults: %q %q %v", model, version, err)
	}
}

func TestEffortArchiveHoldDefersRestart(t *testing.T) {
	s := newSetModelServer(t, nil)
	agent := effortTestAgent(t, s, kyberv1.AgentPhaseRunning, "")
	agent.Annotations = map[string]string{kyberv1.AnnotationArchiveHold: "fixture"}
	if err := s.K8sClient.Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	rr := effortTestRequest(s, http.MethodPost, "/api/v1/agents/wedge/set-effort", `{"effort":"high"}`)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got kyberv1.Agent
	_ = s.K8sClient.Get(context.Background(), types.NamespacedName{Namespace: fdNS, Name: "wedge"}, &got)
	if got.Spec.Effort != "high" || got.Spec.DesiredPhase != kyberv1.AgentPhaseRunning || got.Annotations[kyberv1.AnnotationArchiveHold] != "fixture" {
		t.Fatal("effort bypassed archive owner")
	}
}

func TestEffortRejectsOldImageAndStillPermitsReset(t *testing.T) {
	s := newSetModelServer(t, nil)
	effortTestAgent(t, s, kyberv1.AgentPhaseStopped, "high")
	s.RuntimeImages["claude-code"] = "ghcr.io/matty-v/kyber-claude-code:2222222222222222222222222222222222222222"
	rr := effortTestRequest(s, http.MethodGet, "/api/v1/agents/wedge/effort-options", "")
	if rr.Code != 200 || !bytes.Contains(rr.Body.Bytes(), []byte(`"state":"unknown"`)) {
		t.Fatalf("old image advertised support: %d %s", rr.Code, rr.Body.String())
	}
	rr = effortTestRequest(s, http.MethodPost, "/api/v1/agents/wedge/set-effort", `{"effort":"low"}`)
	if rr.Code != 400 {
		t.Fatalf("old image accepted effort: %d %s", rr.Code, rr.Body.String())
	}
	rr = effortTestRequest(s, http.MethodPost, "/api/v1/agents/wedge/set-effort", `{"effort":""}`)
	if rr.Code != 200 {
		t.Fatalf("reset blocked by image mismatch: %d %s", rr.Code, rr.Body.String())
	}
}
