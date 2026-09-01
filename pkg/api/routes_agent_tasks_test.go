package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/taskobject"
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
			{Name: "other-writer", Key: "other-writer-secret", Scopes: []string{"requests:write"}},
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

func TestAgentTaskCancelQueuedAndReplay(t *testing.T) {
	h, _ := buildTaskHarness(t, true)
	created := taskRequest(t, h, http.MethodPost, "/api/v1/agents/kiosk/tasks", requestWriteKey, map[string]string{"prompt": "obsolete work"}, "create-cancel-test")
	if created.Code != http.StatusAccepted {
		t.Fatal(created.Body.String())
	}
	var task struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	target := "/api/v1/agents/kiosk/tasks/" + task.ID + "/cancel"
	denied := taskRequest(t, h, http.MethodPost, target, "other-writer-secret", map[string]string{"reason": "not mine"}, "other-cancel-key")
	if denied.Code != http.StatusNotFound {
		t.Fatalf("cross-owner cancel=%d %s", denied.Code, denied.Body.String())
	}
	canceled := taskRequest(t, h, http.MethodPost, target, requestWriteKey, map[string]string{"reason": "superseded"}, "cancel-key")
	if canceled.Code != http.StatusOK {
		t.Fatalf("cancel=%d %s", canceled.Code, canceled.Body.String())
	}
	var response struct {
		State  string `json:"state"`
		Cancel struct {
			Applied bool   `json:"applied"`
			Scope   string `json:"scope"`
		} `json:"cancel"`
	}
	if err := json.Unmarshal(canceled.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.State != "canceled" || !response.Cancel.Applied || response.Cancel.Scope != "future_task_work" {
		t.Fatalf("response=%+v", response)
	}
	replay := taskRequest(t, h, http.MethodPost, target, requestWriteKey, map[string]string{"reason": "superseded"}, "cancel-key")
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("replay=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	conflict := taskRequest(t, h, http.MethodPost, target, requestWriteKey, map[string]string{"reason": "different"}, "cancel-key")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict=%d %s", conflict.Code, conflict.Body.String())
	}
}

func TestAgentTaskFileDownloadIsAuthorizedAndRanged(t *testing.T) {
	store, err := taskstore.NewMemoryStore(taskstore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	a := taskstore.AgentRef{Namespace: "kyber-system", Name: "kiosk"}
	created, err := store.Create(t.Context(), taskstore.CreateParams{ID: "task_11111111111111111111111111111111", Agent: a, CreatedBy: "reader", Prompt: "make report"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDispatched(t.Context(), a, created.Task.ID, created.Task.Version); err != nil {
		t.Fatal(err)
	}
	objects := taskobject.NewMemoryStore()
	if err := objects.Put(t.Context(), "private-key", strings.NewReader("abcdef"), 6, taskobject.PutOptions{Filename: "report.pdf", ContentType: "application/pdf"}); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.PublishResult(t.Context(), a, created.Task.ID, "attempt_22222222222222222222222222222222", taskstore.Result{
		ID: "result_33333333333333333333333333333333", Name: "report", Parts: []taskstore.ResultPart{{ID: "part_0", Kind: taskstore.PartFile, File: &taskstore.FileMetadata{ObjectID: "private-key", Filename: "report.pdf", MediaType: "application/pdf", SizeBytes: 6, SHA256: "digest", ScanStatus: "not_configured"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := bareAgent("kiosk")
	agent.Spec.RequestReplyEnabled = true
	server := &api.Server{K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent).Build(), APIKey: testAPIKey, Namespace: "kyber-system", TaskStore: store, TaskObjectStore: objects, TasksEnabled: true, Callers: []api.ScopedCaller{{Name: "reader", Key: requestReadKey, Scopes: []string{"requests:read"}}}}
	if server.TaskObjectStore == nil {
		t.Fatal("task object store was not assigned")
	}
	h := server.BuildHandler()
	target := "/api/v1/agents/kiosk/tasks/task_11111111111111111111111111111111/results/result_33333333333333333333333333333333/parts/0/content"
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("Authorization", "Bearer "+requestReadKey)
	r.Header.Set("Range", "bytes=1-3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent || w.Body.String() != "bcd" || w.Header().Get("Content-Disposition") != `attachment; filename="report.pdf"` {
		t.Fatalf("download=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, target, nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized=%d", unauthorized.Code)
	}
}
