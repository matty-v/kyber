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
