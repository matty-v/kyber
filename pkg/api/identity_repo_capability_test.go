package api_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/githubapp"
)

// newIdentityTestGithubClient builds a real GitHub App client. Constructing it
// makes no network call; these tests only need it to be non-nil, which is what
// "the App is configured" means to the control plane.
func newIdentityTestGithubClient(t *testing.T) *githubapp.Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	c, err := githubapp.NewClient(githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: key})
	if err != nil {
		t.Fatalf("githubapp.NewClient: %v", err)
	}
	return c
}

type identityInstall struct {
	name  string
	app   bool
	owner string
}

var identityInstalls = []identityInstall{
	{name: "app and owner", app: true, owner: "acme"},
	{name: "app without owner", app: true},
	{name: "owner without app", owner: "acme"},
	{name: "neither"},
}

func newIdentityTestServer(t *testing.T, in identityInstall) (*api.Server, client.Client) {
	t.Helper()
	fakeClient := fake.NewClientBuilder().
		WithScheme(mustNewScheme(t)).
		WithRuntimeObjects(defaultMachine()).
		Build()
	s := &api.Server{
		K8sClient:         fakeClient,
		APIKey:            testAPIKey,
		Namespace:         "kyber-system",
		ValidRuntimes:     map[string]bool{"claude-code": true},
		IdentityRepoOwner: in.owner,
	}
	if in.app {
		s.GithubAppClient = newIdentityTestGithubClient(t)
	}
	return s, fakeClient
}

func TestConfig_IdentityCapability(t *testing.T) {
	for _, in := range identityInstalls {
		t.Run(in.name, func(t *testing.T) {
			s, _ := newIdentityTestServer(t, in)
			rr := httptest.NewRecorder()
			s.BuildHandler().ServeHTTP(rr, authedRequest(t, http.MethodGet, "/api/v1/config", nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
			}
			var got api.ConfigResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			id := got.Identity
			if id.RepoOwner != in.owner {
				t.Errorf("repoOwner = %q, want %q", id.RepoOwner, in.owner)
			}
			available := in.app && in.owner != ""
			if id.ManagedReposAvailable != available {
				t.Errorf("managedReposAvailable = %v, want %v", id.ManagedReposAvailable, available)
			}
			wantModes := []string{"none"}
			if available {
				wantModes = []string{"template", "existing", "none"}
			}
			if !reflect.DeepEqual(id.SupportedModes, wantModes) {
				t.Errorf("supportedModes = %v, want %v", id.SupportedModes, wantModes)
			}
			if available && id.UnavailableReason != "" {
				t.Errorf("unavailableReason = %q, want empty", id.UnavailableReason)
			}
			if !available {
				want := "GitHub App is not configured"
				if in.app {
					want = "identityRepo.defaultOwner"
				}
				if !strings.Contains(id.UnavailableReason, want) {
					t.Errorf("unavailableReason = %q, want it to mention %q", id.UnavailableReason, want)
				}
			}
		})
	}
}

func createAgentWithIdentityRepo(t *testing.T, s *api.Server, identityRepo map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]interface{}{
		"name":    "ivy",
		"machine": "worker-1",
		"runtime": "claude-code",
		"model":   "claude-sonnet-4",
		"resources": map[string]interface{}{
			"cpu": "1", "memory": "2Gi", "disk": "50Gi",
		},
		"secrets": map[string]interface{}{
			"authType":               "oauth",
			"telegramEnabled":        true,
			"telegramBotToken":       "123:abc",
			"telegramAllowedUserIds": []string{"42"},
		},
	}
	if identityRepo != nil {
		body["identityRepo"] = identityRepo
	}
	rr := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(rr, authedRequest(t, http.MethodPost, "/api/v1/agents", body))
	return rr
}

// assertNothingPersisted checks a rejected create left no Agent, Secret or PVC
// behind. The request enables Telegram so a late validation would have written
// a Secret for this to catch.
func assertNothingPersisted(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	var agents kyberv1.AgentList
	if err := c.List(ctx, &agents); err != nil {
		t.Fatalf("listing agents: %v", err)
	}
	var secrets corev1.SecretList
	if err := c.List(ctx, &secrets); err != nil {
		t.Fatalf("listing secrets: %v", err)
	}
	var pvcs corev1.PersistentVolumeClaimList
	if err := c.List(ctx, &pvcs); err != nil {
		t.Fatalf("listing pvcs: %v", err)
	}
	if n := len(agents.Items) + len(secrets.Items) + len(pvcs.Items); n != 0 {
		t.Errorf("rejected create persisted %d agents, %d secrets, %d pvcs; want none",
			len(agents.Items), len(secrets.Items), len(pvcs.Items))
	}
}

func assertIdentityRepoValidationError(t *testing.T, rr *httptest.ResponseRecorder, wantInMessage string) {
	t.Helper()
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	var resp api.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error.Code != "VALIDATION_ERROR" || resp.Error.Field != "identityRepo" {
		t.Errorf("error = %s/%s, want VALIDATION_ERROR/identityRepo", resp.Error.Code, resp.Error.Field)
	}
	if !strings.Contains(resp.Error.Message, wantInMessage) {
		t.Errorf("message = %q, want it to mention %q", resp.Error.Message, wantInMessage)
	}
}

func TestAgents_Create_IdentityRepoModesFollowCapability(t *testing.T) {
	cases := []struct {
		mode         string
		identityRepo map[string]interface{}
	}{
		{mode: "none"},
		{mode: "template", identityRepo: map[string]interface{}{"template": "acme/kyber-agent-template"}},
		{mode: "existing", identityRepo: map[string]interface{}{"repo": "acme/ivy-agent"}},
	}
	for _, in := range identityInstalls {
		for _, tc := range cases {
			t.Run(in.name+"/"+tc.mode, func(t *testing.T) {
				s, c := newIdentityTestServer(t, in)
				rr := createAgentWithIdentityRepo(t, s, tc.identityRepo)
				if tc.mode == "none" || (in.app && in.owner != "") {
					if rr.Code != http.StatusCreated {
						t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
					}
					return
				}
				want := "GitHub App is not configured"
				if in.app {
					want = "identityRepo.defaultOwner"
				}
				assertIdentityRepoValidationError(t, rr, want)
				assertNothingPersisted(t, c)
			})
		}
	}
}

func TestAgents_Create_IdentityRepoRejectsRepoAndTemplateTogether(t *testing.T) {
	s, c := newIdentityTestServer(t, identityInstalls[0])
	rr := createAgentWithIdentityRepo(t, s, map[string]interface{}{
		"repo":     "acme/ivy-agent",
		"template": "acme/kyber-agent-template",
	})
	assertIdentityRepoValidationError(t, rr, "mutually exclusive")
	assertNothingPersisted(t, c)
}

func TestAgents_Create_RepoLessAgentHasEmptyIdentityRepo(t *testing.T) {
	s, c := newIdentityTestServer(t, identityInstall{name: "neither"})
	rr := createAgentWithIdentityRepo(t, s, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	got := &kyberv1.Agent{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ivy", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("fetching created agent: %v", err)
	}
	if got.Spec.IdentityRepo != (kyberv1.AgentIdentityRepo{}) {
		t.Errorf("spec.identityRepo = %+v, want empty", got.Spec.IdentityRepo)
	}
}
