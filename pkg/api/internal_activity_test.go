package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/activity"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/podtoken"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestActivityObserverAdmission(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = kyberv1.AddToScheme(scheme)
	now := time.Now().UTC().Truncate(time.Microsecond)
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "test", UID: "agent-life"}, Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning, PodName: "agent-fixture"}}
	controller := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: agent.Status.PodName, Namespace: "test", UID: "pod-life", OwnerReferences: []metav1.OwnerReference{{Kind: "Agent", Name: agent.Name, UID: agent.UID, Controller: &controller}}}, Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: "kyber-status-sidecar", ContainerID: "containerd://first", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Minute))}}}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent, pod).WithObjects(agent, pod).Build()
	key := []byte("unit-test-only-observer-signing-key")
	s := &InternalServer{k8sClient: c, usageAgentReader: c, namespace: "test", internalAuth: NewHMACInternalAuthenticator(key), internalAuthGrace: true}
	token := podtoken.Sign(activity.ObserverIdentity(agent.Name, string(agent.UID)), key)
	post := func(path, credential string, body any) *httptest.ResponseRecorder {
		t.Helper()
		data, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/internal/activity-observers/fixture/"+path, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+credential)
		w := httptest.NewRecorder()
		s.handleActivityObserver(w, r)
		return w
	}
	reg := activity.Registration{PodUID: "pod-life", BootID: "first-boot", BootedAt: now.Add(-30 * time.Second)}
	for _, credential := range []string{"", podtoken.Sign(agent.Name, key), podtoken.Sign(activity.ObserverIdentity(agent.Name, "old-agent-life"), key)} {
		if w := post("register", credential, reg); w.Code != 401 && w.Code != 403 {
			t.Fatalf("invalid credential admitted: %d", w.Code)
		}
	}
	w := post("register", token, reg)
	if w.Code != 200 {
		t.Fatalf("registration: %d %s", w.Code, w.Body.String())
	}
	var registered activity.RegistrationResponse
	_ = json.Unmarshal(w.Body.Bytes(), &registered)
	report := activity.Report{Registration: reg, Epoch: registered.Epoch, Sequence: 1, Snapshot: activity.Snapshot{ObservedAt: now.Add(-time.Second), LastWorkedAt: now.Add(-time.Second), Complete: true, Foreground: 1}}
	if w = post("inventory", token, report); w.Code != 204 {
		t.Fatalf("inventory: %d %s", w.Code, w.Body.String())
	}
	var baseline kyberv1.Agent
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(agent), &baseline)
	if w = post("inventory", token, report); w.Code != 409 {
		t.Fatalf("replay: %d", w.Code)
	}
	for _, tc := range []struct {
		name   string
		change func(*activity.Report)
	}{
		{"future", func(r *activity.Report) { r.ObservedAt = now.Add(time.Hour) }},
		{"stale", func(r *activity.Report) { r.ObservedAt = now.Add(-time.Minute) }},
		{"repeat timestamp", func(r *activity.Report) {}},
		{"bad count", func(r *activity.Report) { r.ObservedAt = now; r.Background = -1 }},
		{"foreign epoch", func(r *activity.Report) { r.ObservedAt = now; r.Epoch = "other" }},
		{"foreign pod", func(r *activity.Report) { r.ObservedAt = now; r.PodUID = "old-pod" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := report
			bad.Sequence = 2
			tc.change(&bad)
			if w := post("inventory", token, bad); w.Code != 409 {
				t.Fatalf("bad inventory accepted: %d", w.Code)
			}
		})
	}
	var got kyberv1.Agent
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(agent), &got)
	if got.Status.Activity.Work.Sequence != 1 || !got.Status.Activity.Work.ReceivedAt.Equal(&baseline.Status.Activity.Work.ReceivedAt) {
		t.Fatal("rejected replay renewed observation")
	}
	// A concurrent legacy heartbeat must preserve admitted work.
	if err := s.applyStatusEvent(context.Background(), agent.Name, statusEvent{Type: "heartbeat", At: now.Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(agent), &got)
	if got.Status.Activity.Work.Sequence != 1 {
		t.Fatal("heartbeat cleared work")
	}
	// Native container replacement invalidates the old epoch even in the same pod.
	pod.Status.InitContainerStatuses[0].ContainerID = "containerd://second"
	pod.Status.InitContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(now)
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	report.Sequence = 2
	report.ObservedAt = now
	if w := post("inventory", token, report); w.Code != 409 {
		t.Fatalf("old container accepted: %d", w.Code)
	}
	if w := post("register", token, reg); w.Code != 409 {
		t.Fatalf("old boot accepted: %d", w.Code)
	}
	reg.BootID = "second-boot"
	reg.BootedAt = now
	if w := post("register", token, reg); w.Code != 200 {
		t.Fatalf("replacement registration: %d", w.Code)
	}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "test", Name: agent.Name}, &got)
	if got.Status.Activity.Work.Epoch == registered.Epoch || got.Status.Activity.Work.Complete {
		t.Fatal("replacement retained previous work lease")
	}
}
