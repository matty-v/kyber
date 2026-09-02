package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/matty-v/kyber/pkg/taskstore"
)

func TestNativeA2AStateMapping(t *testing.T) {
	tests := map[taskstore.State]a2a.TaskState{
		taskstore.StateQueued: a2a.TaskStateSubmitted, taskstore.StateDispatched: a2a.TaskStateWorking,
		taskstore.StateCanceling: a2a.TaskStateWorking, taskstore.StateInputRequired: a2a.TaskStateInputRequired,
		taskstore.StateAuthRequired: a2a.TaskStateAuthRequired, taskstore.StateCanceled: a2a.TaskStateCanceled,
		taskstore.StateCompleted: a2a.TaskStateCompleted, taskstore.StateFailed: a2a.TaskStateFailed,
		taskstore.StateRejected: a2a.TaskStateRejected,
	}
	for native, want := range tests {
		if got := nativeA2AState(native); got != want {
			t.Errorf("%s=%s want %s", native, got, want)
		}
	}
}

func TestNativeA2AArtifactsPreserveTextDataAndAuthorizedFileReferences(t *testing.T) {
	task := &taskstore.Task{ID: "task_11111111111111111111111111111111", AgentName: "kiosk"}
	result := taskstore.Result{ID: "result_1", Name: "report", Parts: []taskstore.ResultPart{{Kind: taskstore.PartText, Text: "done"}, {Kind: taskstore.PartJSON, JSON: json.RawMessage(`{"ok":true}`)}, {Kind: taskstore.PartFile, File: &taskstore.FileMetadata{ObjectID: "private", Filename: "chart.png", MediaType: "image/png"}}}}
	artifact := nativeA2AArtifact(task, result)
	if len(artifact.Parts) != 3 || artifact.Parts[0].Text() != "done" || artifact.Parts[1].Data() == nil {
		t.Fatalf("artifact=%+v", artifact)
	}
	if got := string(artifact.Parts[2].URL()); got != "/api/v1/agents/kiosk/tasks/task_11111111111111111111111111111111/results/result_1/parts/2/content" {
		t.Fatalf("file URL=%q", got)
	}
}

func TestNativeA2AEventUsesCommittedEventState(t *testing.T) {
	now := time.Now().UTC()
	task := &taskstore.Task{ID: "task_1", Correlation: "ctx", State: taskstore.StateCompleted, UpdatedAt: now}
	event := taskstore.TaskEvent{ID: "event_1", TaskID: task.ID, Type: taskstore.EventTaskStateChanged, OccurredAt: now, Payload: json.RawMessage(`{"state":"dispatched"}`)}
	got, ok := nativeA2AEvent(task, event).(*a2a.TaskStatusUpdateEvent)
	if !ok || got.Status.State != a2a.TaskStateWorking {
		t.Fatalf("event=%T %+v", got, got)
	}
}
