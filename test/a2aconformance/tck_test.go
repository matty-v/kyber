//go:build integration

package a2aconformance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/capabilities"
	"github.com/matty-v/kyber/pkg/taskstore"
)

const tckBearer = "a2a-conformance-only-key"

type completingEventStore struct{ *taskstore.MemoryStore }

func (s *completingEventStore) Create(ctx context.Context, params taskstore.CreateParams) (*taskstore.CreateResult, error) {
	result, err := s.MemoryStore.Create(ctx, params)
	if err == nil && !result.Replay {
		go func(id string) {
			time.Sleep(25 * time.Millisecond)
			_ = s.MarkDispatched(context.Background(), params.Agent, id, 1)
			_ = s.Complete(context.Background(), params.Agent, id, 2, "Conformance fixture completed the task.")
		}(result.Task.ID)
	}
	return result, err
}

func (s *completingEventStore) EventSnapshot(ctx context.Context, agent taskstore.AgentRef, id string, auth taskstore.AuthorizationContext) (*taskstore.Task, int64, error) {
	task, err := s.GetAuthorized(ctx, agent, id, auth)
	return task, 0, err
}
func (*completingEventStore) EventHighWater(context.Context, taskstore.AgentRef, string, taskstore.AuthorizationContext) (int64, error) {
	return 0, nil
}
func (*completingEventStore) ReadEvents(context.Context, taskstore.EventReadParams) (*taskstore.EventPage, error) {
	return &taskstore.EventPage{Terminal: true}, nil
}

func TestPinnedOfficialHTTPJSONTCK(t *testing.T) {
	tck := os.Getenv("KYBER_A2A_TCK_DIR")
	if tck == "" {
		t.Skip("KYBER_A2A_TCK_DIR is not set")
	}
	store, err := taskstore.NewMemoryStore(taskstore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	manifest := &kyberv1.AgentPublicCapabilities{
		SchemaVersion: capabilities.SchemaV1Alpha1,
		Identity:      kyberv1.AgentPublicCapabilityIdentity{DisplayName: "Conformance Agent", Description: "Deterministic A2A conformance fixture."},
		Capabilities:  []kyberv1.AgentPublicCapability{{ID: "echo", Version: "1", Name: "Echo", Description: "Returns deterministic output.", InputModes: []string{"text/plain"}, OutputModes: []string{"text/plain"}}},
	}
	_, digest, err := capabilities.NormalizeAndValidate(manifest)
	if err != nil {
		t.Fatal(err)
	}
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "conformance", Namespace: "kyber-system", Generation: 1}}
	agent.Spec.RequestReplyEnabled = true
	agent.Spec.PublicCapabilities = manifest
	agent.Status.PublicCapabilities = &kyberv1.AgentPublicCapabilitiesStatus{ObservedGeneration: 1, ManifestRevision: digest, Conditions: []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue}}, Capabilities: []kyberv1.AgentPublicCapabilityAvailability{{ID: "echo", Availability: "available"}}}
	scheme := runtime.NewScheme()
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	server := &api.Server{K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build(), Namespace: "kyber-system", TaskStore: &completingEventStore{MemoryStore: store}, TasksEnabled: true, A2AEnabled: true, Callers: []api.ScopedCaller{{Name: "tck", PrincipalID: "tck", TenantID: "conformance", AgentResources: []string{"kyber-system/conformance"}, Key: tckBearer, Scopes: []string{"tasks:create", "tasks:read", "tasks:list", "tasks:continue", "tasks:cancel", "task-results:read", "task-events:read"}}}}
	var target http.Handler
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The pinned TCK has no Bearer-header flag. This loopback-only shim is the
		// complete, auditable authentication adaptation; TCK source stays pristine.
		r.Header.Set("Authorization", "Bearer "+tckBearer)
		target.ServeHTTP(w, r)
	}))
	defer fixture.Close()
	server.PublicURL = fixture.URL
	target = server.BuildHandler()

	cmd := exec.Command("uv", "run", "./run_tck.py", "--sut-host", fixture.URL+"/a2a/v1/agents/conformance", "--transport", "http_json")
	cmd.Dir = tck
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pinned TCK failed: %v\n%s", err, output)
	}
	for _, name := range []string{"compatibility.json", "compatibility.html", "junitreport.xml"} {
		if _, err := os.Stat(filepath.Join(tck, "reports", name)); err != nil {
			t.Fatalf("TCK report %s: %v", name, err)
		}
	}
}
