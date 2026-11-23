package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/activity"
	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/podtoken"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestActivityObserverSchemaAndLifetime(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = kyberv1.AddToScheme(scheme)
	env := &envtest.Environment{Scheme: scheme, BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"), CRDDirectoryPaths: []string{filepath.Join("..", "..", "deploy", "helm", "kyber", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	}()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "activity-test"}}); err != nil {
		t.Fatal(err)
	}
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "activity-test"}, Spec: kyberv1.AgentSpec{Machine: "worker", Runtime: "codex", Resources: kyberv1.AgentResources{CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"), Disk: resource.MustParse("5Gi")}}}
	agent.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeAPIKey
	if err := c.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	agent.Status.PodName = "agent-fixture"
	if err := c.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	controller := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: agent.Status.PodName, Namespace: agent.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: "kyber.io/v1", Kind: "Agent", Name: agent.Name, UID: agent.UID, Controller: &controller}}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "fixture"}}, InitContainers: []corev1.Container{{Name: "kyber-status-sidecar", Image: "fixture"}}}}
	createPod := func() {
		t.Helper()
		if err := c.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "kyber-status-sidecar", Image: "fixture", ImageID: "fixture", ContainerID: "containerd://fixture", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Minute))}}}}
		if err := c.Status().Update(ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	createPod()
	key := []byte("test-key")
	server := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(c, agent.Namespace), api.WithUsageAgentReader(c), api.WithInternalAuth(api.NewHMACInternalAuthenticator(key), false))
	credential := podtoken.Sign(activity.ObserverIdentity(agent.Name, string(agent.UID)), key)
	post := func(path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		data, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/internal/activity-observers/fixture/"+path, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+credential)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	registration := activity.Registration{PodUID: string(pod.UID), BootID: "native-life", BootedAt: time.Now().Add(-time.Second)}
	w := post("register", registration)
	if w.Code != 200 {
		t.Fatalf("registration: %d %s", w.Code, w.Body.String())
	}
	var admitted activity.RegistrationResponse
	_ = json.Unmarshal(w.Body.Bytes(), &admitted)
	report := activity.Report{Registration: registration, Epoch: admitted.Epoch, Sequence: 1, Snapshot: activity.Snapshot{ObservedAt: time.Now(), Complete: true, Background: 1}}
	if w := post("inventory", report); w.Code != 204 {
		t.Fatalf("inventory: %d %s", w.Code, w.Body.String())
	}
	var saved kyberv1.Agent
	if err := c.Get(ctx, client.ObjectKeyFromObject(agent), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Status.Activity == nil || saved.Status.Activity.Work == nil || saved.Status.Activity.Work.Background != 1 {
		t.Fatal("CRD pruned admitted work")
	}
	stale := saved.DeepCopy()
	report.Sequence = 2
	report.ObservedAt = time.Now()
	report.Background = 0
	if w := post("inventory", report); w.Code != 204 {
		t.Fatalf("completion: %d %s", w.Code, w.Body.String())
	}
	before := stale.DeepCopy()
	stale.Status.Activity.Work.Sequence = 99
	if err := c.Status().Patch(ctx, stale, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); !apierrors.IsConflict(err) {
		t.Fatalf("stale status write did not conflict: %v", err)
	}
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	report.Sequence = 3
	report.ObservedAt = time.Now()
	if w := post("inventory", report); w.Code == 204 {
		t.Fatal("deleted pod renewed inventory")
	}
}
