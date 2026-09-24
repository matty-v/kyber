package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/capabilities"
	"github.com/matty-v/kyber/pkg/diskarchive"
	"github.com/matty-v/kyber/pkg/taskobject"
)

var testAvatar = []byte("\x89PNG\r\n\x1a\nnot-really-a-png")

// configureSource gives the export harness's source agent every setting an
// import should carry, plus user secrets and an avatar.
func (h *exportHarness) configureSource() {
	h.t.Helper()
	ctx := context.Background()
	avatars := taskobject.NewMemoryStore()
	h.svc.Avatars, h.server.TaskObjectStore = avatars, avatars
	if err := avatars.Put(ctx, "agent-avatars/"+exportAgent, bytes.NewReader(testAvatar), int64(len(testAvatar)),
		taskobject.PutOptions{Filename: "a.png", ContentType: "image/png"}); err != nil {
		h.t.Fatal(err)
	}
	a := h.agent()
	a.Spec.StartupPrompt = "Carry on."
	a.Spec.SessionResume = true
	a.Spec.RequestReplyEnabled = true
	a.Spec.Identity.SoulDescription = "Scruffy-looking."
	a.Spec.Profile = kyberv1.AgentProfile{Alias: "Vault", Description: "Keeps things.", AvatarKey: "agent-avatars/" + exportAgent, AvatarContentType: "image/png"}
	a.Spec.Jobs = []kyberv1.AgentJob{
		{Name: "digest", Schedule: "0 9 * * *", Prompt: "Send the digest.", Exclusive: true, ClearContextAfter: true},
		{Name: "sweep", Schedule: "*/30 * * * *", Prompt: "Sweep.", Paused: true},
	}
	a.Spec.InboundBindings = []kyberv1.AgentInboundBinding{{
		Name: "github", ExistingSecret: exportAgent + "-github-hmac", SignatureHeader: "X-Hub-Signature-256",
		SignaturePrefix: "sha256=", EventHeader: "X-GitHub-Event", MatchEvents: []string{"push"}, Action: "Handle the push.",
	}}
	a.Spec.PublicCapabilities = &kyberv1.AgentPublicCapabilities{
		SchemaVersion: capabilities.SchemaV1Alpha1,
		Identity:      kyberv1.AgentPublicCapabilityIdentity{DisplayName: "Vault", Description: "Keeps things."},
		Capabilities: []kyberv1.AgentPublicCapability{{ID: "store", Version: "1", Name: "Store", Description: "Stores a thing.",
			InputModes: []string{"text/plain"}, OutputModes: []string{"text/plain"}}},
	}
	a.Spec.A2APeers = []kyberv1.AgentA2APeer{
		{Name: "courier", URL: "https://courier.example/a2a", Credential: kyberv1.AgentA2ACredentialRef{ExistingSecret: "courier-token", Key: "token"}},
		{Name: "ghost", URL: "https://ghost.example/a2a", Credential: kyberv1.AgentA2ACredentialRef{ExistingSecret: "ghost-token", Key: "token"}},
	}
	if err := h.c.Update(ctx, a); err != nil {
		h.t.Fatal(err)
	}
	for _, s := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "courier-token", Namespace: "kyber-system"}, Data: map[string][]byte{"token": []byte("t")}},
		{ObjectMeta: metav1.ObjectMeta{Name: exportAgent + "-github-hmac", Namespace: "kyber-system"}, Data: map[string][]byte{"secret": []byte("old")}},
		{ObjectMeta: metav1.ObjectMeta{Name: exportAgent + "-user-secrets-kv", Namespace: "kyber-system"}, Data: map[string][]byte{"API_TOKEN": []byte("kv-value")}},
		{ObjectMeta: metav1.ObjectMeta{Name: exportAgent + "-user-secrets-files", Namespace: "kyber-system"}, Data: map[string][]byte{"cert.bin": []byte("file-value")}},
	} {
		if err := h.c.Create(ctx, s); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *exportHarness) importWith(source map[string]string, apply map[string]any) *http.Response {
	h.t.Helper()
	body := importBody(source, restoredAgent, "20Gi")
	agent := body["agent"].(map[string]any)
	delete(agent, "resources")
	if apply != nil {
		body["apply"] = apply
	}
	return h.post("/api/v1/agent-imports", testAPIKey, body).Result()
}

func (h *exportHarness) importID(resp *http.Response) (string, []string) {
	h.t.Helper()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("start import = %d %s", resp.StatusCode, b)
	}
	var v struct {
		Import struct {
			ID      string
			Cutover []string
		}
	}
	json.Unmarshal(b, &v)
	return v.Import.ID, v.Import.Cutover
}

func (h *exportHarness) secret(name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	return s, h.c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "kyber-system"}, s)
}

func mustContain(t *testing.T, list []string, want ...string) {
	t.Helper()
	all := strings.Join(list, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("checklist lacks %q:\n%s", w, all)
		}
	}
}

func TestExportRecordsTheWholeConfig(t *testing.T) {
	h := newExportHarness(t)
	h.configureSource()
	id, _ := h.completedExport()
	cfg := h.job(id).Summary.Source.Config
	if cfg == nil || cfg.Version != diskarchive.ConfigVersion {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Profile == nil || !bytes.Equal(cfg.Profile.Avatar, testAvatar) || cfg.Profile.Alias != "Vault" {
		t.Errorf("profile = %+v", cfg.Profile)
	}
	if cfg.SoulDescription != "Scruffy-looking." || !cfg.SessionResume || len(cfg.Jobs) != 2 || !cfg.Jobs[1].Paused {
		t.Errorf("config = %+v", cfg)
	}
	if strings.Contains(string(cfg.InboundBindingSpecs), "hmac") || !strings.Contains(string(cfg.InboundBindingSpecs), "X-Hub-Signature-256") {
		t.Errorf("binding specs must carry the definition without the Secret: %s", cfg.InboundBindingSpecs)
	}
	want := []diskarchive.ConfigSecret{{Key: "API_TOKEN", Kind: "kv", Size: 8}, {Key: "CERT", Kind: "file", Size: 10}}
	if !reflect.DeepEqual(cfg.UserSecrets, want) {
		t.Errorf("user secrets = %+v, want %+v", cfg.UserSecrets, want)
	}
	raw, _ := json.Marshal(cfg)
	if bytes.Contains(raw, []byte("kv-value")) || bytes.Contains(raw, []byte("file-value")) {
		t.Error("the archive config contains a secret value")
	}
}

func TestImportAppliesTheArchivedConfig(t *testing.T) {
	h := newExportHarness(t)
	h.configureSource()
	ctx := context.Background()
	exportID, _ := h.completedExport()
	src := h.agent()
	id, cutover := h.importID(h.importWith(map[string]string{"exportId": exportID}, nil))

	a, err := h.newAgent()
	if err != nil {
		t.Fatal(err)
	}
	s := a.Spec
	if s.Model != src.Spec.Model || s.StartupPrompt != "Carry on." || !s.SessionResume || !s.RequestReplyEnabled ||
		s.Identity.SoulDescription != "Scruffy-looking." || !s.Resources.CPU.Equal(src.Spec.Resources.CPU) || !s.Resources.Memory.Equal(src.Spec.Resources.Memory) {
		t.Errorf("create fields not carried: %+v", s)
	}
	// Jobs arrive paused, definitions intact.
	if len(s.Jobs) != 2 {
		t.Fatalf("jobs = %+v", s.Jobs)
	}
	for i, jb := range s.Jobs {
		want := src.Spec.Jobs[i]
		want.Paused = true
		if jb != want {
			t.Errorf("job %d = %+v, want %+v", i, jb, want)
		}
	}
	// Bindings arrive disabled with their own new signing Secret.
	if len(s.InboundBindings) != 1 {
		t.Fatalf("bindings = %+v", s.InboundBindings)
	}
	b := s.InboundBindings[0]
	if !b.Disabled || b.SignatureHeader != "X-Hub-Signature-256" || !reflect.DeepEqual(b.MatchEvents, []string{"push"}) {
		t.Errorf("binding = %+v", b)
	}
	if b.ExistingSecret == "" || b.ExistingSecret == exportAgent+"-github-hmac" {
		t.Errorf("binding secret = %q; it must be new", b.ExistingSecret)
	}
	if sec, err := h.secret(b.ExistingSecret); err != nil || len(sec.Data) == 0 && len(sec.StringData) == 0 {
		t.Errorf("binding secret %s: %v", b.ExistingSecret, err)
	}
	if !reflect.DeepEqual(s.PublicCapabilities, src.Spec.PublicCapabilities) {
		t.Errorf("public capabilities = %+v", s.PublicCapabilities)
	}
	if len(s.A2APeers) != 1 || s.A2APeers[0].Name != "courier" {
		t.Errorf("peers = %+v; only the one whose Secret exists", s.A2APeers)
	}
	// Profile and avatar.
	if s.Profile.Alias != "Vault" || s.Profile.AvatarKey != "agent-avatars/"+restoredAgent || s.Profile.AvatarContentType != "image/png" {
		t.Errorf("profile = %+v", s.Profile)
	}
	obj, err := h.server.TaskObjectStore.Open(ctx, "agent-avatars/"+restoredAgent, nil)
	if err != nil {
		t.Fatal(err)
	}
	img, _ := io.ReadAll(obj.Body)
	obj.Body.Close()
	if !bytes.Equal(img, testAvatar) {
		t.Error("avatar bytes differ")
	}
	// User secrets copied server-side, owned by the new agent.
	for suffix, key := range map[string]string{"-user-secrets-kv": "API_TOKEN", "-user-secrets-files": "cert.bin"} {
		orig, _ := h.secret(exportAgent + suffix)
		cp, err := h.secret(restoredAgent + suffix)
		if err != nil {
			t.Fatalf("%s not copied: %v", suffix, err)
		}
		if !bytes.Equal(cp.Data[key], orig.Data[key]) || !metav1.IsControlledBy(cp, a) || cp.Labels["kyber.io/secret-kind"] != "user-secrets" {
			t.Errorf("%s copy = %+v", suffix, cp.ObjectMeta)
		}
	}

	mustContain(t, cutover, "restored paused (digest, sweep)", "restored disabled with new signing secrets (github)",
		"User secrets were copied", "ghost (needs Secret ghost-token)")
	j := h.job(id)
	for _, name := range []string{b.ExistingSecret, restoredAgent + "-user-secrets-kv", restoredAgent + "-user-secrets-files"} {
		if !contains(j.CreatedSecrets, name) {
			t.Errorf("created secrets %v lack %s", j.CreatedSecrets, name)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestImportExplicitRequestFieldsWin(t *testing.T) {
	h := newExportHarness(t)
	h.configureSource()
	exportID, _ := h.completedExport()
	body := importBody(map[string]string{"exportId": exportID}, restoredAgent, "20Gi")
	body["agent"].(map[string]any)["startupPrompt"] = "Start fresh."
	h.importID(h.post("/api/v1/agent-imports", testAPIKey, body).Result())
	a, _ := h.newAgent()
	if a.Spec.StartupPrompt != "Start fresh." {
		t.Errorf("startupPrompt = %q", a.Spec.StartupPrompt)
	}
}

func TestImportApplySkip(t *testing.T) {
	h := newExportHarness(t)
	h.configureSource()
	exportID, _ := h.completedExport()
	_, cutover := h.importID(h.importWith(map[string]string{"exportId": exportID},
		map[string]any{"jobs": "skip", "bindings": "skip", "secrets": "skip"}))
	a, _ := h.newAgent()
	if len(a.Spec.Jobs) != 0 || len(a.Spec.InboundBindings) != 0 {
		t.Errorf("skipped items applied: jobs=%v bindings=%v", a.Spec.Jobs, a.Spec.InboundBindings)
	}
	if _, err := h.secret(restoredAgent + "-user-secrets-kv"); !k8serrors.IsNotFound(err) {
		t.Errorf("user secrets copied despite skip: %v", err)
	}
	mustContain(t, cutover, "were not copied (digest, sweep)", "binding(s) that were not copied (github)",
		"Set their values on the new agent: API_TOKEN (kv), CERT (file)")
}

func TestImportConfigOff(t *testing.T) {
	h := newExportHarness(t)
	h.configureSource()
	exportID, _ := h.completedExport()
	_, cutover := h.importID(h.importWith(map[string]string{"exportId": exportID}, map[string]any{"config": false}))
	a, _ := h.newAgent()
	if a.Spec.StartupPrompt != "" || len(a.Spec.Jobs) != 0 || a.Spec.Profile.Alias != "" || a.Spec.PublicCapabilities != nil {
		t.Errorf("config applied with config=false: %+v", a.Spec)
	}
	mustContain(t, cutover, "configuration was not applied")
}

func TestImportWithSourceGoneListsSecrets(t *testing.T) {
	h := newExportHarness(t)
	h.configureSource()
	exportID, _ := h.completedExport()
	if err := h.c.Delete(context.Background(), h.agent()); err != nil {
		t.Fatal(err)
	}
	_, cutover := h.importID(h.importWith(map[string]string{"exportId": exportID}, nil))
	if _, err := h.secret(restoredAgent + "-user-secrets-kv"); !k8serrors.IsNotFound(err) {
		t.Errorf("secrets copied from a deleted source: %v", err)
	}
	mustContain(t, cutover, "exporter is no longer on this installation", "API_TOKEN (kv), CERT (file)")
}

func TestImportRejectsBadApply(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()
	resp := h.importWith(map[string]string{"exportId": exportID}, map[string]any{"jobs": "active"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if _, err := h.newAgent(); !k8serrors.IsNotFound(err) {
		t.Errorf("agent created for a rejected import: %v", err)
	}
}

func TestImportLegacyArchiveStillImports(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()
	// An archive from before version 2: names only, no definitions.
	src := h.job(exportID)
	src.Summary.Source.Config = &diskarchive.Config{Runtime: "claude-code", Jobs: []diskarchive.ConfigJob{{Name: "digest", Schedule: "0 9 * * *", Prompt: "p"}},
		InboundBindings: []string{"github"}}
	if err := h.svc.Jobs.Update(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	_, cutover := h.importID(h.importWith(map[string]string{"exportId": exportID}, nil))
	a, _ := h.newAgent()
	if len(a.Spec.Jobs) != 1 || !a.Spec.Jobs[0].Paused || len(a.Spec.InboundBindings) != 0 {
		t.Errorf("legacy import: jobs=%+v bindings=%+v", a.Spec.Jobs, a.Spec.InboundBindings)
	}
	mustContain(t, cutover, "predates full configuration capture", "binding(s) that were not copied (github)")
}

func TestAbandonedImportRemovesAppliedSecrets(t *testing.T) {
	h := newExportHarness(t)
	h.configureSource()
	exportID, _ := h.completedExport()
	id, _ := h.importID(h.importWith(map[string]string{"exportId": exportID}, nil))
	a, _ := h.newAgent()
	bindingSecret := a.Spec.InboundBindings[0].ExistingSecret
	h.svc.Tick(context.Background())
	h.setRestorePhase(id, corev1.PodFailed)
	h.svc.Tick(context.Background())
	if j := h.job(id); j.State != archivejob.StateFailed {
		t.Fatalf("state = %s", j.State)
	}
	for _, name := range []string{bindingSecret, restoredAgent + "-user-secrets-kv", restoredAgent + "-user-secrets-files"} {
		if _, err := h.secret(name); !k8serrors.IsNotFound(err) {
			t.Errorf("secret %s left behind: %v", name, err)
		}
	}
}

func TestImportNeverRestoresHarnessLogin(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()
	body := importBody(map[string]string{"exportId": exportID}, restoredAgent, "20Gi")
	body["keepCredentialFiles"] = true
	id, cutover := h.importID(h.post("/api/v1/agent-imports", testAPIKey, body).Result())
	j := h.job(id)
	if len(j.Skip) != 1 || !strings.HasSuffix(j.Skip[0], ".claude/.credentials.json") {
		t.Errorf("skip = %v; the harness login must be left out even with keepCredentialFiles", j.Skip)
	}
	mustContain(t, cutover, "Harness login files are never restored")
	h.svc.Tick(context.Background())
	env := h.restorePod(id).Spec.Containers[0].Env
	if !strings.Contains(envValue(env, "KYBER_ARCHIVE_LOGIN_FILES"), ".claude/.credentials.json") {
		t.Errorf("restore pod login files = %q", envValue(env, "KYBER_ARCHIVE_LOGIN_FILES"))
	}
}

func TestArchiveViewsLeaveTheAvatarOut(t *testing.T) {
	h := newExportHarness(t)
	h.configureSource()
	id, _ := h.completedExport()
	rr := h.do(http.MethodGet, "/api/v1/agents/"+exportAgent+"/exports/"+id, testAPIKey)
	var v struct {
		Summary struct {
			Source struct {
				Config struct{ Profile map[string]any }
			}
		}
	}
	json.Unmarshal(rr.Body.Bytes(), &v)
	p := v.Summary.Source.Config.Profile
	if rr.Code != http.StatusOK || p["hasAvatar"] != true || p["avatar"] != nil || p["alias"] != "Vault" {
		t.Errorf("view profile = %d %v", rr.Code, p)
	}
	// The stored job keeps the bytes for the import.
	if !bytes.Equal(h.job(id).Summary.Source.Config.Profile.Avatar, testAvatar) {
		t.Error("stored summary lost the avatar")
	}
}

func TestExportPodReadsItsDescriptionFromTheJob(t *testing.T) {
	h := newExportHarness(t)
	h.configureSource()
	id := h.runToArchiving()
	env := h.exportPod(id).Spec.Containers[0].Env
	url := envValue(env, "KYBER_ARCHIVE_DESCRIPTION_URL")
	if !strings.HasSuffix(url, "/internal/archive-jobs/"+id+"/description") || envValue(env, "KYBER_ARCHIVE_DESCRIPTION") != "" {
		t.Fatalf("export pod env = %+v", env)
	}
	get := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/internal/archive-jobs/"+id+"/description", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		h.internal.ServeHTTP(rr, req)
		return rr
	}
	if rr := get("wrong"); rr.Code != http.StatusForbidden {
		t.Errorf("description with a bad token = %d", rr.Code)
	}
	rr := get(h.podToken(id))
	var d struct {
		Source diskarchive.Source
	}
	json.Unmarshal(rr.Body.Bytes(), &d)
	if rr.Code != http.StatusOK || d.Source.Agent != exportAgent || d.Source.Config == nil || !bytes.Equal(d.Source.Config.Profile.Avatar, testAvatar) {
		t.Errorf("description = %d %.200s", rr.Code, rr.Body.String())
	}
}

// An upload's manifest can claim any source agent, so it never authorizes
// copying that agent's secret values.
func TestImportFromUploadNeverCopiesSecrets(t *testing.T) {
	h := newExportHarness(t)
	h.configureSource()
	ctx := context.Background()
	exportID, data := h.completedExport()
	_ = exportID
	req := httptest.NewRequest(http.MethodPost, "/api/v1/archive-uploads", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	h.server.BuildHandler().ServeHTTP(rr, req)
	var up struct{ ID string }
	json.Unmarshal(rr.Body.Bytes(), &up)
	h.svc.Tick(ctx)
	h.playVerifyPod(up.ID)
	h.svc.Tick(ctx)
	if j := h.job(up.ID); j.State != archivejob.StateCompleted || j.Summary.Source.Config == nil {
		t.Fatalf("upload = %s", j.State)
	}
	_, cutover := h.importID(h.importWith(map[string]string{"uploadId": up.ID}, nil))
	if _, err := h.secret(restoredAgent + "-user-secrets-kv"); !k8serrors.IsNotFound(err) {
		t.Errorf("secrets copied on the strength of an upload: %v", err)
	}
	mustContain(t, cutover, "only from an export made on this installation", "API_TOKEN (kv)")
	// Everything else still applies.
	a, _ := h.newAgent()
	if len(a.Spec.Jobs) != 2 || len(a.Spec.InboundBindings) != 1 {
		t.Errorf("upload import did not apply jobs/bindings: %+v", a.Spec)
	}
}

// Archived jobs and bindings pass the same checks as the API routes; an
// invalid one is left off and listed, and the rest still apply.
func TestImportSkipsInvalidArchivedJobsAndBindings(t *testing.T) {
	h := newExportHarness(t)
	exportID, _ := h.completedExport()
	src := h.job(exportID)
	src.Summary.Source.Config = &diskarchive.Config{Version: diskarchive.ConfigVersion, Runtime: "claude-code",
		Jobs: []diskarchive.ConfigJob{
			{Name: "good", Schedule: "0 9 * * *", Prompt: "p"},
			{Name: "Bad Name", Schedule: "0 9 * * *", Prompt: "p"},
			{Name: "noschedule", Schedule: "nope", Prompt: "p"},
			{Name: "good", Schedule: "0 9 * * *", Prompt: "dup"},
		},
		InboundBindings:     []string{"ok", "noaction"},
		InboundBindingSpecs: json.RawMessage(`[{"name":"ok","existingSecret":"","signatureHeader":"X-Sig","action":"do it"},{"name":"noaction","existingSecret":"","signatureHeader":"X-Sig"}]`),
	}
	if err := h.svc.Jobs.Update(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	_, cutover := h.importID(h.importWith(map[string]string{"exportId": exportID}, nil))
	a, _ := h.newAgent()
	if len(a.Spec.Jobs) != 1 || a.Spec.Jobs[0].Name != "good" || len(a.Spec.InboundBindings) != 1 || a.Spec.InboundBindings[0].Name != "ok" {
		t.Errorf("jobs=%+v bindings=%+v", a.Spec.Jobs, a.Spec.InboundBindings)
	}
	mustContain(t, cutover, "scheduled job Bad Name", "scheduled job noschedule", "duplicate name", "inbound binding noaction: action is required")
}
