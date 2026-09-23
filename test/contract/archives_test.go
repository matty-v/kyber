//go:build integration

package contract

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/archivestore"
	"github.com/matty-v/kyber/pkg/diskarchive"
)

// TestContract_AgentExports validates the disk export routes (MAT-87) and
// the config capability against openapi.yaml.
func TestContract_AgentExports(t *testing.T) {
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "exporter", Namespace: contractNS},
		Spec: kyberv1.AgentSpec{
			Machine: "worker-1", Runtime: "claude-code", DesiredPhase: kyberv1.AgentPhaseRunning,
			Resources: kyberv1.AgentResources{CPU: resource.MustParse("1"), Memory: resource.MustParse("2Gi"), Disk: resource.MustParse("50Gi")},
		},
		Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning},
	}
	c := fake.NewClientBuilder().WithScheme(newContractScheme(t)).WithObjects(agent).Build()
	store, err := archivestore.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := &api.Server{K8sClient: c, APIKey: contractAPIKey, Namespace: contractNS, Archives: &api.ArchiveService{
		Client: c, Namespace: contractNS, Store: store, Jobs: archivejob.NewMemoryStore(),
		SigningKey: []byte("k"), ToolImage: "kyber/control-plane:test",
	}}
	h := srv.BuildHandler()

	rr := validateContract(t, h, authedReq(t, http.MethodPost, "/api/v1/agents/exporter/exports", nil))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", rr.Code, rr.Body.String())
	}
	var job struct{ ID string }
	_ = json.Unmarshal(rr.Body.Bytes(), &job)
	validateContract(t, h, authedReq(t, http.MethodGet, "/api/v1/agents/exporter/exports", nil))
	validateContract(t, h, authedReq(t, http.MethodGet, "/api/v1/agents/exporter/exports/"+job.ID, nil))
	validateContract(t, h, authedReq(t, http.MethodPost, "/api/v1/agents/exporter/exports/"+job.ID+"/cancel", nil))
	validateContract(t, h, authedReq(t, http.MethodGet, "/api/v1/config", nil))
}

// TestContract_AgentImports validates create-from-archive (MAT-88) against
// openapi.yaml, from a completed export seeded directly into the job store.
func TestContract_AgentImports(t *testing.T) {
	machine := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: contractNS}}
	machine.Spec.Capacity = kyberv1.MachineCapacity{CPU: resource.MustParse("8"), Memory: resource.MustParse("32Gi")}
	c := fake.NewClientBuilder().WithScheme(newContractScheme(t)).WithObjects(machine).Build()
	store, err := archivestore.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobs := archivejob.NewMemoryStore()
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	export := &archivejob.Job{
		ID: "export1", Kind: archivejob.KindExport, Agent: "source", State: archivejob.StateCompleted,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires,
		Summary: &archivejob.Summary{FormatVersion: diskarchive.FormatVersion, ExportedAt: now,
			Source: diskarchive.Source{Agent: "source", Runtime: "claude-code"},
			Totals: diskarchive.Totals{Entries: 1, Dirs: 1}},
	}
	if err := jobs.Create(context.Background(), export, 0); err != nil {
		t.Fatal(err)
	}
	srv := &api.Server{K8sClient: c, APIKey: contractAPIKey, Namespace: contractNS, Archives: &api.ArchiveService{
		Client: c, Namespace: contractNS, Store: store, Jobs: jobs, SigningKey: []byte("k"), ToolImage: "kyber/control-plane:test",
	}}
	h := srv.BuildHandler()

	validateContract(t, h, authedReq(t, http.MethodGet, "/api/v1/archives", nil))
	rr := validateContract(t, h, authedReq(t, http.MethodPost, "/api/v1/agent-imports", map[string]any{
		"source": map[string]string{"exportId": "export1"},
		"agent": map[string]any{
			"name": "restored", "machine": "worker-1", "runtime": "claude-code",
			"resources": map[string]string{"cpu": "1", "memory": "2Gi", "disk": "20Gi"},
			"secrets":   map[string]string{"authType": "oauth"},
		},
	}))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("create from archive = %d %s", rr.Code, rr.Body.String())
	}
	var created struct {
		Import struct{ ID string }
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	validateContract(t, h, authedReq(t, http.MethodGet, "/api/v1/agent-imports?agent=restored", nil))
	validateContract(t, h, authedReq(t, http.MethodGet, "/api/v1/agent-imports/"+created.Import.ID, nil))
	validateContract(t, h, authedReq(t, http.MethodPost, "/api/v1/agent-imports/"+created.Import.ID+"/cancel", nil))
}
