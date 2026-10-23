package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const sensitiveFixtureKey = "sensitive-integration-fixture"

func sensitiveFixtureCaller(scopes ...string) api.ScopedCaller {
	return api.ScopedCaller{Name: "integration", Key: sensitiveFixtureKey, PrincipalID: "principal_sensitive_fixture", TenantID: "tenant_fixture", CredentialID: "credential_sensitive_fixture", CredentialGeneration: 1, Scopes: scopes, AgentResources: []string{"kyber-system/alpha"}, MachineResources: []string{"kyber-system/worker-1"}}
}

func sensitiveFixtureRequest(t *testing.T, h http.Handler, browser bool, method, path string) *http.Request {
	t.Helper()
	var body io.Reader
	if method == http.MethodPut {
		body = strings.NewReader(`{"value":"not-to-be-read"}`)
	}
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if !browser {
		req.Header.Set("Authorization", "Bearer "+sensitiveFixtureKey)
		return req
	}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/browser-session", nil)
	login.Header.Set("Authorization", "Bearer "+sensitiveFixtureKey)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, login)
	if res.Code != http.StatusNoContent || len(res.Result().Cookies()) == 0 {
		t.Fatal("fixture browser login failed")
	}
	req.AddCookie(res.Result().Cookies()[0])
	req.Header.Set("Origin", "http://example.com")
	return req
}

func TestSensitiveGrantsDenyBeforeBackendReads(t *testing.T) {
	for _, browser := range []bool{false, true} {
		for _, tc := range []struct{ name, scope, method, path string }{
			{"task cannot list secrets", "tasks:read", "GET", "/api/v1/agents/alpha/secrets"},
			{"metadata cannot read values", "secrets:list", "GET", "/api/v1/agents/alpha/secrets/TOKEN"},
			{"read cannot list metadata", "secrets:read", "GET", "/api/v1/agents/alpha/secrets"},
			{"read cannot write", "secrets:read", "PUT", "/api/v1/agents/alpha/secrets/TOKEN"},
			{"read cannot delete", "secrets:read", "DELETE", "/api/v1/agents/alpha/secrets/TOKEN"},
			{"write cannot read values", "secrets:write", "GET", "/api/v1/agents/alpha/secrets/TOKEN"},
			{"write cannot list metadata", "secrets:write", "GET", "/api/v1/agents/alpha/secrets"},
			{"read cannot cross Agent", "secrets:read", "GET", "/api/v1/agents/beta/secrets/TOKEN"},
			{"write cannot cross Agent", "secrets:write", "PUT", "/api/v1/agents/beta/secrets/TOKEN"},
			{"delete cannot cross Agent", "secrets:write", "DELETE", "/api/v1/agents/beta/secrets/TOKEN"},
			{"metadata cannot cross Agent", "secrets:list", "GET", "/api/v1/agents/beta/secrets"},
			{"logs do not grant transcript", "logs:read", "GET", "/api/v1/agents/alpha/logs?source=transcript"},
			{"encoded transcript stays protected", "logs:read", "GET", "/api/v1/agents/alpha/logs?source=%74ranscript"},
			{"first source determines access", "logs:read", "GET", "/api/v1/agents/alpha/logs?source=transcript&source=kubelet"},
			{"transcript does not grant live logs", "transcripts:read", "GET", "/api/v1/agents/alpha/logs"},
			{"transcript does not grant archived logs", "transcripts:read", "GET", "/api/v1/agents/alpha/logs?source=archive"},
			{"logs cannot cross Agent", "logs:read", "GET", "/api/v1/agents/beta/logs"},
			{"transcript cannot cross Agent", "transcripts:read", "GET", "/api/v1/agents/beta/logs?source=transcript"},
			{"Agent logs do not grant host logs", "logs:read", "GET", "/api/v1/machines/worker-1/logs"},
			{"host logs do not grant Agent logs", "machines:logs:read", "GET", "/api/v1/agents/alpha/logs"},
			{"host logs cannot cross Machine", "machines:logs:read", "GET", "/api/v1/machines/worker-2/logs"},
			{"host transcript alias not delegated", "machines:logs:read", "GET", "/api/v1/machines/worker-1/logs?source=transcript"},
			{"unknown log sources stay closed", "logs:read", "GET", "/api/v1/agents/alpha/logs?source=future-private"},
			{"generic log alias stays admin", "logs:read", "GET", "/api/v1/logging/logs?agent=alpha"},
			{"generic export stays admin", "transcripts:read", "GET", "/api/v1/logging/export?agent=alpha&source=transcript"},
			{"unknown secret subpaths stay closed", "secrets:read", "GET", "/api/v1/agents/alpha/secrets/TOKEN/extra"},
		} {
			t.Run(tc.name+map[bool]string{false: " bearer", true: " cookie"}[browser], func(t *testing.T) {
				// Any Kubernetes lookup would panic; denials must happen first, even with
				// the observe-only migration flag at its default false value.
				s := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", Callers: []api.ScopedCaller{sensitiveFixtureCaller(tc.scope)}}
				h := s.BuildHandler()
				req := sensitiveFixtureRequest(t, h, browser, tc.method, tc.path)
				res := httptest.NewRecorder()
				h.ServeHTTP(res, req)
				if res.Code != http.StatusForbidden {
					t.Fatalf("status %d, want 403", res.Code)
				}
				if res.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("denial is cacheable")
				}
			})
		}
	}
}

func TestSecretGrantsSeparateMetadataValuesAndMutation(t *testing.T) {
	for _, browser := range []bool{false, true} {
		for _, scope := range []string{"secrets:list", "secrets:read", "secrets:write"} {
			t.Run(scope+map[bool]string{false: " bearer", true: " cookie"}[browser], func(t *testing.T) {
				agent := sampleAgentCRD("alpha")
				agent.Status.Phase = kyberv1.AgentPhaseStopped
				c := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent, defaultMachine()).Build()
				s := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", K8sClient: c, Callers: []api.ScopedCaller{sensitiveFixtureCaller(scope)}}
				h := s.BuildHandler()
				for _, req := range []*http.Request{
					authedJSONRequest(t, "PUT", "/api/v1/agents/alpha/secrets/TOKEN", map[string]string{"value": "fixture-private-value"}),
					authedMultipartRequest(t, "/api/v1/agents/alpha/secrets/DOCUMENT", []byte("fixture-private-file")),
				} {
					res := httptest.NewRecorder()
					h.ServeHTTP(res, req)
					if res.Code < 200 || res.Code >= 300 {
						t.Fatalf("fixture setup failed: %d", res.Code)
					}
				}
				root := httptest.NewRecorder()
				h.ServeHTTP(root, authedRequest(t, "GET", "/api/v1/agents/alpha/secrets", nil))
				var initial struct {
					Items []struct{ Key, Sha256Prefix string }
				}
				if json.Unmarshal(root.Body.Bytes(), &initial) != nil || len(initial.Items) != 2 || initial.Items[0].Sha256Prefix == "" {
					t.Fatal("fixture metadata fingerprint missing")
				}
				if scope == "secrets:list" {
					res := httptest.NewRecorder()
					h.ServeHTTP(res, sensitiveFixtureRequest(t, h, browser, "GET", "/api/v1/agents/alpha/secrets"))
					if res.Code != 200 || !strings.Contains(res.Body.String(), "TOKEN") {
						t.Fatal("metadata list unavailable")
					}
					for _, value := range []string{"fixture-private-value", "fixture-private-file", initial.Items[0].Sha256Prefix, initial.Items[1].Sha256Prefix} {
						if value != "" && strings.Contains(res.Body.String(), value) {
							t.Fatal("metadata disclosed value or fingerprint")
						}
					}
					if res.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("metadata is cacheable")
					}
				}
				if scope == "secrets:read" {
					for key, want := range map[string]string{"TOKEN": "fixture-private-value", "DOCUMENT": "fixture-private-file"} {
						res := httptest.NewRecorder()
						h.ServeHTTP(res, sensitiveFixtureRequest(t, h, browser, "GET", "/api/v1/agents/alpha/secrets/"+key))
						if res.Code != 200 || res.Body.String() != want || res.Header().Get("Cache-Control") != "no-store" {
							t.Fatal("authorized secret value/cache incorrect")
						}
					}
				}
				if scope == "secrets:write" {
					req := sensitiveFixtureRequest(t, h, browser, "PUT", "/api/v1/agents/alpha/secrets/TOKEN")
					res := httptest.NewRecorder()
					h.ServeHTTP(res, req)
					if res.Code < 200 || res.Code >= 300 {
						t.Fatal("authorized mutation failed")
					}
					read := httptest.NewRecorder()
					h.ServeHTTP(read, sensitiveFixtureRequest(t, h, browser, "GET", "/api/v1/agents/alpha/secrets/TOKEN"))
					if read.Code != 403 {
						t.Fatal("mutation grant implied value read")
					}
					deleted := httptest.NewRecorder()
					h.ServeHTTP(deleted, sensitiveFixtureRequest(t, h, browser, "DELETE", "/api/v1/agents/alpha/secrets/TOKEN"))
					if deleted.Code < 200 || deleted.Code >= 300 {
						t.Fatal("authorized deletion failed")
					}
				}
			})
		}
	}
}

func TestLogGrantsReadOnlyTheirDeclaredLane(t *testing.T) {
	for _, browser := range []bool{false, true} {
		for _, tc := range []struct{ scope, source, want string }{{"logs:read", "archive", "operational fixture"}, {"transcripts:read", "transcript", "transcript fixture"}} {
			t.Run(tc.scope+map[bool]string{false: " bearer", true: " cookie"}[browser], func(t *testing.T) {
				timestamp := time.Date(2026, 6, 3, 10, 30, 0, 0, time.UTC)
				archive := &fakeArchiveReader{byAgent: map[string][]api.LogLine{"alpha": {{Timestamp: timestamp, Text: "operational fixture"}}}}
				transcript := &fakeArchiveReader{byAgent: map[string][]api.LogLine{"alpha": {{Timestamp: timestamp, Text: "transcript fixture"}}}}
				c := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(sampleAgentCRD("alpha")).Build()
				s := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", K8sClient: c, ArchiveReader: archive, TranscriptReader: transcript, Callers: []api.ScopedCaller{sensitiveFixtureCaller(tc.scope)}}
				h := s.BuildHandler()
				path := "/api/v1/agents/alpha/logs?source=" + tc.source + "&since=2026-06-03T10:00:00Z&until=2026-06-03T11:00:00Z"
				res := httptest.NewRecorder()
				h.ServeHTTP(res, sensitiveFixtureRequest(t, h, browser, "GET", path))
				if res.Code != 200 || !strings.Contains(res.Body.String(), tc.want) || res.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("authorized log lane failed: %d", res.Code)
				}
				if (tc.source == "archive" && transcript.callCount != 0) || (tc.source == "transcript" && archive.callCount != 0) {
					t.Fatal("read another data lane")
				}
			})
		}
	}
}

func TestScopedLiveAgentAndMachineLogsAreNotCacheable(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pods") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "fixture-node-agent"}}}})
			return
		}
		_, _ = w.Write([]byte("operational log fixture\n"))
	}))
	defer backend.Close()
	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: backend.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, browser := range []bool{false, true} {
		for _, tc := range []struct{ scope, path string }{{"logs:read", "/api/v1/agents/alpha/logs"}, {"machines:logs:read", "/api/v1/machines/worker-1/logs"}} {
			c := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(sampleAgentCRD("alpha"), defaultMachine()).Build()
			s := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", K8sClient: c, Clientset: clientset, Callers: []api.ScopedCaller{sensitiveFixtureCaller(tc.scope)}}
			h := s.BuildHandler()
			res := httptest.NewRecorder()
			h.ServeHTTP(res, sensitiveFixtureRequest(t, h, browser, "GET", tc.path))
			if res.Code != 200 || !strings.Contains(res.Body.String(), "operational log fixture") || res.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("live scoped logs failed: %d", res.Code)
			}
		}
	}
}

func TestSensitiveGrantConfigurationRequiresIdentityAndExactTargets(t *testing.T) {
	for _, scope := range []string{"secrets:list", "secrets:read", "secrets:write", "logs:read", "transcripts:read", "machines:logs:read"} {
		t.Run(scope, func(t *testing.T) {
			target := "agentResources"
			if scope == "machines:logs:read" {
				target = "machineResources"
			}
			row := map[string]interface{}{"name": "integration", "key": "fixture-material", "principalId": "principal_fixture", "tenantId": "tenant_fixture", "credentialId": "credential_fixture", "credentialGeneration": 1, "scopes": []string{scope}, target: []string{"kyber-system/alpha"}}
			validate := func(wantValid bool) {
				t.Helper()
				raw, err := json.Marshal([]interface{}{row})
				if err != nil {
					t.Fatal(err)
				}
				_, err = api.ParseScopedCallers(string(raw))
				if (err == nil) != wantValid {
					t.Fatalf("unexpected configuration acceptance: %v", err)
				}
			}
			validate(true)
			for _, field := range []string{"principalId", "tenantId", "credentialId", "credentialGeneration"} {
				saved := row[field]
				delete(row, field)
				validate(false)
				row[field] = saved
			}
			row[target] = []string{"*"}
			validate(false)
			row[target] = []string{"kyber-system/alpha.beta"}
			validate(false)
			delete(row, target)
			validate(false)
		})
	}
}
