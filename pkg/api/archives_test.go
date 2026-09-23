package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/archivestore"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/diskarchive"
)

// exportHarness is a fake cluster with one running agent, its machine,
// volume claim and pod, plus an archive service over in-memory job state and
// a filesystem store. The test plays the export pod itself.
type exportHarness struct {
	t        *testing.T
	server   *api.Server
	svc      *api.ArchiveService
	internal http.Handler
	c        client.Client
	store    *archivestore.FilesystemStore
	now      time.Time
}

const exportAgent = "exporter"

func newExportHarness(t *testing.T, callers ...api.ScopedCaller) *exportHarness {
	t.Helper()
	agent := sampleAgentCRD(exportAgent)
	agent.UID = types.UID("exporter-uid")
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	machine := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"}}
	machine.Status.NodeName = "node-a"
	sc := "local-path"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-" + exportAgent + "-pv", Namespace: "kyber-system"},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &sc,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-" + exportAgent, Namespace: "kyber-system"},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Volumes: []corev1.Volume{
				{Name: "persist", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "agent-" + exportAgent + "-pv"}}},
				{Name: "pod-token", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: exportAgent + "-pod-token"}}},
			},
			Containers: []corev1.Container{{Name: "agent", VolumeMounts: []corev1.VolumeMount{
				{Name: "persist", MountPath: "/persist"},
				{Name: "pod-token", MountPath: "/var/run/secrets/kyber"},
			}}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent, machine, pvc, pod).Build()
	store, err := archivestore.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &exportHarness{t: t, c: c, store: store, now: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	h.svc = &api.ArchiveService{
		Client: c, Namespace: "kyber-system", Store: store, Jobs: archivejob.NewMemoryStore(),
		SigningKey: []byte("signing-key"), ToolImage: "kyber/control-plane:test",
		InternalURL: "http://cp.kyber-system.svc:8082",
		Limits:      api.ArchiveLimits{MaxArchiveBytes: 10 << 20},
		Now:         func() time.Time { return h.now },
	}
	h.server = &api.Server{K8sClient: c, APIKey: testAPIKey, Namespace: "kyber-system", Archives: h.svc, Callers: callers}
	h.internal = api.NewInternalServer(briefstore.NewMemoryStore(), api.WithArchiveService(h.svc)).Handler()
	return h
}

func (h *exportHarness) do(method, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rr := httptest.NewRecorder()
	h.server.BuildHandler().ServeHTTP(rr, req)
	return rr
}

func (h *exportHarness) agent() *kyberv1.Agent {
	a := &kyberv1.Agent{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: exportAgent, Namespace: "kyber-system"}, a); err != nil {
		h.t.Fatal(err)
	}
	return a
}

func (h *exportHarness) job(id string) *archivejob.Job {
	j, err := h.svc.Jobs.Get(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return j
}

func (h *exportHarness) start() string {
	h.t.Helper()
	rr := h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/exports", testAPIKey)
	if rr.Code != http.StatusAccepted {
		h.t.Fatalf("start export: %d %s", rr.Code, rr.Body.String())
	}
	var v struct{ ID string }
	json.Unmarshal(rr.Body.Bytes(), &v)
	return v.ID
}

// stopAgentPod plays the controller honouring desiredPhase=Stopped.
func (h *exportHarness) stopAgentPod() {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-" + exportAgent, Namespace: "kyber-system"}}
	if err := h.c.Delete(context.Background(), pod); err != nil && !k8serrors.IsNotFound(err) {
		h.t.Fatal(err)
	}
}

func (h *exportHarness) exportPod(id string) *corev1.Pod {
	pod := &corev1.Pod{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: "disk-export-" + id, Namespace: "kyber-system"}, pod); err != nil {
		h.t.Fatalf("export pod: %v", err)
	}
	return pod
}

func (h *exportHarness) podToken(id string) string {
	s := &corev1.Secret{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: "disk-export-" + id + "-token", Namespace: "kyber-system"}, s); err != nil {
		h.t.Fatalf("token secret: %v", err)
	}
	return s.StringData["token"]
}

// upload plays kyber-disk-archive: archive a tree and PUT it.
func (h *exportHarness) upload(id, token string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/internal/archive-jobs/"+id+"/upload", body)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.internal.ServeHTTP(rr, req)
	return rr
}

func sampleDisk(t *testing.T) []byte {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "agentroot/home/kyber/dev/identity/skills/s"), 0o755)
	os.MkdirAll(filepath.Join(root, "agentroot/home/kyber/.claude"), 0o700)
	os.WriteFile(filepath.Join(root, "agentroot/home/kyber/dev/identity/skills/s/SKILL.md"), []byte("# s\n"), 0o644)
	os.WriteFile(filepath.Join(root, "agentroot/home/kyber/.claude/.credentials.json"), []byte("{}"), 0o600)
	var buf bytes.Buffer
	if _, err := diskarchive.Write(context.Background(), root, &buf, diskarchive.WriteOptions{Source: diskarchive.Source{Agent: exportAgent}}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (h *exportHarness) setPodPhase(id string, phase corev1.PodPhase) {
	pod := h.exportPod(id)
	pod.Status.Phase = phase
	if phase == corev1.PodFailed {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "archive", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "archiving: file changed or became unreadable during export: locked"}}}}
	}
	if err := h.c.Status().Update(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
}

// playVerifyPod does what kyber-disk-archive verify does: read the archive
// back through the job's endpoint, verify it, and post the summary.
func (h *exportHarness) playVerifyPod(id string) {
	h.t.Helper()
	pod := &corev1.Pod{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: "disk-verify-" + id, Namespace: "kyber-system"}, pod); err != nil {
		h.t.Fatalf("verify pod: %v", err)
	}
	if len(pod.Spec.Volumes) != 0 || *pod.Spec.Containers[0].SecurityContext.RunAsUser == 0 {
		h.t.Errorf("verify pod must mount nothing and run unprivileged: %+v", pod.Spec)
	}
	token := h.podToken(id)
	req := httptest.NewRequest(http.MethodGet, "/internal/archive-jobs/"+id+"/archive", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.internal.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		h.t.Fatalf("verify read = %d", rr.Code)
	}
	data := rr.Body.Bytes()
	m, err := diskarchive.Verify(bytes.NewReader(data), int64(len(data)), diskarchive.Limits{})
	if err != nil {
		h.t.Fatalf("stored archive does not verify: %v", err)
	}
	body, _ := json.Marshal(archivejob.SummaryOf(m))
	req = httptest.NewRequest(http.MethodPost, "/internal/archive-jobs/"+id+"/summary", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	h.internal.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		h.t.Fatalf("summary post = %d %s", rr.Code, rr.Body.String())
	}
}

func (h *exportHarness) setVerifyPodPhase(id string, phase corev1.PodPhase, msg string) {
	pod := &corev1.Pod{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: "disk-verify-" + id, Namespace: "kyber-system"}, pod); err != nil {
		h.t.Fatalf("verify pod: %v", err)
	}
	pod.Status.Phase = phase
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "archive", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: msg}}}}
	if err := h.c.Status().Update(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
}

// runToArchiving drives a fresh export to the point where the pod exists.
func (h *exportHarness) runToArchiving() string {
	h.t.Helper()
	id := h.start()
	ctx := context.Background()
	h.svc.Tick(ctx)
	a := h.agent()
	if a.Annotations[kyberv1.AnnotationArchiveHold] != id || a.Spec.DesiredPhase != kyberv1.AgentPhaseStopped {
		h.t.Fatalf("after first tick: hold=%q desired=%s", a.Annotations[kyberv1.AnnotationArchiveHold], a.Spec.DesiredPhase)
	}
	h.svc.Tick(ctx) // pod still running: waits
	if j := h.job(id); j.Step != archivejob.StepPausing {
		h.t.Fatalf("step = %s, want pausing while the agent pod exists", j.Step)
	}
	h.stopAgentPod()
	h.svc.Tick(ctx)
	if j := h.job(id); j.Step != archivejob.StepArchiving {
		h.t.Fatalf("step = %s, want archiving", j.Step)
	}
	return id
}

func TestExportLifecycleEndToEnd(t *testing.T) {
	h := newExportHarness(t)
	ctx := context.Background()
	id := h.runToArchiving()

	pod := h.exportPod(id)
	c := pod.Spec.Containers[0]
	if pod.Spec.NodeName != "node-a" || !pod.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly || !c.VolumeMounts[0].ReadOnly {
		t.Errorf("export pod must be same-node and read-only: %+v", pod.Spec)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("export pod must not mount a service-account token")
	}
	if caps := c.SecurityContext.Capabilities; len(caps.Drop) != 1 || caps.Drop[0] != "ALL" || len(caps.Add) != 1 {
		t.Errorf("export pod capabilities = %+v", caps)
	}
	if pod.Labels["kyber.io/agent"] != exportAgent {
		t.Error("export pod must carry kyber.io/agent so agent network policy applies")
	}

	// Starting the agent while the export holds its volume is refused.
	if rr := h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/start", testAPIKey); rr.Code != http.StatusConflict {
		t.Errorf("start during export = %d, want 409", rr.Code)
	}

	// A wrong token cannot upload.
	if rr := h.upload(id, "wrong", bytes.NewReader([]byte("x"))); rr.Code != http.StatusForbidden {
		t.Errorf("wrong token upload = %d, want 403", rr.Code)
	}
	data := sampleDisk(t)
	token := h.podToken(id)
	if rr := h.upload(id, token, bytes.NewReader(data)); rr.Code != http.StatusNoContent {
		t.Fatalf("upload = %d %s", rr.Code, rr.Body.String())
	}
	// The stored object is sealed, not the plain ZIP.
	raw, _ := h.store.Open(ctx, "exports/"+id+"/archive.sealed", 0, -1)
	stored, _ := io.ReadAll(raw)
	raw.Close()
	if bytes.Contains(stored, []byte("SKILL.md")) {
		t.Error("stored archive is readable without the data key")
	}

	h.setPodPhase(id, corev1.PodSucceeded)
	h.svc.Tick(ctx)
	a := h.agent()
	if a.Annotations[kyberv1.AnnotationArchiveHold] != "" || a.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Fatalf("agent not released after upload: hold=%q desired=%s", a.Annotations[kyberv1.AnnotationArchiveHold], a.Spec.DesiredPhase)
	}
	h.svc.Tick(ctx) // starts the verification pod
	if j := h.job(id); j.Step != archivejob.StepVerifying || j.State != archivejob.StateRunning {
		t.Fatalf("after release: %s/%s", j.State, j.Step)
	}
	h.playVerifyPod(id)
	h.svc.Tick(ctx)
	j := h.job(id)
	if j.State != archivejob.StateCompleted || j.Summary == nil || j.Summary.Totals.Files != 2 {
		t.Fatalf("job = %s summary=%+v err=%s", j.State, j.Summary, j.Error)
	}
	if len(j.Summary.Sensitive) != 1 {
		t.Errorf("sensitive = %v, want the Claude credentials file", j.Summary.Sensitive)
	}
	mounts := map[string]bool{}
	for _, m := range j.Mounts {
		mounts[m.Path] = m.Archived
	}
	if !mounts["/persist"] || mounts["/var/run/secrets/kyber"] {
		t.Errorf("mount inventory = %+v", j.Mounts)
	}

	// Public view never leaks keys.
	rr := h.do(http.MethodGet, "/api/v1/agents/"+exportAgent+"/exports/"+id, testAPIKey)
	if strings.Contains(rr.Body.String(), "wrappedKey") || strings.Contains(rr.Body.String(), "tokenHash") || strings.Contains(rr.Body.String(), "objectKey") {
		t.Errorf("job view leaks internals: %s", rr.Body.String())
	}

	// Download: link, then a plain ZIP identical to what the pod uploaded.
	rr = h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/exports/"+id+"/download-link", testAPIKey)
	var link struct{ URL string }
	json.Unmarshal(rr.Body.Bytes(), &link)
	if rr.Code != http.StatusOK || !strings.HasPrefix(link.URL, "/api/v1/archive-downloads/") {
		t.Fatalf("download-link = %d %s", rr.Code, rr.Body.String())
	}
	rr = h.do(http.MethodGet, link.URL, "")
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), data) {
		t.Fatalf("download = %d, %d bytes (want %d)", rr.Code, rr.Body.Len(), len(data))
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, exportAgent+"-disk-") {
		t.Errorf("Content-Disposition = %q", cd)
	}

	// A tampered link is refused; an expired one too.
	if rr := h.do(http.MethodGet, link.URL+"x", ""); rr.Code != http.StatusForbidden {
		t.Errorf("tampered link = %d, want 403", rr.Code)
	}
	h.now = h.now.Add(11 * time.Minute)
	if rr := h.do(http.MethodGet, link.URL, ""); rr.Code != http.StatusForbidden {
		t.Errorf("expired link = %d, want 403", rr.Code)
	}

	// Retention: the archive is deleted and the job expires.
	h.now = h.now.Add(73 * time.Hour)
	h.svc.Tick(ctx)
	if j := h.job(id); j.State != archivejob.StateExpired {
		t.Errorf("state after retention = %s, want expired", j.State)
	}
	if _, err := h.store.Size(ctx, "exports/"+id+"/archive.sealed"); err != archivestore.ErrNotFound {
		t.Errorf("archive still stored after expiry: %v", err)
	}
}

func TestExportCancelReleasesAgentAndDeletesBytes(t *testing.T) {
	h := newExportHarness(t)
	ctx := context.Background()
	id := h.runToArchiving()
	if rr := h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/exports/"+id+"/cancel", testAPIKey); rr.Code != http.StatusAccepted {
		t.Fatalf("cancel = %d %s", rr.Code, rr.Body.String())
	}
	// An upload after cancel is refused.
	if rr := h.upload(id, h.podToken(id), bytes.NewReader(sampleDisk(t))); rr.Code != http.StatusForbidden {
		t.Errorf("upload after cancel = %d, want 403", rr.Code)
	}
	h.svc.Tick(ctx)
	j := h.job(id)
	if j.State != archivejob.StateCanceled {
		t.Fatalf("state = %s, want canceled", j.State)
	}
	a := h.agent()
	if a.Annotations[kyberv1.AnnotationArchiveHold] != "" || a.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Errorf("agent not restored: hold=%q desired=%s", a.Annotations[kyberv1.AnnotationArchiveHold], a.Spec.DesiredPhase)
	}
	pod := &corev1.Pod{}
	if err := h.c.Get(ctx, types.NamespacedName{Name: "disk-export-" + id, Namespace: "kyber-system"}, pod); !k8serrors.IsNotFound(err) {
		t.Errorf("export pod survived cancel: %v", err)
	}
	// A retry is a new job and succeeds past creation.
	if rr := h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/exports", testAPIKey); rr.Code != http.StatusAccepted {
		t.Errorf("retry after cancel = %d", rr.Code)
	}
}

func TestExportFailuresReleaseAgent(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *exportHarness, id string)
		want  string
	}{
		{"pod reports unreadable file", func(h *exportHarness, id string) {
			h.setPodPhase(id, corev1.PodFailed)
		}, "unreadable"},
		{"pod exits without upload", func(h *exportHarness, id string) {
			h.setPodPhase(id, corev1.PodSucceeded)
		}, "without a completed upload"},
		{"job deadline", func(h *exportHarness, id string) {
			h.now = h.now.Add(3 * time.Hour)
		}, "time limit"},
		{"oversized archive", func(h *exportHarness, id string) {
			big := bytes.Repeat([]byte("x"), 11<<20)
			if rr := h.upload(id, h.podToken(id), bytes.NewReader(big)); rr.Code != http.StatusRequestEntityTooLarge {
				h.t.Errorf("oversized upload = %d, want 413", rr.Code)
			}
			h.setPodPhase(id, corev1.PodFailed)
		}, "exit 1"},
		{"corrupt upload fails verification", func(h *exportHarness, id string) {
			if rr := h.upload(id, h.podToken(id), bytes.NewReader([]byte("not a zip"))); rr.Code != http.StatusNoContent {
				h.t.Fatalf("upload = %d", rr.Code)
			}
			h.setPodPhase(id, corev1.PodSucceeded)
			h.svc.Tick(context.Background()) // release
			h.svc.Tick(context.Background()) // verify pod
			h.setVerifyPodPhase(id, corev1.PodFailed, "diskarchive: archive is invalid: zip: not a valid zip file")
		}, "verification"},
		{"export pod cannot pull its image", func(h *exportHarness, id string) {
			pod := h.exportPod(id)
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "archive", State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
			if err := h.c.Status().Update(context.Background(), pod); err != nil {
				h.t.Fatal(err)
			}
		}, "ImagePullBackOff"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newExportHarness(t)
			id := h.runToArchiving()
			tc.setup(h, id)
			h.svc.Tick(context.Background())
			j := h.job(id)
			if j.State != archivejob.StateFailed || !strings.Contains(j.Error, tc.want) {
				t.Fatalf("state=%s error=%q, want failed containing %q", j.State, j.Error, tc.want)
			}
			a := h.agent()
			if a.Annotations[kyberv1.AnnotationArchiveHold] != "" || a.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
				t.Errorf("agent not restored: hold=%q desired=%s", a.Annotations[kyberv1.AnnotationArchiveHold], a.Spec.DesiredPhase)
			}
			if _, err := h.store.Size(context.Background(), "exports/"+id+"/archive.sealed"); err != archivestore.ErrNotFound {
				t.Errorf("failed export left bytes behind: %v", err)
			}
		})
	}
}

func TestExportPauseTimeout(t *testing.T) {
	h := newExportHarness(t)
	id := h.start()
	h.svc.Tick(context.Background())
	h.now = h.now.Add(6 * time.Minute)
	h.svc.Tick(context.Background())
	if j := h.job(id); j.State != archivejob.StateFailed || !strings.Contains(j.Error, "pause") {
		t.Fatalf("state=%s error=%q", j.State, j.Error)
	}
	if a := h.agent(); a.Spec.DesiredPhase != kyberv1.AgentPhaseRunning || a.Annotations[kyberv1.AnnotationArchiveHold] != "" {
		t.Errorf("agent not restored after pause timeout")
	}
}

func TestExportOperatorIntentWinsOverRelease(t *testing.T) {
	h := newExportHarness(t)
	id := h.runToArchiving()
	// An operator presses Stop during the export: desiredPhase is already
	// Stopped, but the job must then leave it Stopped rather than restore
	// Running. Model that by the operator changing intent to NeedsAuth.
	a := h.agent()
	a.Spec.DesiredPhase = kyberv1.AgentPhaseNeedsAuth
	if err := h.c.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/exports/"+id+"/cancel", testAPIKey)
	h.svc.Tick(context.Background())
	if got := h.agent().Spec.DesiredPhase; got != kyberv1.AgentPhaseNeedsAuth {
		t.Errorf("desiredPhase = %s, want the operator's NeedsAuth", got)
	}
}

func TestExportRequiresArchiveScopeAndResource(t *testing.T) {
	h := newExportHarness(t,
		api.ScopedCaller{Name: "ops", Key: "ops-key", Scopes: []string{"lifecycle:admin"}},
		api.ScopedCaller{Name: "other", Key: "other-key", Scopes: []string{"archives:admin"}, AgentResources: []string{"kyber-system/someone-else"}},
		api.ScopedCaller{Name: "archivist", Key: "archivist-key", Scopes: []string{"archives:admin"}, AgentResources: []string{"kyber-system/" + exportAgent}},
	)
	if rr := h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/exports", "ops-key"); rr.Code != http.StatusForbidden {
		t.Errorf("lifecycle:admin caller = %d, want 403", rr.Code)
	}
	if rr := h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/exports", "other-key"); rr.Code != http.StatusNotFound {
		t.Errorf("caller scoped to another agent = %d, want 404", rr.Code)
	}
	if rr := h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/exports", "archivist-key"); rr.Code != http.StatusAccepted {
		t.Errorf("archivist = %d %s", rr.Code, rr.Body.String())
	}
	// One active export per agent.
	if rr := h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/exports", testAPIKey); rr.Code != http.StatusConflict {
		t.Errorf("second export = %d, want 409", rr.Code)
	}
}

func TestExportRejectsTransientPhase(t *testing.T) {
	h := newExportHarness(t)
	a := h.agent()
	a.Status.Phase = kyberv1.AgentPhaseStarting
	if err := h.c.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if rr := h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/exports", testAPIKey); rr.Code != http.StatusConflict {
		t.Errorf("export from Starting = %d, want 409", rr.Code)
	}
}

func TestExportUnavailableWithoutService(t *testing.T) {
	s := &api.Server{K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).Build(), APIKey: testAPIKey, Namespace: "kyber-system"}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/x/exports", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("no archive service = %d, want 503", rr.Code)
	}
}

// An agent whose desiredPhase was cleared (as the controller does after a
// restart) comes back to Running after the export.
func TestExportRestoresRunningWhenDesiredPhaseWasEmpty(t *testing.T) {
	h := newExportHarness(t)
	a := h.agent()
	a.Spec.DesiredPhase = ""
	if err := h.c.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	id := h.runToArchiving()
	h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/exports/"+id+"/cancel", testAPIKey)
	h.svc.Tick(context.Background())
	if got := h.agent().Spec.DesiredPhase; got != kyberv1.AgentPhaseRunning {
		t.Errorf("desiredPhase after export = %q, want Running", got)
	}
}

// Stop pressed during an export is the operator's intent; release keeps it.
func TestExportKeepsOperatorStop(t *testing.T) {
	h := newExportHarness(t)
	id := h.runToArchiving()
	if rr := h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/stop", testAPIKey); rr.Code != http.StatusOK {
		t.Fatalf("stop during export = %d %s", rr.Code, rr.Body.String())
	}
	if h.agent().Annotations[kyberv1.AnnotationArchivePaused] != "" {
		t.Fatal("Stop did not clear the job's paused mark")
	}
	h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/exports/"+id+"/cancel", testAPIKey)
	h.svc.Tick(context.Background())
	a := h.agent()
	if a.Spec.DesiredPhase != kyberv1.AgentPhaseStopped || a.Annotations[kyberv1.AnnotationArchiveHold] != "" {
		t.Errorf("after release: desired=%s hold=%q, want the operator's Stopped and no hold", a.Spec.DesiredPhase, a.Annotations[kyberv1.AnnotationArchiveHold])
	}
}

// A job accepts exactly one upload attempt.
func TestExportRejectsSecondUpload(t *testing.T) {
	h := newExportHarness(t)
	id := h.runToArchiving()
	token := h.podToken(id)
	if rr := h.upload(id, token, bytes.NewReader(sampleDisk(t))); rr.Code != http.StatusNoContent {
		t.Fatalf("first upload = %d", rr.Code)
	}
	if rr := h.upload(id, token, bytes.NewReader(sampleDisk(t))); rr.Code != http.StatusConflict {
		t.Errorf("second upload = %d, want 409", rr.Code)
	}
	if _, err := h.store.Size(context.Background(), "exports/"+id+"/archive.sealed"); err != nil {
		t.Errorf("the first upload's object is gone: %v", err)
	}
}

// A job whose cleanup was interrupted after it recorded its outcome finishes
// that cleanup on the next step, and a stale writer cannot undo it.
func TestFinishResumesAfterInterruption(t *testing.T) {
	h := newExportHarness(t)
	id := h.runToArchiving()
	j := h.job(id)
	j.Finishing = archivejob.StateFailed
	j.FinishReason = "simulated crash mid-cleanup"
	if err := h.svc.Jobs.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	// Uploads are refused once a job is finishing.
	if rr := h.upload(id, h.podToken(id), bytes.NewReader(sampleDisk(t))); rr.Code != http.StatusForbidden {
		t.Errorf("upload to a finishing job = %d, want 403", rr.Code)
	}
	h.svc.Tick(context.Background())
	j = h.job(id)
	if j.State != archivejob.StateFailed || j.Error != "simulated crash mid-cleanup" || j.Finishing != "" {
		t.Fatalf("state=%s error=%q finishing=%q", j.State, j.Error, j.Finishing)
	}
	if a := h.agent(); a.Annotations[kyberv1.AnnotationArchiveHold] != "" || a.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Errorf("agent not released by the resumed cleanup")
	}
}
