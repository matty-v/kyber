package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/taskstore"
)

type countingLiveAuthority struct {
	base  *memoryOperatorAuthority
	loads atomic.Int64
}

func (a *countingLiveAuthority) Load(ctx context.Context) (operatorcredentials.Snapshot, error) {
	a.loads.Add(1)
	return a.base.Load(ctx)
}

type replayLiveTaskEvents struct{ idleLiveTaskEvents }

func (*replayLiveTaskEvents) ReadEvents(_ context.Context, p taskstore.EventReadParams) (*taskstore.EventPage, error) {
	if p.Authorization.PrincipalID != "client@example.invalid" || p.Authorization.TenantID != "tenant_client" {
		return nil, taskstore.ErrNotFound
	}
	page := &taskstore.EventPage{HighWater: 100, Terminal: true}
	for sequence := p.AfterSequence + 1; sequence <= 100; sequence++ {
		page.Events = append(page.Events, taskstore.TaskEvent{ID: fmt.Sprintf("event_%032x", sequence), TaskID: p.TaskID, Sequence: sequence, TaskVersion: sequence, Type: taskstore.EventTaskCreated, OccurredAt: time.Now().UTC(), PayloadVersion: "v1", Payload: json.RawMessage(`{"state":"queued"}`)})
	}
	return page, nil
}
func TestLiveTaskReplayDoesNotReloadAuthorityPerEvent(t *testing.T) {
	base := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	authority := &countingLiveAuthority{base: base}
	auth := registryFixtureAuthenticator(t, base)
	auth.authority = authority
	s := &Server{Namespace: "kyber-system", TaskStore: &replayLiveTaskEvents{}, auth: auth}
	request := registryBearer("client-key-fixture")
	request.URL.Path = "/api/v1/agents/alpha/tasks/task_11111111111111111111111111111111/events"
	recorder := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(recorder, request)
	if recorder.Code != 200 || strings.Count(recorder.Body.String(), "event: task.created") != 100 {
		t.Fatal("terminal replay did not return its events", recorder.Code)
	}
	if authority.loads.Load() >= 10 {
		t.Fatal("task replay performs authority IO per event", authority.loads.Load())
	}
	if strings.Contains(recorder.Body.String(), "client-key-fixture") {
		t.Fatal("cursor exposed signing material")
	}
}
