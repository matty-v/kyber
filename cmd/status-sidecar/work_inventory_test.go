package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/activity"
)

func TestWorkInventoryIdentityAndNoReplayRenewal(t *testing.T) {
	var reports []activity.Report
	registrations := 0
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-private-observer" {
			t.Error("observer credential not used")
		}
		if r.URL.Path == "/internal/activity-observers/fixture/register" {
			registrations++
			_ = json.NewEncoder(w).Encode(activity.RegistrationResponse{Epoch: "platform-issued"})
			return
		}
		var report activity.Report
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Error(err)
		}
		reports = append(reports, report)
		w.WriteHeader(204)
	}))
	defer cp.Close()
	f := newWorkInventoryForwarder(cp.Client(), config{AgentName: "fixture", ControlPlaneURL: cp.URL}, "platform-pod")
	f.tokenPath = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(f.tokenPath, []byte("fixture-private-observer"), 0600); err != nil {
		t.Fatal(err)
	}
	post := func(body any) int {
		t.Helper()
		b, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/work-inventory", bytes.NewReader(b))
		w := httptest.NewRecorder()
		f.ServeHTTP(w, r)
		return w.Code
	}
	if code := post(map[string]any{"podUID": "injected", "observedAt": time.Now(), "complete": true}); code != 400 {
		t.Fatalf("runtime identity accepted: %d", code)
	}
	for i := 0; i < 2; i++ {
		snapshot := activity.Snapshot{ObservedAt: time.Now(), Complete: true, Background: 1}
		if code := post(snapshot); code != 204 {
			t.Fatalf("valid snapshot: %d", code)
		}
	}
	if registrations != 1 || len(reports) != 2 {
		t.Fatalf("registrations=%d reports=%d", registrations, len(reports))
	}
	for i, r := range reports {
		if r.PodUID != "platform-pod" || r.Epoch != "platform-issued" || r.Sequence != int64(i+1) || r.BootID == "" {
			t.Fatal("missing trusted envelope")
		}
	}
	old := activity.Snapshot{ObservedAt: time.Now().Add(-time.Minute), Complete: true}
	if code := post(old); code != 400 || len(reports) != 2 {
		t.Fatal("old snapshot was forwarded with renewed timestamp")
	}
}
