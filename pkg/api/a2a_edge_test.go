package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/capabilities"
	"github.com/matty-v/kyber/pkg/taskstore"
)

func buildA2AHarness(t *testing.T, enabled bool) (http.Handler, *taskstore.MemoryStore) {
	t.Helper()
	store, err := taskstore.NewMemoryStore(taskstore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	agent := bareAgent("kiosk")
	agent.Generation = 7
	agent.Spec.RequestReplyEnabled = true
	agent.Spec.PublicCapabilities = &kyberv1.AgentPublicCapabilities{SchemaVersion: capabilities.SchemaV1Alpha1, Identity: kyberv1.AgentPublicCapabilityIdentity{DisplayName: "Kiosk", Description: "Operates the kiosk."}, Capabilities: []kyberv1.AgentPublicCapability{{ID: "inspect", Version: "1", Name: "Inspect", Description: "Inspect the fleet.", InputModes: []string{"text/plain"}, OutputModes: []string{"application/json", "text/plain"}}}}
	_, digest, err := capabilities.NormalizeAndValidate(agent.Spec.PublicCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	agent.Status.PublicCapabilities = &kyberv1.AgentPublicCapabilitiesStatus{ObservedGeneration: agent.Generation, ManifestRevision: digest, Conditions: []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue}}, Capabilities: []kyberv1.AgentPublicCapabilityAvailability{{ID: "inspect", Availability: "available"}}}
	server := &api.Server{K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent).Build(), APIKey: testAPIKey, Namespace: "kyber-system", PublicURL: "https://kyber.example", TaskStore: store, TasksEnabled: true, A2AEnabled: enabled, Callers: []api.ScopedCaller{{Name: "a2a", PrincipalID: "principal_a2a", TenantID: "tenant_test", AgentResources: []string{"kyber-system/kiosk"}, Key: requestWriteKey, Scopes: []string{"tasks:create", "tasks:read", "tasks:list", "tasks:continue", "tasks:cancel", "task-results:read", "task-events:read"}}, {Name: "other", PrincipalID: "principal_other", TenantID: "tenant_test", AgentResources: []string{"kyber-system/kiosk"}, Key: "other-a2a-secret", Scopes: []string{"tasks:read", "tasks:list"}}}}
	return server.BuildHandler(), store
}

func a2aRequest(t *testing.T, h http.Handler, method, target, key, version string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(raw))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if version != "" {
		req.Header.Set("A2A-Version", version)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestA2AFeatureGateAuthVersionAndCard(t *testing.T) {
	disabled, _ := buildA2AHarness(t, false)
	if got := a2aRequest(t, disabled, http.MethodGet, "/a2a/v1/agents/kiosk/.well-known/agent-card.json", requestWriteKey, "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("disabled=%d %s", got.Code, got.Body.String())
	}
	h, _ := buildA2AHarness(t, true)
	if got := a2aRequest(t, h, http.MethodGet, "/a2a/v1/agents/kiosk/tasks", "", "1.0", nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated=%d %s", got.Code, got.Body.String())
	}
	if got := a2aRequest(t, h, http.MethodGet, "/a2a/v1/agents/kiosk/tasks", requestWriteKey, "", nil); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "version_not_supported") {
		t.Fatalf("missing version=%d %s", got.Code, got.Body.String())
	}
	card := a2aRequest(t, h, http.MethodGet, "/a2a/v1/agents/kiosk/.well-known/agent-card.json", requestWriteKey, "", nil)
	if card.Code != http.StatusOK || !strings.Contains(card.Body.String(), `"protocolBinding":"HTTP+JSON"`) || !strings.Contains(card.Body.String(), `"id":"inspect"`) || strings.Contains(card.Body.String(), "evidence") {
		t.Fatalf("card=%d %s", card.Code, card.Body.String())
	}
}

func TestA2ASendReplayGetListAndOwnerIsolation(t *testing.T) {
	h, _ := buildA2AHarness(t, true)
	body := map[string]any{"message": map[string]any{"messageId": "msg-1", "contextId": "ctx-1", "role": "ROLE_USER", "parts": []map[string]any{{"text": "inspect the fleet"}}}, "configuration": map[string]any{"returnImmediately": true}}
	first := a2aRequest(t, h, http.MethodPost, "/a2a/v1/agents/kiosk/message:send", requestWriteKey, "1.0", body)
	if first.Code != http.StatusOK {
		t.Fatalf("send=%d %s", first.Code, first.Body.String())
	}
	var response struct {
		Task struct {
			ID, ContextID string
			Status        struct{ State string }
		} `json:"task"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Task.ID == "" || response.Task.ContextID != "ctx-1" || response.Task.Status.State != "TASK_STATE_SUBMITTED" {
		t.Fatalf("response=%+v body=%s", response, first.Body.String())
	}
	replay := a2aRequest(t, h, http.MethodPost, "/a2a/v1/agents/kiosk/message:send", requestWriteKey, "1.0", body)
	var replayResponse struct {
		Task struct{ ID string } `json:"task"`
	}
	_ = json.Unmarshal(replay.Body.Bytes(), &replayResponse)
	if replay.Code != http.StatusOK || replayResponse.Task.ID != response.Task.ID {
		t.Fatalf("replay=%d %s", replay.Code, replay.Body.String())
	}
	get := a2aRequest(t, h, http.MethodGet, "/a2a/v1/agents/kiosk/tasks/"+response.Task.ID+"?historyLength=1", requestWriteKey, "1.0", nil)
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), "inspect the fleet") {
		t.Fatalf("get=%d %s", get.Code, get.Body.String())
	}
	list := a2aRequest(t, h, http.MethodGet, "/a2a/v1/agents/kiosk/tasks?pageSize=10&contextId=ctx-1", requestWriteKey, "1.0", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), response.Task.ID) {
		t.Fatalf("list=%d %s", list.Code, list.Body.String())
	}
	other := a2aRequest(t, h, http.MethodGet, "/a2a/v1/agents/kiosk/tasks/"+response.Task.ID, "other-a2a-secret", "1.0", nil)
	if other.Code != http.StatusNotFound {
		t.Fatalf("other=%d %s", other.Code, other.Body.String())
	}
}

func TestA2ARejectsUnsupportedInputAndPush(t *testing.T) {
	h, _ := buildA2AHarness(t, true)
	raw := map[string]any{"message": map[string]any{"messageId": "msg-raw", "role": "ROLE_USER", "parts": []map[string]any{{"raw": "YQ=="}}}, "configuration": map[string]any{"returnImmediately": true}}
	if got := a2aRequest(t, h, http.MethodPost, "/a2a/v1/agents/kiosk/message:send", requestWriteKey, "1.0", raw); got.Code != http.StatusBadRequest && got.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("raw=%d %s", got.Code, got.Body.String())
	}
	push := map[string]any{"message": map[string]any{"messageId": "msg-push", "role": "ROLE_USER", "parts": []map[string]any{{"text": "x"}}}, "configuration": map[string]any{"returnImmediately": true, "taskPushNotificationConfig": map[string]any{"url": "https://example.com"}}}
	if got := a2aRequest(t, h, http.MethodPost, "/a2a/v1/agents/kiosk/message:send", requestWriteKey, "1.0", push); got.Code != http.StatusBadRequest {
		t.Fatalf("push=%d %s", got.Code, got.Body.String())
	}
}

func TestA2ARejectsAmbiguousTrailingAndOversizedJSON(t *testing.T) {
	h, _ := buildA2AHarness(t, true)
	for name, raw := range map[string]string{
		"duplicate": `{"message":{"messageId":"one","messageId":"two","role":"ROLE_USER","parts":[{"text":"x"}]}}`,
		"trailing":  `{"message":{"messageId":"one","role":"ROLE_USER","parts":[{"text":"x"}]}} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/kiosk/message:send", strings.NewReader(raw))
			req.Header.Set("Authorization", "Bearer "+requestWriteKey)
			req.Header.Set("A2A-Version", "1.0")
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
	oversized := strings.Repeat("x", (256<<10)+1)
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/kiosk/message:send", strings.NewReader(oversized))
	req.Header.Set("Authorization", "Bearer "+requestWriteKey)
	req.Header.Set("A2A-Version", "1.0")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized=%d %s", rr.Code, rr.Body.String())
	}
}

func TestA2ACancelAndMediaNegotiation(t *testing.T) {
	h, _ := buildA2AHarness(t, true)
	body := map[string]any{"message": map[string]any{"messageId": "msg-cancel", "role": "ROLE_USER", "parts": []map[string]any{{"text": "wait"}}}, "configuration": map[string]any{"returnImmediately": true}}
	created := a2aRequest(t, h, http.MethodPost, "/a2a/v1/agents/kiosk/message:send", requestWriteKey, "1.0", body)
	var sent struct {
		Task struct{ ID string } `json:"task"`
	}
	_ = json.Unmarshal(created.Body.Bytes(), &sent)
	canceled := a2aRequest(t, h, http.MethodPost, "/a2a/v1/agents/kiosk/tasks/"+sent.Task.ID+":cancel", requestWriteKey, "1.0", map[string]any{})
	if canceled.Code != http.StatusOK || !strings.Contains(canceled.Body.String(), "TASK_STATE_CANCELED") {
		t.Fatalf("cancel=%d %s", canceled.Code, canceled.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/kiosk/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+requestWriteKey)
	req.Header.Set("A2A-Version", "1.0")
	req.Header.Set("Accept", "image/png")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotAcceptable {
		t.Fatalf("accept=%d %s", rr.Code, rr.Body.String())
	}
}

func TestA2AConcurrentSendsCreateIndependentTasks(t *testing.T) {
	h, _ := buildA2AHarness(t, true)
	const count = 8
	ids := make(chan string, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := map[string]any{"message": map[string]any{"messageId": "burst-" + string(rune('a'+i)), "role": "ROLE_USER", "parts": []map[string]any{{"text": "work"}}}, "configuration": map[string]any{"returnImmediately": true}}
			got := a2aRequest(t, h, http.MethodPost, "/a2a/v1/agents/kiosk/message:send", requestWriteKey, "1.0", body)
			if got.Code != http.StatusOK {
				t.Errorf("send %d=%d %s", i, got.Code, got.Body.String())
				return
			}
			var response struct {
				Task struct{ ID string } `json:"task"`
			}
			_ = json.Unmarshal(got.Body.Bytes(), &response)
			ids <- response.Task.ID
		}(i)
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != count {
		t.Fatalf("unique tasks=%d want %d", len(seen), count)
	}
}

type a2aEventMemoryStore struct{ *taskstore.MemoryStore }

func (s *a2aEventMemoryStore) EventSnapshot(ctx context.Context, agent taskstore.AgentRef, id string, auth taskstore.AuthorizationContext) (*taskstore.Task, int64, error) {
	t, err := s.GetAuthorized(ctx, agent, id, auth)
	return t, 0, err
}
func (*a2aEventMemoryStore) EventHighWater(context.Context, taskstore.AgentRef, string, taskstore.AuthorizationContext) (int64, error) {
	return 0, nil
}
func (*a2aEventMemoryStore) ReadEvents(context.Context, taskstore.EventReadParams) (*taskstore.EventPage, error) {
	return &taskstore.EventPage{Terminal: true}, nil
}

func TestA2AStreamingStartsWithCurrentTaskSnapshot(t *testing.T) {
	base, err := taskstore.NewMemoryStore(taskstore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	store := &a2aEventMemoryStore{MemoryStore: base}
	agent := bareAgent("kiosk")
	agent.Spec.RequestReplyEnabled = true
	server := &api.Server{K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent).Build(), APIKey: testAPIKey, Namespace: "kyber-system", TaskStore: store, TasksEnabled: true, A2AEnabled: true, Callers: []api.ScopedCaller{{Name: "a2a", PrincipalID: "principal_a2a", TenantID: "tenant_test", AgentResources: []string{"kyber-system/kiosk"}, Key: requestWriteKey, Scopes: []string{"tasks:create", "task-events:read"}}}}
	body := map[string]any{"message": map[string]any{"messageId": "msg-stream", "role": "ROLE_USER", "parts": []map[string]any{{"text": "stream"}}}}
	got := a2aRequest(t, server.BuildHandler(), http.MethodPost, "/a2a/v1/agents/kiosk/message:stream", requestWriteKey, "1.0", body)
	if got.Code != http.StatusOK || !strings.Contains(got.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(got.Body.String(), `"task"`) || !strings.Contains(got.Body.String(), "TASK_STATE_SUBMITTED") {
		t.Fatalf("stream=%d headers=%v body=%s", got.Code, got.Header(), got.Body.String())
	}
}

func TestA2AContinuesPausedNativeTask(t *testing.T) {
	h, store := buildA2AHarness(t, true)
	createBody := map[string]any{"message": map[string]any{"messageId": "msg-start", "contextId": "ctx-turns", "role": "ROLE_USER", "parts": []map[string]any{{"text": "deploy"}}}, "configuration": map[string]any{"returnImmediately": true}}
	created := a2aRequest(t, h, http.MethodPost, "/a2a/v1/agents/kiosk/message:send", requestWriteKey, "1.0", createBody)
	var sent struct {
		Task struct{ ID string } `json:"task"`
	}
	_ = json.Unmarshal(created.Body.Bytes(), &sent)
	ref := taskstore.AgentRef{Namespace: "kyber-system", Name: "kiosk"}
	if err := store.MarkDispatched(context.Background(), ref, sent.Task.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestInteraction(context.Background(), taskstore.RequestInteractionParams{Agent: ref, TaskID: sent.Task.ID, AttemptID: "attempt-1", InteractionID: "interaction-1", Type: taskstore.InteractionText, Question: "Which region?"}); err != nil {
		t.Fatal(err)
	}
	continueBody := map[string]any{"message": map[string]any{"messageId": "msg-answer", "taskId": sent.Task.ID, "contextId": "ctx-turns", "role": "ROLE_USER", "parts": []map[string]any{{"text": "us-central1"}}}, "configuration": map[string]any{"returnImmediately": true}}
	continued := a2aRequest(t, h, http.MethodPost, "/a2a/v1/agents/kiosk/message:send", requestWriteKey, "1.0", continueBody)
	if continued.Code != http.StatusOK || !strings.Contains(continued.Body.String(), "TASK_STATE_SUBMITTED") || !strings.Contains(continued.Body.String(), "us-central1") {
		t.Fatalf("continued=%d %s", continued.Code, continued.Body.String())
	}
}
