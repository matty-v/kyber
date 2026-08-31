package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/taskstore"
)

func buildTaskHarness(t *testing.T, enabled bool) (http.Handler, *taskstore.MemoryStore) {
	t.Helper()
	store, err := taskstore.NewMemoryStore(taskstore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	agent := bareAgent("kiosk")
	agent.Spec.RequestReplyEnabled = true
	server := &api.Server{
		K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent).Build(),
		APIKey:    testAPIKey, Namespace: "kyber-system", TaskStore: store, TasksEnabled: enabled,
		Callers: []api.ScopedCaller{
			{Name: "writer", Key: requestWriteKey, Scopes: []string{"requests:write"}},
			{Name: "reader", Key: requestReadKey, Scopes: []string{"requests:read"}},
		},
	}
	return server.BuildHandler(), store
}

func taskRequest(t *testing.T, h http.Handler, method, target, key string, body any, idem string) *httptest.ResponseRecorder {
	t.Helper()
	var b []byte
	if body != nil {
		var err error
		b, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, target, bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	if idem != "" {
		r.Header.Set("Idempotency-Key", idem)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAgentTasksCreateReplayGetAndList(t *testing.T) {
	h, _ := buildTaskHarness(t, true)
	body := map[string]string{"prompt": "inspect the fleet", "correlation": "op-42"}
	first := taskRequest(t, h, http.MethodPost, "/api/v1/agents/kiosk/tasks", requestWriteKey, body, "stable-key")
	if first.Code != http.StatusAccepted {
		t.Fatalf("first create=%d %s", first.Code, first.Body.String())
	}
	var created struct {
		ID      string `json:"id"`
		State   string `json:"state"`
		Version int64  `json:"version"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.State != "queued" || created.Version != 1 {
		t.Fatalf("created=%+v", created)
	}
	replay := taskRequest(t, h, http.MethodPost, "/api/v1/agents/kiosk/tasks", requestWriteKey, body, "stable-key")
	if replay.Code != http.StatusAccepted || replay.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("replay=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	var replayed struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(replay.Body.Bytes(), &replayed)
	if replayed.ID != created.ID {
		t.Fatalf("replay id=%s want %s", replayed.ID, created.ID)
	}
	get := taskRequest(t, h, http.MethodGet, "/api/v1/agents/kiosk/tasks/"+created.ID, requestReadKey, nil, "")
	if get.Code != http.StatusOK {
		t.Fatalf("get=%d %s", get.Code, get.Body.String())
	}
	list := taskRequest(t, h, http.MethodGet, "/api/v1/agents/kiosk/tasks?limit=1", requestReadKey, nil, "")
	if list.Code != http.StatusOK {
		t.Fatalf("list=%d %s", list.Code, list.Body.String())
	}
}

func TestAgentTasksFailClosedAndAuthorize(t *testing.T) {
	h, _ := buildTaskHarness(t, false)
	disabled := taskRequest(t, h, http.MethodPost, "/api/v1/agents/kiosk/tasks", requestWriteKey, map[string]string{"prompt": "x"}, "")
	if disabled.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled=%d %s", disabled.Code, disabled.Body.String())
	}
	h, _ = buildTaskHarness(t, true)
	forbidden := taskRequest(t, h, http.MethodPost, "/api/v1/agents/kiosk/tasks", requestReadKey, map[string]string{"prompt": "x"}, "")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("forbidden=%d %s", forbidden.Code, forbidden.Body.String())
	}
}

func TestAgentTasksIdempotencyConflict(t *testing.T) {
	h, _ := buildTaskHarness(t, true)
	one := taskRequest(t, h, http.MethodPost, "/api/v1/agents/kiosk/tasks", requestWriteKey, map[string]string{"prompt": "one"}, "key")
	if one.Code != http.StatusAccepted {
		t.Fatal(one.Body.String())
	}
	two := taskRequest(t, h, http.MethodPost, "/api/v1/agents/kiosk/tasks", requestWriteKey, map[string]string{"prompt": "two"}, "key")
	if two.Code != http.StatusConflict {
		t.Fatalf("conflict=%d %s", two.Code, two.Body.String())
	}
}
