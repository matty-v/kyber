package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/skillscan"
	"github.com/matty-v/kyber/pkg/skillstore"
)

// postSkills drives the in-pod reporting path: POST to the internal API, the
// same call the status sidecar forwards.
func postSkills(t *testing.T, srv *api.InternalServer, agent, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/agents/"+agent+"/skills", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

const oneHealthySkill = `{
  "version": 1,
  "skills": [
    {"name":"restart","description":"Planned shutdown.","source":"identity","path":"skills/restart","linked":["claude-code","codex"]}
  ]
}`

func TestSkillsReport_StoresAndServesBack(t *testing.T) {
	store := skillstore.NewMemoryStore()
	internal := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithSkillStore(store))
	if rr := postSkills(t, internal, "dave", oneHealthySkill); rr.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	public := &api.Server{K8sClient: nil, APIKey: testAPIKey, Namespace: "kyber-system", SkillStore: store}
	rr := httptest.NewRecorder()
	public.BuildHandler().ServeHTTP(rr, authedRequest(t, http.MethodGet, "/api/v1/agents/dave/skills", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got api.SkillsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Agent != "dave" || len(got.Skills) != 1 || got.Skills[0].Name != "restart" {
		t.Fatalf("response = %+v", got)
	}
	if got.Summary.Total != 1 || got.Summary.Healthy != 1 || got.Summary.Broken != 0 {
		t.Errorf("summary = %+v", got.Summary)
	}
	// ReportedAt is stamped by the control plane, not taken from the pod,
	// so the "as of" the operator reads can't be thrown off by pod clock skew.
	if got.ReportedAt == "" {
		t.Error("expected the control plane to stamp reportedAt")
	}
}

// The reason the feature exists: a skill that is committed and present but
// loadable by nothing must read as broken, not as fine.
func TestSkillsReport_UnloadableSkillCountsAsBroken(t *testing.T) {
	store := skillstore.NewMemoryStore()
	internal := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithSkillStore(store))
	body := `{"version":1,"skills":[
	  {"name":"restart","source":"identity","path":"skills/restart","linked":[],
	   "issues":[{"code":"not_linked","severity":"error","detail":"not loadable by claude-code"}]}],
	 "issues":[{"code":"unmanaged","severity":"warning","detail":"stray dir"}]}`
	if rr := postSkills(t, internal, "dave", body); rr.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d; body=%s", rr.Code, rr.Body.String())
	}

	public := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SkillStore: store}
	rr := httptest.NewRecorder()
	public.BuildHandler().ServeHTTP(rr, authedRequest(t, http.MethodGet, "/api/v1/agents/dave/skills", nil))
	var got api.SkillsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Summary.Healthy != 0 || got.Summary.Broken != 1 {
		t.Errorf("summary = %+v, want 0 healthy / 1 broken", got.Summary)
	}
	if got.Summary.OtherIssues != 1 {
		t.Errorf("otherIssues = %d, want 1", got.Summary.OtherIssues)
	}
}

// "Never reported" and "reported zero skills" are different facts and must not
// render the same, so the first is a 404 rather than an empty list.
func TestSkillsGet_NeverReportedIs404(t *testing.T) {
	public := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SkillStore: skillstore.NewMemoryStore()}
	rr := httptest.NewRecorder()
	public.BuildHandler().ServeHTTP(rr, authedRequest(t, http.MethodGet, "/api/v1/agents/ghost/skills", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestSkillsGet_EmptyReportServesEmptyListNotNull(t *testing.T) {
	store := skillstore.NewMemoryStore()
	internal := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithSkillStore(store))
	if rr := postSkills(t, internal, "dave", `{"version":1,"skills":[]}`); rr.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d; body=%s", rr.Code, rr.Body.String())
	}
	public := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SkillStore: store}
	rr := httptest.NewRecorder()
	public.BuildHandler().ServeHTTP(rr, authedRequest(t, http.MethodGet, "/api/v1/agents/dave/skills", nil))
	if body := rr.Body.String(); !bytes.Contains([]byte(body), []byte(`"skills":[]`)) ||
		!bytes.Contains([]byte(body), []byte(`"issues":[]`)) {
		t.Fatalf("expected empty arrays rather than null; body=%s", body)
	}
}

// The API is read-only on purpose: skills are managed by talking to the agent.
// A write here would let an operator put the repo and the pod out of sync,
// which is precisely the state this surface exists to expose.
func TestSkillsGet_IsReadOnly(t *testing.T) {
	public := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SkillStore: skillstore.NewMemoryStore()}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rr := httptest.NewRecorder()
		public.BuildHandler().ServeHTTP(rr, authedRequest(t, method, "/api/v1/agents/dave/skills", map[string]any{}))
		if rr.Code == http.StatusOK || rr.Code == http.StatusNoContent || rr.Code == http.StatusCreated {
			t.Errorf("%s /skills returned %d — the skills API must not accept writes", method, rr.Code)
		}
	}
}

func TestSkillsGet_NoStoreConfiguredIs503(t *testing.T) {
	public := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system"}
	rr := httptest.NewRecorder()
	public.BuildHandler().ServeHTTP(rr, authedRequest(t, http.MethodGet, "/api/v1/agents/dave/skills", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestSkillsReport_Rejections(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"unknown version", `{"version":99,"skills":[]}`},
		{"not json", `{`},
		{"empty skill name", `{"version":1,"skills":[{"name":"  ","source":"identity","path":"skills/x"}]}`},
		{"unknown source", `{"version":1,"skills":[{"name":"x","source":"somewhere-else","path":"skills/x"}]}`},
		{"unknown linked runtime", `{"version":1,"skills":[{"name":"x","source":"identity","path":"skills/x","linked":["emacs"]}]}`},
		{"empty issue code", `{"version":1,"skills":[],"issues":[{"code":"","severity":"error","detail":"d"}]}`},
		{"unknown issue severity", `{"version":1,"skills":[],"issues":[{"code":"unmanaged","severity":"catastrophic","detail":"d"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := skillstore.NewMemoryStore()
			internal := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithSkillStore(store))
			if rr := postSkills(t, internal, "dave", tc.body); rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
			if _, err := store.Get(context.Background(), "dave"); err == nil {
				t.Error("a rejected report must not be stored")
			}
		})
	}
}

func TestSkillsReport_TooManySkillsRejected(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"version":1,"skills":[`)
	for i := 0; i < 201; i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`{"name":"s","source":"identity","path":"skills/s"}`)
	}
	buf.WriteString(`]}`)

	internal := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithSkillStore(skillstore.NewMemoryStore()))
	if rr := postSkills(t, internal, "dave", buf.String()); rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestSkillsReport_PlatformSourceAccepted(t *testing.T) {
	store := skillstore.NewMemoryStore()
	internal := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithSkillStore(store))
	body := `{"version":1,"skills":[{"name":"telegram-messaging","source":"platform","path":"/opt/kyber/skills/telegram-messaging","linked":["claude-code"]}]}`
	if rr := postSkills(t, internal, "dave", body); rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	rep, err := store.Get(context.Background(), "dave")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Skills) != 1 || rep.Skills[0].Source != skillscan.SourcePlatform {
		t.Fatalf("stored = %+v", rep.Skills)
	}
}

func TestSkillsReport_NoStoreConfiguredIs503(t *testing.T) {
	internal := api.NewInternalServer(briefstore.NewMemoryStore())
	if rr := postSkills(t, internal, "dave", oneHealthySkill); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

// A skill that works but is missing its description is a warning, not a
// failure. Counting it as broken would put a red number on a healthy fleet and
// train the operator to ignore it.
func TestSkillsGet_WarningSkillIsNotCountedAsBroken(t *testing.T) {
	store := skillstore.NewMemoryStore()
	internal := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithSkillStore(store))
	body := `{"version":1,"skills":[
	  {"name":"restart","source":"identity","path":"skills/restart","linked":["claude-code","codex"],
	   "issues":[{"code":"missing_description","severity":"warning","detail":"no description"}]}]}`
	if rr := postSkills(t, internal, "dave", body); rr.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d; body=%s", rr.Code, rr.Body.String())
	}
	public := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SkillStore: store}
	rr := httptest.NewRecorder()
	public.BuildHandler().ServeHTTP(rr, authedRequest(t, http.MethodGet, "/api/v1/agents/dave/skills", nil))
	var got api.SkillsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Summary.Broken != 0 || got.Summary.Warnings != 1 || got.Summary.Healthy != 0 {
		t.Errorf("summary = %+v, want 0 broken / 1 warning / 0 healthy", got.Summary)
	}
}

// Runtime images are pinned per cluster, so a control plane routinely runs
// ahead of the images reporting into it. A reporter that predates severities
// must degrade to "error", not have its whole report rejected.
func TestSkillsReport_MissingSeverityDefaultsToError(t *testing.T) {
	store := skillstore.NewMemoryStore()
	internal := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithSkillStore(store))
	body := `{"version":1,"skills":[{"name":"x","source":"identity","path":"skills/x","linked":[],
	  "issues":[{"code":"not_linked","detail":"no severity from an older image"}]}]}`
	if rr := postSkills(t, internal, "dave", body); rr.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	rep, err := store.Get(context.Background(), "dave")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skills[0].Issues[0].Severity != skillscan.SeverityError {
		t.Errorf("severity = %q, want %q", rep.Skills[0].Issues[0].Severity, skillscan.SeverityError)
	}
}

func TestSkillsReport_MethodNotAllowed(t *testing.T) {
	internal := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithSkillStore(skillstore.NewMemoryStore()))
	req := httptest.NewRequest(http.MethodGet, "/internal/agents/dave/skills", nil)
	rr := httptest.NewRecorder()
	internal.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestSkillsReportTriggersCapabilityReconcile(t *testing.T) {
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "kyber-system", UID: types.UID("agent-dave")}, Spec: kyberv1.AgentSpec{PublicCapabilities: &kyberv1.AgentPublicCapabilities{SchemaVersion: "v1alpha1", Identity: kyberv1.AgentPublicCapabilityIdentity{DisplayName: "Dave", Description: "A test agent."}}}}
	client := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent).Build()
	internal := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithSkillStore(skillstore.NewMemoryStore()), api.WithKubeClient(client, "kyber-system"))
	if rr := postSkills(t, internal, "dave", oneHealthySkill); rr.Code != http.StatusNoContent {
		t.Fatalf("POST=%d %s", rr.Code, rr.Body.String())
	}
	stored := &kyberv1.Agent{}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "kyber-system", Name: "dave"}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Annotations["kyber.io/capability-evidence-reported-at"] == "" {
		t.Fatal("evidence refresh annotation was not written")
	}
}
