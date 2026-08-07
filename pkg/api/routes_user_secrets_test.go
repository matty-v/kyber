package api_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/messagebuffer"
)

// buildUserSecretsHandler returns an API handler backed by a fake client with
// the given agent and the two shell user-secrets Secrets pre-seeded (mirrors
// what the reconciler does). The agent is seeded Running — the canonical case
// for a secret update, and the one where the pod must actually roll (kyber#515).
// Returns the handler and the fake client so tests can inspect the stored
// Secrets after requests.
func buildUserSecretsHandler(t *testing.T, agentName string) (http.Handler, client.Client) {
	return buildUserSecretsHandlerWithPhase(t, agentName, kyberv1.AgentPhaseRunning)
}

// buildUserSecretsHandlerWithPhase is buildUserSecretsHandler with an explicit
// seeded Status.Phase, so tests can exercise the phase-aware roll path of
// rollAgentForUserSecret (kyber#515) — a live agent rolls, a dormant one is
// left untouched.
func buildUserSecretsHandlerWithPhase(t *testing.T, agentName string, phase kyberv1.AgentPhase) (http.Handler, client.Client) {
	t.Helper()

	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Machine: "worker-1",
			Runtime: "claude-code",
		},
		Status: kyberv1.AgentStatus{Phase: phase},
	}
	kvSec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentName + "-user-secrets-kv",
			Namespace: "kyber-system",
			Labels:    map[string]string{"kyber.io/agent": agentName, "kyber.io/secret-kind": "user-secrets"},
		},
		Type: corev1.SecretTypeOpaque,
	}
	fileSec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentName + "-user-secrets-files",
			Namespace: "kyber-system",
			Labels:    map[string]string{"kyber.io/agent": agentName, "kyber.io/secret-kind": "user-secrets"},
		},
		Type: corev1.SecretTypeOpaque,
	}

	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(defaultMachine(), agent, kvSec, fileSec).
		Build()
	s := &api.Server{
		K8sClient:     fakeClient,
		MessageBuffer: messagebuffer.NewMemoryBuffer(),
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{"claude-code": true},
	}
	return s.BuildHandler(), fakeClient
}

// authedJSONRequest is authedRequest with an explicit Content-Type — the base
// helper already sets application/json, so this is a thin wrapper for readability.
func authedJSONRequest(t *testing.T, method, target string, body interface{}) *http.Request {
	return authedRequest(t, method, target, body)
}

// authedMultipartRequest builds a multipart/form-data PUT with kind=file and
// a file part named "file".
func authedMultipartRequest(t *testing.T, target string, fileBytes []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("kind", "file"); err != nil {
		t.Fatalf("writing kind field: %v", err)
	}
	fw, err := mw.CreateFormFile("file", "payload.bin")
	if err != nil {
		t.Fatalf("creating file part: %v", err)
	}
	if _, err := fw.Write(fileBytes); err != nil {
		t.Fatalf("writing file bytes: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, target, &buf)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func getSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	sec := &corev1.Secret{}
	if err := c.Get(t.Context(), types.NamespacedName{Name: name, Namespace: "kyber-system"}, sec); err != nil {
		t.Fatalf("getting secret %s: %v", name, err)
	}
	return sec
}

func getAgent(t *testing.T, c client.Client, name string) *kyberv1.Agent {
	t.Helper()
	a := &kyberv1.Agent{}
	if err := c.Get(t.Context(), types.NamespacedName{Name: name, Namespace: "kyber-system"}, a); err != nil {
		t.Fatalf("getting agent %s: %v", name, err)
	}
	return a
}

func TestUserSecrets_PutKV_HappyPath(t *testing.T) {
	h, c := buildUserSecretsHandler(t, "dave")

	req := authedJSONRequest(t, http.MethodPut, "/api/v1/agents/dave/secrets/FOO", map[string]string{
		"kind":  "kv",
		"value": "bar",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rr.Code, rr.Body.String())
	}

	sec := getSecret(t, c, "dave-user-secrets-kv")
	if got, want := string(sec.Data["FOO"]), "bar"; got != want {
		t.Errorf("kv Data[FOO]: got %q, want %q", got, want)
	}
	if sec.Annotations["kyber.io/user-secrets-metadata"] == "" {
		t.Error("metadata annotation missing")
	}
	var meta map[string]map[string]string
	_ = json.Unmarshal([]byte(sec.Annotations["kyber.io/user-secrets-metadata"]), &meta)
	if meta["FOO"]["sha256Prefix"] == "" {
		t.Error("sha256Prefix not recorded")
	}
	if meta["FOO"]["createdAt"] == "" || meta["FOO"]["updatedAt"] == "" {
		t.Error("createdAt/updatedAt not recorded")
	}

	agent := getAgent(t, c, "dave")
	if agent.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
		t.Errorf("DesiredPhase: got %q, want Restarting (kyber#515: a Running agent must roll)", agent.Spec.DesiredPhase)
	}
}

func TestUserSecrets_PutFile_HappyPath(t *testing.T) {
	h, c := buildUserSecretsHandler(t, "dave")

	payload := []byte("-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----\n")
	req := authedMultipartRequest(t, "/api/v1/agents/dave/secrets/APP_PEM", payload)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rr.Code, rr.Body.String())
	}

	sec := getSecret(t, c, "dave-user-secrets-files")
	if got := sec.Data["app_pem.bin"]; !bytes.Equal(got, payload) {
		t.Errorf("files Data[app_pem.bin]: got %q, want %q", got, payload)
	}
	agent := getAgent(t, c, "dave")
	// A file-mode write must NOT roll: file-mode lands live via the #516
	// bind-mount, so no pod recreate is needed. Rolling on every file write was
	// the bug that restarted an entire agent team every 20 min when a minter
	// rotated its file-mode issue token (a production incident).
	if agent.Spec.DesiredPhase != "" {
		t.Errorf("DesiredPhase: got %q, want empty — a file-mode write must NOT roll the agent", agent.Spec.DesiredPhase)
	}
}

func TestUserSecrets_PutReplacesAcrossKinds(t *testing.T) {
	h, c := buildUserSecretsHandler(t, "dave")

	// First: PUT FOO as kv.
	req := authedJSONRequest(t, http.MethodPut, "/api/v1/agents/dave/secrets/FOO", map[string]string{
		"kind": "kv", "value": "kv-value",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("first PUT want 204, got %d: %s", rr.Code, rr.Body.String())
	}

	// Second: PUT FOO as file — must move it out of kv into files.
	req = authedMultipartRequest(t, "/api/v1/agents/dave/secrets/FOO", []byte("binary-content"))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("second PUT want 204, got %d: %s", rr.Code, rr.Body.String())
	}

	kvSec := getSecret(t, c, "dave-user-secrets-kv")
	if _, ok := kvSec.Data["FOO"]; ok {
		t.Error("kv Data[FOO] should have been removed after file PUT")
	}
	fileSec := getSecret(t, c, "dave-user-secrets-files")
	if got := fileSec.Data["foo.bin"]; !bytes.Equal(got, []byte("binary-content")) {
		t.Errorf("files Data[foo.bin]: got %q, want %q", got, "binary-content")
	}
}

func TestUserSecrets_List_ReturnsMetadataNoValues(t *testing.T) {
	h, c := buildUserSecretsHandler(t, "dave")

	// Seed one kv and one file via PUT so metadata is populated.
	put := func(target, kind, value string) {
		var req *http.Request
		if kind == "kv" {
			req = authedJSONRequest(t, http.MethodPut, target, map[string]string{"kind": "kv", "value": value})
		} else {
			req = authedMultipartRequest(t, target, []byte(value))
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("PUT %s want 204, got %d: %s", target, rr.Code, rr.Body.String())
		}
	}
	put("/api/v1/agents/dave/secrets/FOO", "kv", "bar")
	put("/api/v1/agents/dave/secrets/APP_PEM", "file", "pem-bytes")

	// Confirm file Data is stored under the transformed key.
	fileSec := getSecret(t, c, "dave-user-secrets-files")
	if _, ok := fileSec.Data["app_pem.bin"]; !ok {
		t.Errorf("expected files Data[app_pem.bin], keys=%v", keys(fileSec.Data))
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/dave/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Items []struct {
			Key          string `json:"key"`
			Kind         string `json:"kind"`
			Size         int    `json:"size"`
			Sha256Prefix string `json:"sha256Prefix"`
		} `json:"items"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items: got %d, want 2 (%+v)", len(resp.Items), resp.Items)
	}
	// Sorted: APP_PEM (file) before FOO (kv).
	if resp.Items[0].Key != "APP_PEM" || resp.Items[0].Kind != "file" {
		t.Errorf("item[0]: got %+v, want APP_PEM/file", resp.Items[0])
	}
	if resp.Items[1].Key != "FOO" || resp.Items[1].Kind != "kv" {
		t.Errorf("item[1]: got %+v, want FOO/kv", resp.Items[1])
	}
	if resp.Items[0].Size != len("pem-bytes") || resp.Items[1].Size != len("bar") {
		t.Errorf("sizes: got %d/%d, want %d/%d", resp.Items[0].Size, resp.Items[1].Size, len("pem-bytes"), len("bar"))
	}
	// Values must never appear in the list payload — defense-in-depth grep.
	if strings.Contains(rr.Body.String(), "bar") || strings.Contains(rr.Body.String(), "pem-bytes") {
		t.Errorf("list payload leaked a value: %s", rr.Body.String())
	}
}

func TestUserSecrets_GetReadback_KV(t *testing.T) {
	h, _ := buildUserSecretsHandler(t, "dave")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedJSONRequest(t, http.MethodPut, "/api/v1/agents/dave/secrets/FOO", map[string]string{
		"kind": "kv", "value": "hello-world",
	}))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("PUT: got %d: %s", rr.Code, rr.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/dave/secrets/FOO", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type: got %q, want text/plain", ct)
	}
	if rr.Body.String() != "hello-world" {
		t.Errorf("body: got %q, want %q", rr.Body.String(), "hello-world")
	}
}

func TestUserSecrets_GetReadback_File(t *testing.T) {
	h, _ := buildUserSecretsHandler(t, "dave")

	payload := []byte{0x00, 0x01, 0x02, 0x03, 0xff}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedMultipartRequest(t, "/api/v1/agents/dave/secrets/APP_PEM", payload))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("PUT: got %d: %s", rr.Code, rr.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/dave/secrets/APP_PEM", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type: got %q, want application/octet-stream", ct)
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "app_pem.bin") {
		t.Errorf("Content-Disposition: got %q, want to contain app_pem.bin", cd)
	}
	if !bytes.Equal(rr.Body.Bytes(), payload) {
		t.Errorf("body bytes mismatch: got %v, want %v", rr.Body.Bytes(), payload)
	}
}

func TestUserSecrets_Get_NotSet_Returns404(t *testing.T) {
	h, _ := buildUserSecretsHandler(t, "dave")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/dave/secrets/MISSING", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUserSecrets_Delete_RemovesAndRolls(t *testing.T) {
	h, c := buildUserSecretsHandler(t, "dave")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedJSONRequest(t, http.MethodPut, "/api/v1/agents/dave/secrets/FOO", map[string]string{
		"kind": "kv", "value": "bar",
	}))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("seed PUT: got %d", rr.Code)
	}
	// Reset agent.Spec.DesiredPhase so we can verify DELETE re-patches it.
	a := getAgent(t, c, "dave")
	a.Spec.DesiredPhase = ""
	if err := c.Update(t.Context(), a); err != nil {
		t.Fatalf("resetting desiredPhase: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dave/secrets/FOO", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE want 204, got %d: %s", rr.Code, rr.Body.String())
	}

	sec := getSecret(t, c, "dave-user-secrets-kv")
	if _, ok := sec.Data["FOO"]; ok {
		t.Error("kv Data[FOO] should have been deleted")
	}
	a = getAgent(t, c, "dave")
	if a.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
		t.Errorf("DesiredPhase after DELETE: got %q, want Restarting (kyber#515: a Running agent must roll)", a.Spec.DesiredPhase)
	}
}

// TestUserSecrets_PutKV_RollsRunningAgent is the kyber#515 regression test:
// updating a kv-mode user-secret on an already-Running agent must produce a
// spec mutation that recreates the pod, so the new value (projected as envFrom
// at pod boot, pod_builder.go) is picked up. The old code set
// DesiredPhase=Running — a no-op merge-patch for an already-Running agent — so
// the pod was never recreated and the value stayed stale. The fix drives the
// standard graceful roll (DesiredPhase=Restarting) used by setModel/setResources.
func TestUserSecrets_PutKV_RollsRunningAgent(t *testing.T) {
	h, c := buildUserSecretsHandlerWithPhase(t, "dave", kyberv1.AgentPhaseRunning)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedJSONRequest(t, http.MethodPut, "/api/v1/agents/dave/secrets/FOO", map[string]string{
		"kind": "kv", "value": "bar",
	}))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rr.Code, rr.Body.String())
	}

	a := getAgent(t, c, "dave")
	// Restarting is the only DesiredPhase that recreates a Running agent's pod
	// (Running→Restarting→…→Starting via the state machine). DesiredPhase=Running
	// on an already-Running agent is the no-op this bug was.
	if a.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
		t.Errorf("DesiredPhase: got %q, want Restarting — a Running agent must be rolled to re-project the secret, not left a no-op", a.Spec.DesiredPhase)
	}
}

// TestUserSecrets_PutKV_DormantAgentNotRolled is the non-regression guard:
// updating a secret on a non-Running (Stopped) agent must NOT change its
// lifecycle — no error, secret written, and DesiredPhase left untouched so the
// update doesn't wake a dormant agent. The new value lands via envFrom on its
// next start.
func TestUserSecrets_PutKV_DormantAgentNotRolled(t *testing.T) {
	h, c := buildUserSecretsHandlerWithPhase(t, "dave", kyberv1.AgentPhaseStopped)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedJSONRequest(t, http.MethodPut, "/api/v1/agents/dave/secrets/FOO", map[string]string{
		"kind": "kv", "value": "bar",
	}))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rr.Code, rr.Body.String())
	}

	// Secret is written regardless of phase.
	if got := string(getSecret(t, c, "dave-user-secrets-kv").Data["FOO"]); got != "bar" {
		t.Errorf("kv Data[FOO]: got %q, want %q", got, "bar")
	}
	// Lifecycle untouched: a secret update must not wake a Stopped agent.
	if dp := getAgent(t, c, "dave").Spec.DesiredPhase; dp != "" {
		t.Errorf("DesiredPhase on Stopped agent: got %q, want unchanged (\"\") — a secret update must not wake a dormant agent", dp)
	}
}

func TestUserSecrets_Delete_NotSet_Returns404(t *testing.T) {
	h, _ := buildUserSecretsHandler(t, "dave")
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dave/secrets/MISSING", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUserSecrets_Validation_BadGrammar(t *testing.T) {
	h, _ := buildUserSecretsHandler(t, "dave")
	// Keys that pass URL parsing but fail the grammar check.
	cases := []string{"foo", "1FOO", "FOO-BAR"}
	for _, key := range cases {
		t.Run("key="+key, func(t *testing.T) {
			req := authedJSONRequest(t, http.MethodPut, "/api/v1/agents/dave/secrets/"+key, map[string]string{
				"kind": "kv", "value": "x",
			})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("want 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "invalid_key") {
				t.Errorf("error code: got %s, want invalid_key", rr.Body.String())
			}
		})
	}
}

func TestUserSecrets_Validation_ReservedPrefix(t *testing.T) {
	h, _ := buildUserSecretsHandler(t, "dave")
	for _, key := range []string{"USER_FOO", "KYBER_FOO"} {
		t.Run("key="+key, func(t *testing.T) {
			req := authedJSONRequest(t, http.MethodPut, "/api/v1/agents/dave/secrets/"+key, map[string]string{
				"kind": "kv", "value": "x",
			})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "reserved_prefix") {
				t.Errorf("error code: got %s, want reserved_prefix", rr.Body.String())
			}
		})
	}
}

func TestUserSecrets_Validation_ValueTooLarge(t *testing.T) {
	h, _ := buildUserSecretsHandler(t, "dave")
	big := strings.Repeat("x", 64*1024+1)
	req := authedJSONRequest(t, http.MethodPut, "/api/v1/agents/dave/secrets/FOO", map[string]string{
		"kind": "kv", "value": big,
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "value_too_large") {
		t.Errorf("error code: got %s, want value_too_large", rr.Body.String())
	}
}

func TestUserSecrets_Validation_AggregateTooLarge(t *testing.T) {
	h, _ := buildUserSecretsHandler(t, "dave")

	// Four 64 KiB entries fit exactly at 256 KiB; a fifth would blow the aggregate.
	payload := strings.Repeat("x", 64*1024)
	for i, key := range []string{"A1", "A2", "A3", "A4"} {
		req := authedJSONRequest(t, http.MethodPut, "/api/v1/agents/dave/secrets/"+key, map[string]string{
			"kind": "kv", "value": payload,
		})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("seed PUT %d (%s): got %d: %s", i, key, rr.Code, rr.Body.String())
		}
	}
	// Fifth write must be rejected by aggregate check.
	req := authedJSONRequest(t, http.MethodPut, "/api/v1/agents/dave/secrets/A5", map[string]string{
		"kind": "kv", "value": "just one byte over",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "aggregate_too_large") {
		t.Errorf("error code: got %s, want aggregate_too_large", rr.Body.String())
	}
}

// TestUserSecrets_AggregateOverflow_CrossKind_PreservesExisting covers the
// regression fixed during self-review: an aggregate-overflow cross-kind PUT
// must be rejected *before* we drop the existing entry from the other kind.
// Otherwise the 400 response would silently delete the user's prior secret.
//
// Setup layers kv entries + an existing file FOO so that the aggregate is
// already tight. Then we attempt a file→kv move for FOO with a value big
// enough that the new-total (kv filler + new kv FOO) overflows. Entry size
// stays under the per-entry limit so entry-size validation doesn't intercept.
func TestUserSecrets_AggregateOverflow_CrossKind_PreservesExisting(t *testing.T) {
	h, c := buildUserSecretsHandler(t, "dave")

	putKV := func(key, value string) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, authedJSONRequest(t, http.MethodPut, "/api/v1/agents/dave/secrets/"+key, map[string]string{
			"kind": "kv", "value": value,
		}))
		if rr.Code != http.StatusNoContent {
			t.Fatalf("seed kv PUT %s: got %d: %s", key, rr.Code, rr.Body.String())
		}
	}
	// 4 × 50 KiB kv filler = 200 KiB.
	for _, k := range []string{"A1", "A2", "A3", "A4"} {
		putKV(k, strings.Repeat("x", 50*1024))
	}
	// FOO as file, 50 KiB → aggregate 250 KiB.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedMultipartRequest(t, "/api/v1/agents/dave/secrets/FOO", []byte(strings.Repeat("f", 50*1024))))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("seed file PUT FOO: got %d: %s", rr.Code, rr.Body.String())
	}

	// Now attempt a file→kv cross-kind move of FOO with 57 KiB. The old FOO
	// (50 KiB file) is excluded from the aggregate calc, so the projected
	// total is 200 KiB (A1–A4) + 57 KiB (new kv FOO) = 257 KiB → 400.
	// 57 KiB is under the 64 KiB per-entry cap so entry-size passes.
	req := authedJSONRequest(t, http.MethodPut, "/api/v1/agents/dave/secrets/FOO", map[string]string{
		"kind":  "kv",
		"value": strings.Repeat("n", 57*1024),
	})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400 on aggregate-overflow cross-kind PUT, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "aggregate_too_large") {
		t.Errorf("error code: got %s, want aggregate_too_large", rr.Body.String())
	}

	// The existing file FOO must still be present — that's the regression.
	fileSec := getSecret(t, c, "dave-user-secrets-files")
	if _, ok := fileSec.Data["foo.bin"]; !ok {
		t.Fatal("cross-kind PUT that failed aggregate validation dropped the existing file FOO entry")
	}
}

func TestUserSecrets_UnknownAgent_Returns404(t *testing.T) {
	h, _ := buildUserSecretsHandler(t, "dave")

	cases := []struct {
		method, target string
		body           map[string]string
	}{
		{http.MethodGet, "/api/v1/agents/ghost/secrets", nil},
		{http.MethodGet, "/api/v1/agents/ghost/secrets/FOO", nil},
		{http.MethodPut, "/api/v1/agents/ghost/secrets/FOO", map[string]string{"kind": "kv", "value": "x"}},
		{http.MethodDelete, "/api/v1/agents/ghost/secrets/FOO", nil},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			var req *http.Request
			if tc.body != nil {
				req = authedJSONRequest(t, tc.method, tc.target, tc.body)
			} else {
				req = httptest.NewRequest(tc.method, tc.target, nil)
				req.Header.Set("Authorization", "Bearer "+testAPIKey)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusNotFound {
				t.Errorf("want 404, got %d: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestUserSecrets_Put_UnsupportedContentType(t *testing.T) {
	h, _ := buildUserSecretsHandler(t, "dave")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/agents/dave/secrets/FOO", strings.NewReader("raw"))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
