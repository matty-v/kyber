package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTaskReceiptForwarderPreservesStatusAndBody(t *testing.T) {
	var method, path, body string
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"receipt":{"taskId":"task_1"}}`))
	}))
	defer cp.Close()
	handler := taskReceiptForwarder(cp.Client(), config{AgentName: "alice", ControlPlaneURL: cp.URL})
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/task-receipts", strings.NewReader(`{"taskId":"task_1"}`)))
	if rec.Code != http.StatusCreated || method != http.MethodPost || path != "/internal/agents/alice/task-receipts" || body != `{"taskId":"task_1"}` {
		t.Fatalf("code=%d method=%q path=%q body=%q response=%q", rec.Code, method, path, body, rec.Body.String())
	}
}
