package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/taskstore"
)

type fixedEventStore struct {
	taskstore.Store
	event   taskstore.TaskEvent
	expired bool
}

func (s *fixedEventStore) EventHighWater(context.Context, taskstore.AgentRef, string, taskstore.AuthorizationContext) (int64, error) {
	return s.event.Sequence, nil
}

func (s *fixedEventStore) EventSnapshot(ctx context.Context, a taskstore.AgentRef, id string, auth taskstore.AuthorizationContext) (*taskstore.Task, int64, error) {
	t, err := s.Store.GetAuthorized(ctx, a, id, auth)
	return t, s.event.Sequence, err
}

func (s *fixedEventStore) ReadEvents(_ context.Context, p taskstore.EventReadParams) (*taskstore.EventPage, error) {
	if p.Authorization.PrincipalID != "principal_writer" {
		return nil, taskstore.ErrNotFound
	}
	page := &taskstore.EventPage{RetainedFloor: s.event.Sequence, HighWater: s.event.Sequence, Terminal: true}
	if s.expired && p.Resume {
		page.RetainedFloor = s.event.Sequence + 2
		page.HighWater = s.event.Sequence + 2
		return page, taskstore.ErrEventCursorExpired
	}
	if p.AfterSequence < s.event.Sequence {
		page.Events = []taskstore.TaskEvent{s.event}
	}
	return page, nil
}

func TestTaskEventExpiredCursorRequiresSnapshot(t *testing.T) {
	h, id, store := buildTaskEventHarness(t)
	get := taskRequest(t, h, http.MethodGet, "/api/v1/agents/kiosk/tasks/"+id, requestWriteKey, nil, "")
	var snapshot struct {
		EventCursor string `json:"eventCursor"`
	}
	_ = json.Unmarshal(get.Body.Bytes(), &snapshot)
	store.expired = true
	r := httptest.NewRequest(http.MethodGet, "/api/v1/agents/kiosk/tasks/"+id+"/events", nil)
	r.Header.Set("Authorization", "Bearer "+requestWriteKey)
	r.Header.Set("Last-Event-ID", snapshot.EventCursor)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), `"snapshotRequired":true`) || !strings.Contains(w.Body.String(), `"currentCursor":`) {
		t.Fatalf("expired=%d %s", w.Code, w.Body.String())
	}
}

func buildTaskEventHarness(t *testing.T) (http.Handler, string, *fixedEventStore) {
	t.Helper()
	memory, err := taskstore.NewMemoryStore(taskstore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	id := "task_11111111111111111111111111111111"
	auth := taskstore.AuthorizationContext{TenantID: "tenant_test", PrincipalID: "principal_writer", AgentResourceID: "kyber-system/kiosk"}
	if _, err = memory.Create(context.Background(), taskstore.CreateParams{ID: id, Agent: taskstore.AgentRef{Namespace: "kyber-system", Name: "kiosk"}, CreatedBy: auth.PrincipalID, Prompt: "test", Authorization: auth, DeadlineAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	store := &fixedEventStore{Store: memory, event: taskstore.TaskEvent{ID: "event_11111111111111111111111111111111", TaskID: id, Sequence: 1, TaskVersion: 1, Type: taskstore.EventTaskCreated, OccurredAt: time.Now().UTC(), PayloadVersion: "v1", Payload: json.RawMessage(`{"state":"queued"}`)}}
	agent := bareAgent("kiosk")
	agent.Spec.RequestReplyEnabled = true
	server := &api.Server{K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent).Build(), APIKey: testAPIKey, Namespace: "kyber-system", TaskStore: store, TasksEnabled: true, Callers: []api.ScopedCaller{
		{Name: "writer", PrincipalID: auth.PrincipalID, TenantID: auth.TenantID, CredentialID: "credential_writer", CredentialGeneration: 1, AgentResources: []string{auth.AgentResourceID}, Key: requestWriteKey, Scopes: []string{"tasks:read", "task-events:read"}},
		{Name: "other", PrincipalID: "principal_other", TenantID: auth.TenantID, CredentialID: "credential_other", CredentialGeneration: 1, AgentResources: []string{auth.AgentResourceID}, Key: "other-events-key", Scopes: []string{"task-events:read"}},
	}}
	return server.BuildHandler(), id, store
}

func TestTaskEventSnapshotCursorAndTerminalReplay(t *testing.T) {
	h, id, _ := buildTaskEventHarness(t)
	get := taskRequest(t, h, http.MethodGet, "/api/v1/agents/kiosk/tasks/"+id, requestWriteKey, nil, "")
	if get.Code != http.StatusOK {
		t.Fatalf("get=%d %s", get.Code, get.Body.String())
	}
	var snapshot struct {
		EventCursor string `json:"eventCursor"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &snapshot); err != nil || snapshot.EventCursor == "" {
		t.Fatalf("snapshot cursor=%q err=%v", snapshot.EventCursor, err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/agents/kiosk/tasks/"+id+"/events", nil)
	r.Header.Set("Authorization", "Bearer "+requestWriteKey)
	r.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "event: task.created\n") || !strings.Contains(w.Body.String(), "\"sequence\":1") {
		t.Fatalf("stream=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") || w.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("stream cache headers=%v", w.Header())
	}

	resumed := httptest.NewRequest(http.MethodGet, "/api/v1/agents/kiosk/tasks/"+id+"/events", nil)
	resumed.Header.Set("Authorization", "Bearer "+requestWriteKey)
	resumed.Header.Set("Last-Event-ID", snapshot.EventCursor)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, resumed)
	if rw.Code != http.StatusOK || strings.Contains(rw.Body.String(), "event: task.created") {
		t.Fatalf("resumed=%d body=%q", rw.Code, rw.Body.String())
	}
}

func TestTaskEventCursorRejectsTamperingPrincipalAndConflict(t *testing.T) {
	h, id, _ := buildTaskEventHarness(t)
	get := taskRequest(t, h, http.MethodGet, "/api/v1/agents/kiosk/tasks/"+id, requestWriteKey, nil, "")
	var snapshot struct {
		EventCursor string `json:"eventCursor"`
	}
	_ = json.Unmarshal(get.Body.Bytes(), &snapshot)

	for name, tc := range map[string][3]string{
		"tampered":        {requestWriteKey, snapshot.EventCursor + "x", ""},
		"cross-principal": {"other-events-key", snapshot.EventCursor, ""},
		"conflicting":     {requestWriteKey, snapshot.EventCursor, snapshot.EventCursor + "x"},
	} {
		t.Run(name, func(t *testing.T) {
			key, header, query := tc[0], tc[1], tc[2]
			target := "/api/v1/agents/kiosk/tasks/" + id + "/events"
			if query != "" {
				target += "?cursor=" + query
			}
			r := httptest.NewRequest(http.MethodGet, target, nil)
			r.Header.Set("Authorization", "Bearer "+key)
			r.Header.Set("Last-Event-ID", header)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_cursor") {
				t.Fatalf("response=%d %s", w.Code, w.Body.String())
			}
		})
	}
}
