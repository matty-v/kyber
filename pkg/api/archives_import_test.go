package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/archivestore"
	"github.com/matty-v/kyber/pkg/diskarchive"
)

const restoredAgent = "restored"

// completedExport drives an export to completion and returns its ID and the
// ZIP the pod uploaded.
func (h *exportHarness) completedExport() (string, []byte) {
	h.t.Helper()
	id := h.runToArchiving()
	data := sampleDisk(h.t)
	if rr := h.upload(id, h.podToken(id), bytes.NewReader(data)); rr.Code != http.StatusNoContent {
		h.t.Fatalf("upload = %d", rr.Code)
	}
	h.setPodPhase(id, corev1.PodSucceeded)
	h.svc.Tick(context.Background()) // release
	h.svc.Tick(context.Background()) // verification pod
	h.playVerifyPod(id)
	h.svc.Tick(context.Background())
	if j := h.job(id); j.State != archivejob.StateCompleted {
		h.t.Fatalf("export state = %s (%s)", j.State, j.Error)
	}
	return id, data
}

func (h *exportHarness) post(path, key string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.server.BuildHandler().ServeHTTP(rr, req)
	return rr
}

func importBody(source map[string]string, name, disk string) map[string]any {
	return map[string]any{
		"source": source,
		"agent": map[string]any{
			"name": name, "machine": "worker-1", "runtime": "claude-code",
			"resources": map[string]string{"cpu": "1", "memory": "2Gi", "disk": disk},
			"secrets":   map[string]string{"authType": "oauth"},
		},
	}
}

func (h *exportHarness) startImport(source map[string]string) string {
	h.t.Helper()
	rr := h.post("/api/v1/agent-imports", testAPIKey, importBody(source, restoredAgent, "20Gi"))
	if rr.Code != http.StatusAccepted {
		h.t.Fatalf("start import = %d %s", rr.Code, rr.Body.String())
	}
	var v struct {
		Import struct{ ID string }
	}
	json.Unmarshal(rr.Body.Bytes(), &v)
	return v.Import.ID
}

func (h *exportHarness) newAgent() (*kyberv1.Agent, error) {
	a := &kyberv1.Agent{}
	err := h.c.Get(context.Background(), types.NamespacedName{Name: restoredAgent, Namespace: "kyber-system"}, a)
	return a, err
}

func (h *exportHarness) restorePod(id string) *corev1.Pod {
	pod := &corev1.Pod{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: "disk-import-" + id, Namespace: "kyber-system"}, pod); err != nil {
		h.t.Fatalf("restore pod: %v", err)
	}
	return pod
}

func (h *exportHarness) setRestorePhase(id string, phase corev1.PodPhase) {
	pod := h.restorePod(id)
	pod.Status.Phase = phase
	if phase == corev1.PodFailed {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "archive", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "restoring: destination is not empty"}}}}
	}
	if err := h.c.Status().Update(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
}

// readRestoreSource plays the restore pod's ranged reads.
func (h *exportHarness) readRestoreSource(id, token string) []byte {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodHead, "/internal/archive-jobs/"+id+"/archive", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.internal.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		h.t.Fatalf("HEAD source = %d", rr.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/internal/archive-jobs/"+id+"/archive", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Range", "bytes=0-")
	rr = httptest.NewRecorder()
	h.internal.ServeHTTP(rr, req)
	if rr.Code != http.StatusPartialContent {
		h.t.Fatalf("GET source = %d", rr.Code)
	}
	return rr.Body.Bytes()
}

func (h *exportHarness) setNewAgentPhase(phase kyberv1.AgentPhase) {
	a, err := h.newAgent()
	if err != nil {
		h.t.Fatal(err)
	}
	a.Status.Phase = phase
	if err := h.c.Update(context.Background(), a); err != nil {
		h.t.Fatal(err)
	}
}

func TestImportFromExportEndToEnd(t *testing.T) {
	h := newExportHarness(t)
	// A retention shorter than the job timeout lets the test show that a
	// source outliving its retention survives while an import reads it.
	h.svc.Limits.Retention = time.Hour
	ctx := context.Background()
	exportID, data := h.completedExport()
	id := h.startImport(map[string]string{"exportId": exportID})

	// Created through the normal create path, held from birth.
	a, err := h.newAgent()
	if err != nil {
		t.Fatalf("new agent not created: %v", err)
	}
	if a.Annotations[kyberv1.AnnotationArchiveHold] != id {
		t.Fatalf("new agent hold = %q, want %q", a.Annotations[kyberv1.AnnotationArchiveHold], id)
	}
	j := h.job(id)
	if len(j.Skip) != 1 || !strings.HasSuffix(j.Skip[0], ".claude/.credentials.json") {
		t.Errorf("skip = %v, want the Claude credentials file", j.Skip)
	}

	h.svc.Tick(ctx)
	pvc := &corev1.PersistentVolumeClaim{}
	if err := h.c.Get(ctx, types.NamespacedName{Name: "agent-" + restoredAgent + "-pv", Namespace: "kyber-system"}, pvc); err != nil {
		t.Fatalf("destination volume: %v", err)
	}
	if len(pvc.OwnerReferences) != 1 || pvc.OwnerReferences[0].UID != a.UID {
		t.Errorf("destination volume owner = %+v", pvc.OwnerReferences)
	}
	pod := h.restorePod(id)
	if pinnedNode(pod) != "node-a" || pod.Spec.NodeName != "" || pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "agent-"+restoredAgent+"-pv" ||
		pod.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly {
		t.Errorf("restore pod volume/placement = %+v", pod.Spec)
	}
	if got := pod.Spec.Containers[0].Command; got[1] != "restore" {
		t.Errorf("restore pod command = %v", got)
	}
	if env := pod.Spec.Containers[0].Env; envValue(env, "KYBER_ARCHIVE_SKIP_CREDENTIALS") != "true" || envValue(env, "KYBER_ARCHIVE_SKIP_CRONTABS") != "true" {
		t.Errorf("restore pod must leave credentials and crontabs out by default: %+v", env)
	}

	// The restore pod reads exactly the exported archive, and only with its token.
	token := h.podTokenFor("disk-import-" + id)
	if got := h.readRestoreSource(id, token); !bytes.Equal(got, data) {
		t.Fatalf("restore source differs from the export (%d vs %d bytes)", len(got), len(data))
	}
	req := httptest.NewRequest(http.MethodGet, "/internal/archive-jobs/"+exportID+"/archive", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.internal.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("import token read the export job directly: %d", rr.Code)
	}

	// Retention does not delete a source an import is still reading.
	h.now = h.now.Add(90 * time.Minute)
	h.svc.Tick(ctx)
	if src := h.job(exportID); src.State != archivejob.StateCompleted {
		t.Fatalf("source expired during an active import: %s", src.State)
	}

	h.setRestorePhase(id, corev1.PodSucceeded)
	h.svc.Tick(ctx) // records the verified restore
	h.svc.Tick(ctx) // releases the hold
	a, _ = h.newAgent()
	if a.Annotations[kyberv1.AnnotationArchiveHold] != "" {
		t.Fatal("hold not released after a verified restore")
	}
	if j := h.job(id); j.Step != archivejob.StepStarting || !j.Restored {
		t.Fatalf("after restore: step=%s restored=%v", j.Step, j.Restored)
	}
	h.setNewAgentPhase(kyberv1.AgentPhaseNeedsAuth)
	h.svc.Tick(ctx)
	j = h.job(id)
	if j.State != archivejob.StateCompleted {
		t.Fatalf("import state = %s (%s)", j.State, j.Error)
	}
	// The source agent was never touched.
	src := h.agent()
	if src.Annotations[kyberv1.AnnotationArchiveHold] != "" || src.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Errorf("source agent changed: %+v", src.Annotations)
	}

	rr = h.do(http.MethodGet, "/api/v1/agent-imports/"+id, testAPIKey)
	var v struct{ Cutover []string }
	json.Unmarshal(rr.Body.Bytes(), &v)
	if rr.Code != http.StatusOK || len(v.Cutover) == 0 || !strings.Contains(strings.Join(v.Cutover, " "), "look like credentials were not restored") {
		t.Errorf("import view = %d %s", rr.Code, rr.Body.String())
	}
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func (h *exportHarness) podTokenFor(pod string) string {
	s := &corev1.Secret{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: pod + "-token", Namespace: "kyber-system"}, s); err != nil {
		h.t.Fatalf("token secret: %v", err)
	}
	return s.StringData["token"]
}

func TestImportFailureAndCancelDiscardTheNewAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(h *exportHarness, id string)
	}{
		{"restore pod fails", func(h *exportHarness, id string) { h.setRestorePhase(id, corev1.PodFailed) }},
		{"operator cancels", func(h *exportHarness, id string) {
			if rr := h.post("/api/v1/agent-imports/"+id+"/cancel", testAPIKey, nil); rr.Code != http.StatusAccepted {
				h.t.Fatalf("cancel = %d %s", rr.Code, rr.Body.String())
			}
		}},
		{"deadline", func(h *exportHarness, id string) { h.now = h.now.Add(3 * time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newExportHarness(t)
			exportID, _ := h.completedExport()
			id := h.startImport(map[string]string{"exportId": exportID})
			h.svc.Tick(context.Background())
			tc.act(h, id)
			h.svc.Tick(context.Background())
			if j := h.job(id); !j.State.Terminal() || j.State == archivejob.StateCompleted {
				t.Fatalf("state = %s", j.State)
			}
			if _, err := h.newAgent(); !k8serrors.IsNotFound(err) {
				t.Errorf("partially restored agent kept: %v", err)
			}
			pod := &corev1.Pod{}
			if err := h.c.Get(context.Background(), types.NamespacedName{Name: "disk-import-" + id, Namespace: "kyber-system"}, pod); !k8serrors.IsNotFound(err) {
				t.Errorf("restore pod kept: %v", err)
			}
			// The source export is still intact and importable again.
			if j := h.job(exportID); j.State != archivejob.StateCompleted {
				t.Errorf("source export state = %s", j.State)
			}
			h.startImport(map[string]string{"exportId": exportID})
		})
	}
}

func TestImportBootFailureKeepsRestoredAgent(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()
	id := h.startImport(map[string]string{"exportId": exportID})
	h.svc.Tick(context.Background())
	h.setRestorePhase(id, corev1.PodSucceeded)
	h.svc.Tick(context.Background())
	h.svc.Tick(context.Background())
	h.setNewAgentPhase(kyberv1.AgentPhaseFailed)
	h.svc.Tick(context.Background())
	if j := h.job(id); j.State != archivejob.StateFailed || !strings.Contains(j.Error, "kept for inspection") {
		t.Fatalf("state=%s error=%q", j.State, j.Error)
	}
	if _, err := h.newAgent(); err != nil {
		t.Errorf("a fully restored agent was deleted after a boot failure: %v", err)
	}
	if rr := h.post("/api/v1/agent-imports/"+id+"/cancel", testAPIKey, nil); rr.Code != http.StatusConflict {
		t.Errorf("cancel after restore = %d, want 409", rr.Code)
	}
}

func TestImportRejectsBeforeCreatingAnything(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()

	// Too small a disk.
	if rr := h.post("/api/v1/agent-imports", testAPIKey, importBody(map[string]string{"exportId": exportID}, restoredAgent, "100Mi")); rr.Code != http.StatusBadRequest ||
		!strings.Contains(rr.Body.String(), "insufficient_capacity") {
		t.Errorf("small disk = %d %s", rr.Code, rr.Body.String())
	}
	// Existing agent name: the create path's own 409 is relayed.
	if rr := h.post("/api/v1/agent-imports", testAPIKey, importBody(map[string]string{"exportId": exportID}, exportAgent, "20Gi")); rr.Code != http.StatusConflict {
		t.Errorf("existing name = %d %s", rr.Code, rr.Body.String())
	}
	// A leftover volume is never reused.
	leftover := &corev1.PersistentVolumeClaim{}
	leftover.Name, leftover.Namespace = "agent-orphan-pv", "kyber-system"
	if err := h.c.Create(context.Background(), leftover); err != nil {
		t.Fatal(err)
	}
	if rr := h.post("/api/v1/agent-imports", testAPIKey, importBody(map[string]string{"exportId": exportID}, "orphan", "20Gi")); rr.Code != http.StatusConflict {
		t.Errorf("existing PVC = %d %s", rr.Code, rr.Body.String())
	}
	// Unknown and ambiguous sources.
	if rr := h.post("/api/v1/agent-imports", testAPIKey, importBody(map[string]string{"exportId": "nope"}, restoredAgent, "20Gi")); rr.Code != http.StatusBadRequest {
		t.Errorf("unknown source = %d", rr.Code)
	}
	if rr := h.post("/api/v1/agent-imports", testAPIKey, importBody(map[string]string{"exportId": exportID, "uploadId": "x"}, restoredAgent, "20Gi")); rr.Code != http.StatusBadRequest {
		t.Errorf("ambiguous source = %d", rr.Code)
	}
	// An expired source.
	h.now = h.now.Add(73 * time.Hour)
	if rr := h.post("/api/v1/agent-imports", testAPIKey, importBody(map[string]string{"exportId": exportID}, restoredAgent, "20Gi")); rr.Code != http.StatusBadRequest {
		t.Errorf("expired source = %d", rr.Code)
	}
	if _, err := h.newAgent(); !k8serrors.IsNotFound(err) {
		t.Errorf("a rejected import created the agent: %v", err)
	}
}

func TestImportRequiresAccessToSourceAndDestination(t *testing.T) {
	h := newExportHarness(t,
		api.ScopedCaller{Name: "dest-only", Key: "dest-key", Scopes: []string{"archives:admin"}, AgentResources: []string{"kyber-system/" + restoredAgent}},
	)
	exportID, _ := h.completedExport()
	if rr := h.post("/api/v1/agent-imports", "dest-key", importBody(map[string]string{"exportId": exportID}, restoredAgent, "20Gi")); rr.Code != http.StatusNotFound {
		t.Errorf("caller without access to the source agent = %d, want 404", rr.Code)
	}
	if rr := h.post("/api/v1/archive-uploads", "dest-key", nil); rr.Code != http.StatusForbidden {
		t.Errorf("scoped caller upload = %d, want 403", rr.Code)
	}
}

func TestUploadThenImport(t *testing.T) {
	h := newExportHarness(t)
	ctx := context.Background()
	data := sampleDisk(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/archive-uploads", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	h.server.BuildHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("upload = %d %s", rr.Code, rr.Body.String())
	}
	var up struct{ ID string }
	json.Unmarshal(rr.Body.Bytes(), &up)
	h.svc.Tick(ctx) // verification pod
	h.playVerifyPod(up.ID)
	h.svc.Tick(ctx)
	if j := h.job(up.ID); j.State != archivejob.StateCompleted || j.Summary == nil {
		t.Fatalf("upload state = %s (%s)", j.State, j.Error)
	}
	// The stored upload is sealed.
	raw, _ := h.store.Open(ctx, "uploads/"+up.ID+"/archive.sealed", 0, -1)
	stored, _ := io.ReadAll(raw)
	raw.Close()
	if bytes.Contains(stored, []byte("SKILL.md")) {
		t.Error("stored upload is readable without the data key")
	}
	rr = h.do(http.MethodGet, "/api/v1/archives", testAPIKey)
	if !strings.Contains(rr.Body.String(), up.ID) {
		t.Errorf("archives list misses the upload: %s", rr.Body.String())
	}

	id := h.startImport(map[string]string{"uploadId": up.ID})
	h.svc.Tick(ctx)
	if got := h.readRestoreSource(id, h.podTokenFor("disk-import-"+id)); !bytes.Equal(got, data) {
		t.Fatal("restore source differs from the upload")
	}
}

func TestUploadOfGarbageFailsVerification(t *testing.T) {
	h := newExportHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/archive-uploads", strings.NewReader("definitely not a zip"))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	h.server.BuildHandler().ServeHTTP(rr, req)
	var up struct{ ID string }
	json.Unmarshal(rr.Body.Bytes(), &up)
	h.svc.Tick(context.Background())
	h.setVerifyPodPhase(up.ID, corev1.PodFailed, "diskarchive: archive is invalid: zip: not a valid zip file")
	h.svc.Tick(context.Background())
	j := h.job(up.ID)
	if j.State != archivejob.StateFailed || !strings.Contains(j.Error, "verification") {
		t.Fatalf("state=%s error=%q", j.State, j.Error)
	}
	if _, err := h.store.Size(context.Background(), "uploads/"+up.ID+"/archive.sealed"); err != archivestore.ErrNotFound {
		t.Errorf("failed upload left bytes: %v", err)
	}
}

func TestImportRejectsUnsupportedFormatVersion(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()
	j := h.job(exportID)
	j.Summary.FormatVersion = "kyber.io/agent-disk-archive/v9"
	if err := h.svc.Jobs.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	rr := h.post("/api/v1/agent-imports", testAPIKey, importBody(map[string]string{"exportId": exportID}, restoredAgent, "20Gi"))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("unsupported version = %d %s", rr.Code, rr.Body.String())
	}
	_ = diskarchive.FormatVersion
}

func apiKeyImportBody(source map[string]string, name string) map[string]any {
	b := importBody(source, name, "20Gi")
	b["agent"].(map[string]any)["secrets"] = map[string]string{"authType": "api-key", "anthropicApiKey": "sk-test"}
	return b
}

func (h *exportHarness) secretNames() []string {
	var list corev1.SecretList
	h.c.List(context.Background(), &list)
	var out []string
	for _, s := range list.Items {
		if strings.HasPrefix(s.Name, restoredAgent) {
			out = append(out, s.Name)
		}
	}
	return out
}

// Capacity is refused before anything is created.
func TestImportCapacityRefusedBeforeCreate(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()
	h.svc.Limits.MaxConcurrentJobs = 1
	h.start() // an active export fills the only slot
	rr := h.post("/api/v1/agent-imports", testAPIKey, apiKeyImportBody(map[string]string{"exportId": exportID}, restoredAgent))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("import at capacity = %d %s", rr.Code, rr.Body.String())
	}
	if _, err := h.newAgent(); !k8serrors.IsNotFound(err) {
		t.Errorf("agent created despite capacity refusal: %v", err)
	}
	if got := h.secretNames(); len(got) != 0 {
		t.Errorf("secrets created despite capacity refusal: %v", got)
	}
}

// A failed restore removes the Secrets the create made, so a retry with the
// same name works.
func TestImportFailureRemovesCreatedSecrets(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()
	rr := h.post("/api/v1/agent-imports", testAPIKey, apiKeyImportBody(map[string]string{"exportId": exportID}, restoredAgent))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("import = %d %s", rr.Code, rr.Body.String())
	}
	if len(h.secretNames()) == 0 {
		t.Skip("the api-key create path made no Secret in this configuration")
	}
	var v struct{ Import struct{ ID string } }
	json.Unmarshal(rr.Body.Bytes(), &v)
	h.svc.Tick(context.Background())
	h.setRestorePhase(v.Import.ID, corev1.PodFailed)
	h.svc.Tick(context.Background())
	if got := h.secretNames(); len(got) != 0 {
		t.Fatalf("secrets left after a failed restore: %v", got)
	}
	if rr := h.post("/api/v1/agent-imports", testAPIKey, apiKeyImportBody(map[string]string{"exportId": exportID}, restoredAgent)); rr.Code != http.StatusAccepted {
		t.Errorf("retry after failure = %d %s", rr.Code, rr.Body.String())
	}
}

// A create the normal handler rejects leaves a failed job and nothing else.
func TestImportCreateRejectionFailsTheJob(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()
	body := importBody(map[string]string{"exportId": exportID}, restoredAgent, "20Gi")
	body["agent"].(map[string]any)["machine"] = "no-such-machine"
	if rr := h.post("/api/v1/agent-imports", testAPIKey, body); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad machine = %d %s", rr.Code, rr.Body.String())
	}
	jobs, _ := h.svc.Jobs.List(context.Background(), archivejob.KindImport, restoredAgent, 5)
	if len(jobs) != 1 || jobs[0].State != archivejob.StateFailed {
		t.Fatalf("import jobs = %+v", jobs)
	}
	// The name is free for a corrected retry.
	if rr := h.post("/api/v1/agent-imports", testAPIKey, importBody(map[string]string{"exportId": exportID}, restoredAgent, "20Gi")); rr.Code != http.StatusAccepted {
		t.Errorf("corrected retry = %d %s", rr.Code, rr.Body.String())
	}
}

// If the control plane stopped between creating the agent and recording it,
// the worker finds the agent by its hold and carries on.
func TestImportAdoptsUnrecordedAgent(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()
	rr := h.post("/api/v1/agent-imports", testAPIKey, apiKeyImportBody(map[string]string{"exportId": exportID}, restoredAgent))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("import = %d %s", rr.Code, rr.Body.String())
	}
	var v struct{ Import struct{ ID string } }
	json.Unmarshal(rr.Body.Bytes(), &v)
	id := v.Import.ID
	created := h.job(id).CreatedSecrets
	j := h.job(id)
	j.AgentUID, j.CreatedSecrets = "", nil
	if err := h.svc.Jobs.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	h.svc.Tick(context.Background())
	a, _ := h.newAgent()
	if j := h.job(id); j.AgentUID != string(a.UID) || len(j.CreatedSecrets) != len(created) {
		t.Fatalf("worker did not adopt the held agent and its secrets: uid %q vs %q, secrets %v vs %v", j.AgentUID, a.UID, j.CreatedSecrets, created)
	}
	h.svc.Tick(context.Background())
	h.restorePod(id)
}

// Once the disk is verified the agent is kept: a cancel is refused and the
// release still happens.
func TestImportRestoredAgentSurvivesLateCancel(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()
	id := h.startImport(map[string]string{"exportId": exportID})
	h.svc.Tick(context.Background())
	h.setRestorePhase(id, corev1.PodSucceeded)
	h.svc.Tick(context.Background())
	if j := h.job(id); !j.Restored {
		t.Fatal("restore not recorded")
	}
	if rr := h.post("/api/v1/agent-imports/"+id+"/cancel", testAPIKey, nil); rr.Code != http.StatusConflict {
		t.Errorf("cancel after verification = %d, want 409", rr.Code)
	}
	// Force the cancel flag as if it had raced in, then advance.
	j := h.job(id)
	j.CancelRequested = true
	h.svc.Jobs.Update(context.Background(), j)
	h.svc.Tick(context.Background())
	a, err := h.newAgent()
	if err != nil || a.Annotations[kyberv1.AnnotationArchiveHold] != "" {
		t.Fatalf("verified agent deleted or still held: %v %+v", err, a.Annotations)
	}
}

// A restart after the restore step was saved but before the pod existed
// resumes by creating the pod.
func TestImportRecreatesRestorePodAfterRestart(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()
	id := h.startImport(map[string]string{"exportId": exportID})
	h.svc.Tick(context.Background())
	j := h.job(id)
	j.RestoreStarted = false
	h.svc.Jobs.Update(context.Background(), j)
	h.c.Delete(context.Background(), h.restorePod(id))
	h.svc.Tick(context.Background())
	h.restorePod(id)
	if j := h.job(id); j.State != archivejob.StateRunning {
		t.Fatalf("state = %s (%s)", j.State, j.Error)
	}
}

func TestImportRejectsIncompatiblePersistence(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()
	j := h.job(exportID)
	j.Summary.Source.PersistenceMode = "overlay"
	h.svc.Jobs.Update(context.Background(), j)
	h.svc.PersistenceMode = "rootfs"
	rr := h.post("/api/v1/agent-imports", testAPIKey, importBody(map[string]string{"exportId": exportID}, restoredAgent, "20Gi"))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("persistence mismatch = %d %s", rr.Code, rr.Body.String())
	}
}

// A later agent that reuses a failed import's name does not inherit it.
func TestImportListIgnoresEarlierAgentWithSameName(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()
	id := h.startImport(map[string]string{"exportId": exportID})
	h.svc.Tick(context.Background())
	h.setRestorePhase(id, corev1.PodFailed)
	h.svc.Tick(context.Background())
	fresh := sampleAgentCRD(restoredAgent)
	if err := h.c.Create(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	rr := h.do(http.MethodGet, "/api/v1/agent-imports?agent="+restoredAgent, testAPIKey)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), id) {
		t.Errorf("new agent shows the old import: %d %s", rr.Code, rr.Body.String())
	}
}
