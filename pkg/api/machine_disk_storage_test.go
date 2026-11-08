package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matty-v/kyber/internal/testutil"
	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
	_ "github.com/matty-v/kyber/pkg/runtimes/claudecode"
	_ "github.com/matty-v/kyber/pkg/runtimes/codex"
	_ "github.com/matty-v/kyber/pkg/runtimes/hermes"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Old controller capacity may still be cached during an upgrade. Both machine
// endpoints must suppress that false constraint, including before a Node exists.
func TestMachineExternalDiskCapacity(t *testing.T) {
	for _, driver := range []string{"pd.csi.storage.gke.io", "ebs.csi.aws.com", "rancher.io/local-path"} {
		t.Run(driver, func(t *testing.T) {
			capacity := kyberv1.MachineCapacity{CPU: resource.MustParse("4"), Memory: resource.MustParse("16Gi"), EphemeralStorage: resource.MustParse("150Gi")}
			machine := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "coders", Namespace: "kyber-system"},
				Spec:   kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGKE, Capacity: capacity},
				Status: kyberv1.MachineStatus{ObservedCapacity: &capacity, AssignableCapacity: &capacity, AvailableCapacity: &capacity}}
			class := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "agents"}, Provisioner: driver}
			c := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(machine, class).Build()
			s := &api.Server{K8sClient: c, Namespace: "kyber-system", APIKey: testAPIKey,
				SecurityAudit: testutil.NewSecurityAudit(), AgentStorageClass: class.Name}
			h := s.BuildHandler()
			for _, path := range []string{"/api/v1/machines/coders", "/api/v1/machines"} {
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, authedRequest(t, http.MethodGet, path, nil))
				if rr.Code != http.StatusOK {
					t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
				}
				var got api.MachineResponse
				if path == "/api/v1/machines" {
					var list api.MachineListResponse
					if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
						t.Fatalf("list response: %s, %v", rr.Body.String(), err)
					}
					got = list.Items[0]
				} else if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				external := driver != "rancher.io/local-path"
				if (got.AgentDiskStorage == "external") != external {
					t.Fatalf("agentDiskStorage = %q", got.AgentDiskStorage)
				}
				if (got.Status.AssignableCapacity.EphemeralStorage == "") != external ||
					(got.Status.AvailableCapacity.EphemeralStorage == "") != external ||
					(got.Spec.Capacity.EphemeralStorage == "") != external {
					t.Fatalf("incorrect disk budget: %+v", got)
				}
				if got.Status.ObservedCapacity.EphemeralStorage != "150Gi" || got.Status.AvailableCapacity.CPU != "4" {
					t.Fatalf("physical node observation or CPU changed: %+v", got)
				}
			}
		})
	}
}

// Enumerate the registry: machine accounting must never remove Agent measurements,
// including for future harnesses using this shared API projection.
func TestExternalStoragePreservesAgentDiskUsage(t *testing.T) {
	for _, descriptor := range runtimes.Descriptors() {
		t.Run(descriptor.ID, func(t *testing.T) {
			agent := sampleAgentCRD("disk-agent")
			agent.Spec.Runtime = descriptor.ID
			agent.Spec.Resources.Disk = resource.MustParse("50Gi")
			agent.Status.Activity = &kyberv1.ActivityStatus{Resources: &kyberv1.AgentResourceUsage{
				SampledAt: metav1.Now(), DiskUsedBytes: 40 << 30, DiskTotalBytes: 50 << 30,
				DiskLimitEnforced: true, DiskReserveReached: true, DiskUsageMethod: "statfs", DiskUsageState: "ready",
				DiskBackingTotalBytes: 50 << 30, DiskBackingAvailableBytes: 10 << 30,
			}}
			class := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "cloud"}, Provisioner: "pd.csi.storage.gke.io"}
			c := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent, class).Build()
			s := &api.Server{K8sClient: c, Namespace: "kyber-system", APIKey: testAPIKey,
				AgentStorageClass: class.Name, SecurityAudit: testutil.NewSecurityAudit()}
			rr := httptest.NewRecorder()
			s.BuildHandler().ServeHTTP(rr, authedRequest(t, http.MethodGet, "/api/v1/agents/disk-agent", nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("GET agent: %d %s", rr.Code, rr.Body.String())
			}
			var got struct {
				Resources struct {
					Disk string `json:"disk"`
				} `json:"resources"`
				Activity struct {
					Resources struct {
						Used         int64  `json:"diskUsedBytes"`
						Total        int64  `json:"diskTotalBytes"`
						Enforced     bool   `json:"diskLimitEnforced"`
						Reserve      bool   `json:"diskReserveReached"`
						Method       string `json:"diskUsageMethod"`
						BackingTotal int64  `json:"diskBackingTotalBytes"`
						BackingFree  int64  `json:"diskBackingAvailableBytes"`
					} `json:"resources"`
				} `json:"activity"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			usage := got.Activity.Resources
			if got.Resources.Disk != "50Gi" || usage.Used != 40<<30 || usage.Total != 50<<30 || !usage.Enforced || !usage.Reserve || usage.Method != "statfs" || usage.BackingTotal != 50<<30 || usage.BackingFree != 10<<30 {
				t.Fatalf("Agent disk usage changed: %+v", got)
			}
		})
	}
}
