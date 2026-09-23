//go:build integration

package contract

import (
	"encoding/json"
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/archivestore"
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
