package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func switchTestServer(t *testing.T, source string, runner *fakeRuntimeRepairRunner) (*api.Server, *kyberv1.Agent) {
	t.Helper()
	agent := sampleAgentCRD("switch-test")
	agent.UID = types.UID("switch-test-uid")
	agent.Spec.Runtime = source
	agent.Spec.Model = "old-harness-model"
	agent.Spec.RuntimeVersion = "old-version"
	agent.Spec.SessionResume = true
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	agent.Status.Runtime.Runtime = source
	agent.Status.Runtime.InstalledVersion = "old-version"
	s := newTestPublicServer(t, testAPIKey)
	if err := s.K8sClient.Create(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	s.ValidRuntimes = map[string]bool{"claude-code": true, "codex": true, "hermes": true}
	s.RuntimeImages = map[string]string{"claude-code": "test/claude", "codex": "test/codex", "hermes": "test/hermes"}
	s.RuntimeRepairPlans = map[string]api.RuntimeRepairPlan{
		"claude-code": {Image: "test/claude", PackageName: "@anthropic-ai/claude-code", BinaryName: "claude"},
		"codex":       {Image: "test/codex", PackageName: "@openai/codex", BinaryName: "codex"},
	}
	s.RuntimeRepairRunner = runner
	return s, agent
}

func postSwitch(t *testing.T, s *api.Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/switch-test/switch-runtime", strings.NewReader(`{"runtime":"`+target+`"}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	return rr
}

func TestSwitchRuntimeBothDirectionsPreservesAgentAndClearsScopedSpec(t *testing.T) {
	for _, tc := range []struct{ source, target, packageName string }{
		{"codex", "claude-code", "@anthropic-ai/claude-code"},
		{"claude-code", "codex", "@openai/codex"},
	} {
		t.Run(tc.source+"-to-"+tc.target, func(t *testing.T) {
			runner := &fakeRuntimeRepairRunner{}
			s, original := switchTestServer(t, tc.source, runner)
			secretName := original.Name + "-codex-auth"
			if tc.source == "claude-code" {
				secretName = original.Name + "-oauth"
			}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: original.Namespace}, Data: map[string][]byte{"credential": []byte("old-credential")}}
			if err := s.K8sClient.Create(context.Background(), secret); err != nil {
				t.Fatal(err)
			}
			rr := postSwitch(t, s, tc.target)
			if rr.Code != http.StatusAccepted {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			if runner.calls != 1 || runner.plan.PackageName != tc.packageName || runner.plan.Version != "" {
				t.Fatalf("target preparation: calls=%d plan=%+v", runner.calls, runner.plan)
			}
			stored := &kyberv1.Agent{}
			if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: original.Name, Namespace: original.Namespace}, stored); err != nil {
				t.Fatal(err)
			}
			if stored.UID != original.UID || stored.Name != original.Name || stored.Spec.Machine != original.Spec.Machine || stored.Spec.Resources.Disk.Cmp(original.Spec.Resources.Disk) != 0 {
				t.Fatal("agent identity, placement, or PVC size changed")
			}
			if stored.Spec.Runtime != tc.target || stored.Spec.Model != "" || stored.Spec.RuntimeVersion != "" || stored.Spec.DesiredPhase != kyberv1.AgentPhaseNeedsAuth {
				t.Fatalf("switched spec=%+v", stored.Spec)
			}
			if !stored.Spec.SessionResume {
				t.Fatal("session resume preference unexpectedly removed")
			}
			readReq := scopedRequest(http.MethodGet, "/api/v1/agents/"+original.Name, testAPIKey)
			readResp := httptest.NewRecorder()
			buildTestHandler(s).ServeHTTP(readResp, readReq)
			var view api.AgentResponse
			if readResp.Code != http.StatusOK || json.Unmarshal(readResp.Body.Bytes(), &view) != nil {
				t.Fatalf("read after switch: status=%d body=%s", readResp.Code, readResp.Body.String())
			}
			if view.Phase != kyberv1.AgentPhaseRestarting || view.RuntimeVersion != nil || view.CurrentModel != "" {
				t.Fatalf("source runtime appeared healthy during handoff: %+v", view)
			}
			preserved := &corev1.Secret{}
			if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: secretName, Namespace: original.Namespace}, preserved); err != nil || string(preserved.Data["credential"]) != "old-credential" {
				t.Fatalf("source credential was lost: err=%v secret=%+v", err, preserved)
			}
		})
	}
}

func TestSwitchRuntimeRejectsUnconfiguredImageBeforePreparation(t *testing.T) {
	runner := &fakeRuntimeRepairRunner{}
	s, _ := switchTestServer(t, "codex", runner)
	s.RuntimeImages["claude-code"] = ""
	rr := postSwitch(t, s, "claude-code")
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "image.claudeCode.tag") || runner.calls != 0 {
		t.Fatalf("status=%d body=%s calls=%d", rr.Code, rr.Body.String(), runner.calls)
	}
}

func TestSwitchRuntimeLegacyObservationLooksTransient(t *testing.T) {
	s := newTestPublicServer(t, testAPIKey)
	agent := sampleAgentCRD("legacy-handoff")
	agent.Generation = 3
	agent.Spec.Runtime = "claude-code"
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseNeedsAuth
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	agent.Status.ObservedGeneration = 2
	agent.Status.Runtime.InstalledVersion = "source-version"
	agent.Status.CurrentModel = "source-model"
	if err := s.K8sClient.Create(t.Context(), agent); err != nil {
		t.Fatal(err)
	}
	req := scopedRequest(http.MethodGet, "/api/v1/agents/"+agent.Name, testAPIKey)
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	var view api.AgentResponse
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &view) != nil {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if view.Phase != kyberv1.AgentPhaseRestarting || view.CurrentModel != "" || view.RuntimeVersion != nil {
		t.Fatalf("legacy source observation appeared healthy: %+v", view)
	}
}

// After a switch the controller reaches NeedsAuth without creating a pod, so
// observedGeneration stays behind. The view must still say NeedsAuth, or the
// PWA never offers the target's authorization and the agent is stuck.
func TestSwitchRuntimeShowsNeedsAuthOnceControllerReachesIt(t *testing.T) {
	s := newTestPublicServer(t, testAPIKey)
	agent := sampleAgentCRD("switched")
	agent.Generation = 56
	agent.Spec.Runtime = "codex"
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseNeedsAuth
	agent.Status.Phase = kyberv1.AgentPhaseNeedsAuth
	agent.Status.ObservedGeneration = 55
	agent.Status.CurrentModel = "source-model"
	if err := s.K8sClient.Create(t.Context(), agent); err != nil {
		t.Fatal(err)
	}
	req := scopedRequest(http.MethodGet, "/api/v1/agents/"+agent.Name, testAPIKey)
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	var view api.AgentResponse
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &view) != nil {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if view.Phase != kyberv1.AgentPhaseNeedsAuth || view.CurrentModel != "" {
		t.Fatalf("phase=%q currentModel=%q, want NeedsAuth with no source model", view.Phase, view.CurrentModel)
	}
}

func TestSwitchRuntimeRejectsQueuedStop(t *testing.T) {
	runner := &fakeRuntimeRepairRunner{}
	s, original := switchTestServer(t, "codex", runner)
	current := &kyberv1.Agent{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: original.Name, Namespace: original.Namespace}, current); err != nil {
		t.Fatal(err)
	}
	current.Spec.DesiredPhase = kyberv1.AgentPhaseStopped
	if err := s.K8sClient.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	rr := postSwitch(t, s, "claude-code")
	if rr.Code != http.StatusConflict || runner.calls != 0 {
		t.Fatalf("status=%d body=%s calls=%d", rr.Code, rr.Body.String(), runner.calls)
	}
}

func TestSwitchRuntimeAcceptsAgentWithNoDesiredPhase(t *testing.T) {
	// Agents whose lifecycle was never driven through the API carry an empty
	// desiredPhase. That is no queued intent, not a pending one.
	for _, phase := range []kyberv1.AgentPhase{kyberv1.AgentPhaseRunning, kyberv1.AgentPhaseStopped, kyberv1.AgentPhaseFailed, kyberv1.AgentPhaseNeedsAuth} {
		t.Run(string(phase), func(t *testing.T) {
			runner := &fakeRuntimeRepairRunner{}
			s, original := switchTestServer(t, "claude-code", runner)
			current := &kyberv1.Agent{}
			key := types.NamespacedName{Name: original.Name, Namespace: original.Namespace}
			if err := s.K8sClient.Get(context.Background(), key, current); err != nil {
				t.Fatal(err)
			}
			current.Spec.DesiredPhase = ""
			current.Status.Phase = phase
			if err := s.K8sClient.Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			rr := postSwitch(t, s, "codex")
			if rr.Code != http.StatusAccepted || runner.calls != 1 {
				t.Fatalf("status=%d body=%s calls=%d", rr.Code, rr.Body.String(), runner.calls)
			}
			stored := &kyberv1.Agent{}
			if err := s.K8sClient.Get(context.Background(), key, stored); err != nil {
				t.Fatal(err)
			}
			if stored.Spec.Runtime != "codex" || stored.Spec.DesiredPhase != kyberv1.AgentPhaseNeedsAuth {
				t.Fatalf("runtime=%q desired=%q", stored.Spec.Runtime, stored.Spec.DesiredPhase)
			}
		})
	}
}

func TestSwitchRuntimeRejectsUnsupportedAuthAndChannel(t *testing.T) {
	runner := &fakeRuntimeRepairRunner{}
	s, original := switchTestServer(t, "codex", runner)
	rr := postSwitch(t, s, "hermes")
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "authentication") {
		t.Fatalf("auth: status=%d body=%s", rr.Code, rr.Body.String())
	}
	stored := &kyberv1.Agent{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: original.Name, Namespace: original.Namespace}, stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeAPIKey
	stored.Spec.Secrets.TelegramEnabled = true
	if err := s.K8sClient.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	rr = postSwitch(t, s, "claude-code")
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "telegram") || runner.calls != 0 {
		t.Fatalf("channel: status=%d body=%s calls=%d", rr.Code, rr.Body.String(), runner.calls)
	}
}

func TestSwitchRuntimePreparationFailureLeavesSource(t *testing.T) {
	runner := &fakeRuntimeRepairRunner{err: ErrSwitchTestFailure}
	s, original := switchTestServer(t, "codex", runner)
	rr := postSwitch(t, s, "claude-code")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	stored := &kyberv1.Agent{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: original.Name, Namespace: original.Namespace}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Runtime != "codex" || stored.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Fatalf("source mutated: %+v", stored.Spec)
	}
}

func TestSwitchRuntimeBackKeepsOriginalCredential(t *testing.T) {
	runner := &fakeRuntimeRepairRunner{}
	s, original := switchTestServer(t, "codex", runner)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: original.Name + "-codex-auth", Namespace: original.Namespace}, Data: map[string][]byte{"auth.json": []byte(`{"saved":true}`)}}
	if err := s.K8sClient.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if rr := postSwitch(t, s, "claude-code"); rr.Code != http.StatusAccepted {
		t.Fatalf("first switch: %d %s", rr.Code, rr.Body.String())
	}
	current := &kyberv1.Agent{}
	key := types.NamespacedName{Name: original.Name, Namespace: original.Namespace}
	if err := s.K8sClient.Get(context.Background(), key, current); err != nil {
		t.Fatal(err)
	}
	current.Status.Phase = kyberv1.AgentPhaseNeedsAuth
	if err := s.K8sClient.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if rr := postSwitch(t, s, "codex"); rr.Code != http.StatusAccepted {
		t.Fatalf("switch back: %d %s", rr.Code, rr.Body.String())
	}
	if err := s.K8sClient.Get(context.Background(), key, current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.Runtime != "codex" || current.Spec.DesiredPhase != kyberv1.AgentPhaseNeedsAuth {
		t.Fatalf("switch back spec=%+v", current.Spec)
	}
	if err := s.K8sClient.Get(context.Background(), client.ObjectKeyFromObject(secret), secret); err != nil || string(secret.Data["auth.json"]) != `{"saved":true}` {
		t.Fatalf("original credential missing: err=%v", err)
	}
}

var ErrSwitchTestFailure = &switchTestError{}

type switchTestError struct{}

func (*switchTestError) Error() string { return "installer failed" }

func postSwitchAuth(t *testing.T, s *api.Server, target string, authType kyberv1.AgentAuthType) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"runtime":"` + target + `","authType":"` + string(authType) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/switch-test/switch-runtime", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	return rr
}

// A subscription agent can move to a harness that only takes an API key by
// choosing that harness's mode, and come back to its untouched subscription.
func TestSwitchRuntimeChangesAuthModeAndBack(t *testing.T) {
	runner := &fakeRuntimeRepairRunner{}
	s, original := switchTestServer(t, "codex", runner)
	key := types.NamespacedName{Name: original.Name, Namespace: original.Namespace}
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: original.Name + "-codex-auth", Namespace: original.Namespace}, Data: map[string][]byte{"auth.json": []byte(`{"saved":true}`)}}
	if err := s.K8sClient.Create(context.Background(), source); err != nil {
		t.Fatal(err)
	}

	rr := postSwitchAuth(t, s, "hermes", kyberv1.AgentAuthTypeAPIKey)
	if rr.Code != http.StatusAccepted || !strings.Contains(rr.Body.String(), `"authType":"api-key"`) {
		t.Fatalf("to hermes: status=%d body=%s", rr.Code, rr.Body.String())
	}
	current := &kyberv1.Agent{}
	if err := s.K8sClient.Get(context.Background(), key, current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.Runtime != "hermes" || current.Spec.Secrets.AuthType != kyberv1.AgentAuthTypeAPIKey || current.Spec.DesiredPhase != kyberv1.AgentPhaseNeedsAuth {
		t.Fatalf("to hermes spec: runtime=%q auth=%q desired=%q", current.Spec.Runtime, current.Spec.Secrets.AuthType, current.Spec.DesiredPhase)
	}

	current.Status.Phase = kyberv1.AgentPhaseNeedsAuth
	if err := s.K8sClient.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if rr := postSwitchAuth(t, s, "codex", kyberv1.AgentAuthTypeOAuth); rr.Code != http.StatusAccepted {
		t.Fatalf("back to codex: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if err := s.K8sClient.Get(context.Background(), key, current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.Runtime != "codex" || current.Spec.Secrets.AuthType != kyberv1.AgentAuthTypeOAuth {
		t.Fatalf("back to codex spec: runtime=%q auth=%q", current.Spec.Runtime, current.Spec.Secrets.AuthType)
	}
	if err := s.K8sClient.Get(context.Background(), client.ObjectKeyFromObject(source), source); err != nil || string(source.Data["auth.json"]) != `{"saved":true}` {
		t.Fatalf("subscription credential not preserved: err=%v", err)
	}
}

func TestSwitchRuntimeValidatesTargetAuthMode(t *testing.T) {
	runner := &fakeRuntimeRepairRunner{}
	s, original := switchTestServer(t, "codex", runner)

	// Omitted mode keeps the current one, which Hermes lacks: the error names what it offers.
	rr := postSwitch(t, s, "hermes")
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "choose one of: api-key") {
		t.Fatalf("omitted: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := postSwitchAuth(t, s, "claude-code", "bogus"); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown mode: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Channels are checked against the chosen mode, not the current one.
	stored := &kyberv1.Agent{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: original.Name, Namespace: original.Namespace}, stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Secrets.TelegramEnabled = true
	if err := s.K8sClient.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	rr = postSwitchAuth(t, s, "claude-code", kyberv1.AgentAuthTypeAPIKey)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "telegram") {
		t.Fatalf("channel: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if runner.calls != 0 {
		t.Fatalf("preparation ran for a rejected switch: calls=%d", runner.calls)
	}
}

// Leaving Hermes: its root has no npm, so an npm harness cannot be staged
// ahead of time. The switch still commits and the target installs at boot.
func TestSwitchRuntimeCommitsWhenPreparationIsDeferred(t *testing.T) {
	runner := &fakeRuntimeRepairRunner{err: api.ErrRuntimePreparationDeferred}
	s, original := switchTestServer(t, "hermes", runner)
	current := &kyberv1.Agent{}
	key := types.NamespacedName{Name: original.Name, Namespace: original.Namespace}
	if err := s.K8sClient.Get(context.Background(), key, current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeAPIKey
	if err := s.K8sClient.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	rr := postSwitchAuth(t, s, "claude-code", kyberv1.AgentAuthTypeOAuth)
	if rr.Code != http.StatusAccepted || runner.calls != 1 {
		t.Fatalf("status=%d body=%s calls=%d", rr.Code, rr.Body.String(), runner.calls)
	}
	if err := s.K8sClient.Get(context.Background(), key, current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.Runtime != "claude-code" || current.Spec.Secrets.AuthType != kyberv1.AgentAuthTypeOAuth || current.Spec.DesiredPhase != kyberv1.AgentPhaseNeedsAuth {
		t.Fatalf("runtime=%q auth=%q desired=%q", current.Spec.Runtime, current.Spec.Secrets.AuthType, current.Spec.DesiredPhase)
	}
}
