package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func buildDiskHandler(t *testing.T, objs ...runtime.Object) (http.Handler, client.Client) {
	t.Helper()
	all := append([]runtime.Object{defaultMachine()}, objs...)
	fakeClient := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithRuntimeObjects(all...).WithStatusSubresource(&kyberv1.Agent{}).Build()
	s := &api.Server{K8sClient: fakeClient, APIKey: testAPIKey, Namespace: "kyber-system", ValidRuntimes: map[string]bool{"claude-code": true}}
	return s.BuildHandler(), fakeClient
}

func diskObjects(t *testing.T, driver string) (*kyberv1.Agent, *corev1.PersistentVolumeClaim, *corev1.PersistentVolume, *storagev1.StorageClass) {
	t.Helper()
	agent := sampleAgentCRD("dave")
	agent.UID = types.UID("agent-uid")
	agent.Spec.Resources.Disk = resource.MustParse("20Gi")
	agent.Status.Phase = kyberv1.AgentPhaseStopped
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseStopped
	className := "agent-disk"
	mode := corev1.PersistentVolumeFilesystem
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-dave-pv", Namespace: agent.Namespace, UID: types.UID("pvc-uid"),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "kyber.io/v1", Kind: "Agent", Name: agent.Name, UID: agent.UID}}},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-dave", StorageClassName: &className, VolumeMode: &mode,
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}}},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}},
	}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-dave", UID: types.UID("pv-uid")},
		Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{Name: pvc.Name, Namespace: pvc.Namespace, UID: pvc.UID},
			Capacity:               corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: "private-volume-id"}}}}
	allow := true
	class := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: className}, Provisioner: driver, AllowVolumeExpansion: &allow}
	return agent, pvc, pv, class
}

func TestAgentDiskAPI_PreflightAndIncreasePreservesVolume(t *testing.T) {
	agent, pvc, pv, class := diskObjects(t, "pd.csi.storage.gke.io")
	h, c := buildDiskHandler(t, agent, pvc, pv, class)
	options := httptest.NewRecorder()
	h.ServeHTTP(options, authedRequest(t, http.MethodGet, "/api/v1/agents/dave/disk-options", nil))
	if options.Code != http.StatusOK || !strings.Contains(options.Body.String(), `"supported":true`) || strings.Contains(options.Body.String(), "private-volume-id") {
		t.Fatalf("disk options: %d %s", options.Code, options.Body.String())
	}
	for _, target := range []any{20, 10, 30.5} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, authedRequest(t, http.MethodPost, "/api/v1/agents/dave/increase-disk", map[string]any{"sizeGiB": target}))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("target %v: expected 400, got %d: %s", target, rr.Code, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedRequest(t, http.MethodPost, "/api/v1/agents/dave/increase-disk", map[string]any{"sizeGiB": 50}))
	if rr.Code != http.StatusAccepted || !strings.Contains(rr.Body.String(), `"stage":"Requested"`) || strings.Contains(rr.Body.String(), "pvc-uid") {
		t.Fatalf("increase: %d %s", rr.Code, rr.Body.String())
	}
	stored := &kyberv1.Agent{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Resources.Disk.Cmp(resource.MustParse("50Gi")) != 0 || stored.Spec.DesiredPhase != kyberv1.AgentPhaseStopped {
		t.Fatalf("incorrect Agent state after request: disk=%s phase=%s", stored.Spec.Resources.Disk.String(), stored.Spec.DesiredPhase)
	}
	claim := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: pvc.Name, Namespace: pvc.Namespace}, claim); err != nil {
		t.Fatal(err)
	}
	if claim.UID != pvc.UID || claim.Spec.VolumeName != pv.Name {
		t.Fatal("API request replaced the original persistent volume")
	}
}

func TestAgentDiskAPI_UnsupportedDriver(t *testing.T) {
	agent, pvc, pv, class := diskObjects(t, "rancher.io/local-path")
	h, _ := buildDiskHandler(t, agent, pvc, pv, class)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedRequest(t, http.MethodPost, "/api/v1/agents/dave/increase-disk", map[string]any{"sizeGiB": 50}))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "not verified") {
		t.Fatalf("unsupported driver: %d %s", rr.Code, rr.Body.String())
	}
}

func TestAgentDiskAPI_RejectsUnownedPVC(t *testing.T) {
	agent, pvc, pv, class := diskObjects(t, "pd.csi.storage.gke.io")
	pvc.OwnerReferences = nil
	h, _ := buildDiskHandler(t, agent, pvc, pv, class)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedRequest(t, http.MethodPost, "/api/v1/agents/dave/increase-disk", map[string]any{"sizeGiB": 50}))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "ownership") {
		t.Fatalf("unowned PVC must be rejected: %d %s", rr.Code, rr.Body.String())
	}
}

func TestAgentDiskAPI_BlockedUnfulfilledTargetCanBeReducedWithoutShrinkingVolume(t *testing.T) {
	agent, pvc, pv, class := diskObjects(t, "pd.csi.storage.gke.io")
	agent.Spec.Resources.Disk = resource.MustParse("100Gi")
	agent.Status.DiskExpansion = &kyberv1.AgentDiskExpansionStatus{
		TargetGiB: 100, PVCUID: pvc.UID, PVUID: pv.UID, Stage: "Blocked",
	}
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("100Gi")
	h, c := buildDiskHandler(t, agent, pvc, pv, class)

	options := httptest.NewRecorder()
	h.ServeHTTP(options, authedRequest(t, http.MethodGet, "/api/v1/agents/dave/disk-options", nil))
	if options.Code != http.StatusOK || !strings.Contains(options.Body.String(), `"minimumTargetGiB":21`) {
		t.Fatalf("blocked retry options: %d %s", options.Code, options.Body.String())
	}
	for _, target := range []int{20, 50} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, authedRequest(t, http.MethodPost, "/api/v1/agents/dave/increase-disk", map[string]any{"sizeGiB": target}))
		if target == 20 && rr.Code != http.StatusBadRequest {
			t.Fatalf("physical shrink should be rejected: %d %s", rr.Code, rr.Body.String())
		}
		if target == 50 && rr.Code != http.StatusAccepted {
			t.Fatalf("unfulfilled target recovery should be accepted: %d %s", rr.Code, rr.Body.String())
		}
	}
	stored := &kyberv1.Agent{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Resources.Disk.Cmp(resource.MustParse("50Gi")) != 0 || stored.Status.DiskExpansion.TargetGiB != 50 {
		t.Fatalf("retry target was not recorded: %+v", stored.Status.DiskExpansion)
	}
	claim := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: pvc.Name, Namespace: pvc.Namespace}, claim); err != nil {
		t.Fatal(err)
	}
	if claim.UID != pvc.UID || claim.Spec.VolumeName != pv.Name {
		t.Fatal("blocked retry replaced the original PVC")
	}
}
