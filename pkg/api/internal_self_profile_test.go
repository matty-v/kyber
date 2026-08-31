package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/podtoken"
	"github.com/matty-v/kyber/pkg/skillscan"
	"github.com/matty-v/kyber/pkg/skillstore"
)

func TestInternalSelfProfile_IsSelfOnlyAndSanitized(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "glyph", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Machine: "private-machine", Runtime: "codex", Model: "gpt-test",
			Resources: kyberv1.AgentResources{
				CPU: resource.MustParse("2"), Memory: resource.MustParse("8Gi"), Disk: resource.MustParse("20Gi"),
			},
			IdentityRepo: kyberv1.AgentIdentityRepo{Repo: "private/repo"},
		},
		Status: kyberv1.AgentStatus{
			Phase: kyberv1.AgentPhaseRunning, PodName: "private-pod", PodIP: "10.0.0.8", NodeName: "private-node",
			CurrentModel: "gpt-live", Runtime: kyberv1.AgentRuntimeStatus{InstalledVersion: "0.1.2"},
		},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	skills := skillstore.NewMemoryStore()
	if err := skills.Put(context.Background(), "glyph", &skillscan.Report{Version: skillscan.ReportVersion, Skills: []skillscan.Skill{
		{Name: "glyph-about", Description: "Describe Glyph's public profile."},
	}}); err != nil {
		t.Fatal(err)
	}
	server := api.NewInternalServer(
		briefstore.NewMemoryStore(),
		api.WithKubeClient(kube, "kyber-system"),
		api.WithSkillStore(skills),
		api.WithInternalAuth(api.NewHMACInternalAuthenticator(testSigningKey), false),
	)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	wrong := do(t, ts, http.MethodGet, "/internal/agents/glyph/self-profile", podtoken.Sign("other", testSigningKey), "")
	wrong.Body.Close()
	if wrong.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-agent status = %d, want 403", wrong.StatusCode)
	}

	resp := do(t, ts, http.MethodGet, "/internal/agents/glyph/self-profile", podtoken.Sign("glyph", testSigningKey), "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, forbidden := range []string{"private-machine", "private-pod", "10.0.0.8", "private-node", "private/repo"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, body)
		}
	}
	var profile api.SelfProfile
	if err := json.Unmarshal(body, &profile); err != nil {
		t.Fatal(err)
	}
	if profile.Name != "glyph" || profile.Runtime != "codex" || profile.Model != "gpt-live" || profile.Resources.Memory != "8Gi" {
		t.Fatalf("profile = %+v", profile)
	}
	if !profile.SkillsReported || len(profile.Skills) != 1 || profile.Skills[0].Name != "glyph-about" {
		t.Fatalf("skills = %+v", profile.Skills)
	}
}

func TestInternalSelfProfile_DistinguishesUnreportedSkills(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "new-agent", Namespace: "kyber-system"}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	server := api.NewInternalServer(
		briefstore.NewMemoryStore(),
		api.WithKubeClient(kube, "kyber-system"),
		api.WithSkillStore(skillstore.NewMemoryStore()),
	)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/internal/agents/new-agent/self-profile")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var profile api.SelfProfile
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		t.Fatal(err)
	}
	if profile.SkillsReported || profile.Skills == nil || len(profile.Skills) != 0 {
		t.Fatalf("unreported skills = reported:%v skills:%+v", profile.SkillsReported, profile.Skills)
	}
}
