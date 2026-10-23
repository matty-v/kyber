package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/matty-v/kyber/internal/testutil"
	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestExecGrantsRequireExactKindAndResourceForBearerAndCookie(t *testing.T) {
	for _, browser := range []bool{false, true} {
		for _, tc := range []struct {
			name, scope, kind, target string
			allowed                   bool
		}{
			{"agent allowed", "agents:exec", "agents", "alpha", true},
			{"agent cross resource", "agents:exec", "agents", "other", false},
			{"agent cannot reach machine", "agents:exec", "machines", "alpha", false},
			{"machine allowed", "machines:exec", "machines", "alpha", true},
			{"machine cross resource", "machines:exec", "machines", "other", false},
			{"machine cannot reach agent", "machines:exec", "agents", "alpha", false},
			{"lifecycle is not exec", "lifecycle:admin", "agents", "alpha", false},
		} {
			t.Run(tc.name+map[bool]string{false: " bearer", true: " cookie"}[browser], func(t *testing.T) {
				s := &api.Server{SecurityAudit: testutil.NewSecurityAudit(), APIKey: "exec-root", Namespace: "kyber-system", Callers: []api.ScopedCaller{{Name: "integration", Key: "exec-integration", PrincipalID: "principal_exec_test", TenantID: "tenant_test", CredentialID: "credential_exec_test", CredentialGeneration: 1, Scopes: []string{tc.scope}, AgentResources: []string{"kyber-system/alpha"}, MachineResources: []string{"kyber-system/alpha"}}}}
				// An unauthorized lookup would panic. Allowed routes use an empty backend
				// and must return 404, proving authorization admitted exactly that target.
				if tc.allowed {
					s.K8sClient = fake.NewClientBuilder().WithScheme(mustNewScheme(t)).Build()
				}
				h := s.BuildHandler()
				var cookie *http.Cookie
				if browser {
					login := httptest.NewRequest(http.MethodPost, "/api/v1/browser-session", nil)
					login.Header.Set("Authorization", "Bearer exec-integration")
					res := httptest.NewRecorder()
					h.ServeHTTP(res, login)
					if res.Code != http.StatusNoContent || len(res.Result().Cookies()) == 0 {
						t.Fatal("browser login failed")
					}
					cookie = res.Result().Cookies()[0]
				}
				for _, query := range []string{"?mode=shell", "?mode=attach", "?mode=history", "?mode=device-auth", "?cmd=private-command"} {
					req := httptest.NewRequest(http.MethodGet, "/api/v1/"+tc.kind+"/"+tc.target+"/exec"+query, nil)
					req.Header.Set("Connection", "Upgrade")
					req.Header.Set("Upgrade", "websocket")
					if cookie != nil {
						req.AddCookie(cookie)
					} else {
						req.Header.Set("Authorization", "Bearer exec-integration")
					}
					res := httptest.NewRecorder()
					h.ServeHTTP(res, req)
					want := http.StatusForbidden
					if tc.allowed {
						want = http.StatusNotFound
					}
					if res.Code != want {
						t.Fatalf("%s: got %d, want %d", query, res.Code, want)
					}
					if res.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("exec response can be cached")
					}
					if !tc.allowed && strings.Contains(res.Body.String(), tc.target) {
						t.Fatal("denial disclosed target existence")
					}
				}
			})
		}
	}
}

func TestExecGrantConfigurationRequiresStableIdentityAndExactResources(t *testing.T) {
	for _, field := range []string{"agentResources", "machineResources"} {
		scope := "agents:exec"
		if field == "machineResources" {
			scope = "machines:exec"
		}
		for _, resource := range []string{"kyber-system/alpha", "*", "alpha", "kyber-system/*", "kyber-system/alpha.beta", "kyber-system/1alpha"} {
			raw := `[{"name":"exec","key":"private","principalId":"principal_exec","tenantId":"tenant_installation","credentialId":"credential_exec","credentialGeneration":1,"scopes":["` + scope + `"],"` + field + `":["` + resource + `"]}]`
			_, err := api.ParseScopedCallers(raw)
			if (err == nil) != (resource == "kyber-system/alpha") {
				t.Fatalf("%s %s: unexpected validation result: %v", scope, resource, err)
			}
		}
		if _, err := api.ParseScopedCallers(`[{"name":"exec","key":"private","scopes":["` + scope + `"],"` + field + `":["kyber-system/alpha"]}]`); err == nil {
			t.Fatal("accepted exec without stable identity")
		}
	}
}

func TestExecGrantedWebSocketUpgradeForBearerAndCookie(t *testing.T) {
	for _, kind := range []string{"agents", "machines"} {
		for _, browser := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: " bearer", true: " cookie"}[browser], func(t *testing.T) {
				var execCalls atomic.Int32
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/pods") {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "node-agent-test", Namespace: "kyber-system"}}}})
						return
					}
					if strings.HasSuffix(r.URL.Path, "/exec") {
						execCalls.Add(1)
					}
					http.Error(w, "test executor stopped", http.StatusServiceUnavailable)
				}))
				defer backend.Close()
				config := &rest.Config{Host: backend.URL}
				clientset, err := kubernetes.NewForConfig(config)
				if err != nil {
					t.Fatal(err)
				}
				agent := sampleAgentForExec("alpha", "agent-alpha")
				machine := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "kyber-system"}}
				scope := "agents:exec"
				if kind == "machines" {
					scope = "machines:exec"
				}
				s := &api.Server{SecurityAudit: testutil.NewSecurityAudit(), APIKey: "exec-root", Namespace: "kyber-system", RestConfig: config, Clientset: clientset,
					K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent, machine).Build(),
					Callers:   []api.ScopedCaller{{Name: "integration", Key: "exec-integration", PrincipalID: "principal_exec_test", TenantID: "tenant_test", CredentialID: "credential_exec_test", CredentialGeneration: 1, Scopes: []string{scope}, AgentResources: []string{"kyber-system/alpha"}, MachineResources: []string{"kyber-system/alpha"}}},
				}
				h := s.BuildHandler()
				headers := http.Header{"Authorization": []string{"Bearer exec-integration"}}
				if browser {
					req := httptest.NewRequest(http.MethodPost, "/api/v1/browser-session", nil)
					req.Header.Set("Authorization", "Bearer exec-integration")
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					if rec.Code != http.StatusNoContent || len(rec.Result().Cookies()) == 0 {
						t.Fatal("browser login failed")
					}
					c := rec.Result().Cookies()[0]
					headers = http.Header{"Cookie": []string{c.Name + "=" + c.Value}}
				}
				server := httptest.NewServer(h)
				defer server.Close()
				for _, query := range []string{"?mode=shell", "?mode=attach", "?mode=history", "?mode=device-auth", "?cmd=echo&cmd=PRIVATE_EXEC_ARGUMENT"} {
					dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
					conn, res, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/"+kind+"/alpha/exec"+query, headers)
					if err != nil {
						t.Fatalf("granted websocket upgrade failed: %v", err)
					}
					if res.StatusCode != http.StatusSwitchingProtocols {
						t.Fatal("upgrade status incorrect")
					}
					if res.Header.Get(api.RequestIDHeader) == "" || res.Header.Get("Cache-Control") != "no-store" {
						t.Fatal("terminal upgrade lost audit correlation/cache headers")
					}
					_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
					// The stub returns a controlled executor error after authorization and
					// upgrade. No command runs, but the actual Kubernetes transport is reached.
					kind, payload, readErr := conn.ReadMessage()
					_ = conn.Close()
					if readErr != nil || kind != websocket.TextMessage || !strings.Contains(string(payload), `"type":"exit"`) {
						t.Fatal("completed exec did not report exit before client close")
					}
				}
				if execCalls.Load() != 5 {
					t.Fatalf("authorized executor calls = %d, want 5", execCalls.Load())
				}
				audit := s.SecurityAudit.(*testutil.SecurityAudit)
				deadline := time.Now().Add(2 * time.Second)
				for len(audit.Records()) < 10 && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				records := audit.Records()
				if len(records) != 10 {
					t.Fatalf("terminal audit records=%d, want 10", len(records))
				}
				completions := 0
				for _, record := range records {
					if record.Phase == "completion" {
						completions++
						if record.Outcome != "failure" {
							t.Fatal("upgrade was confused with successful exec", record.Outcome)
						}
					}
				}
				if completions != 5 {
					t.Fatal("missing terminal completion records", completions)
				}
				encoded, _ := json.Marshal(records)
				if strings.Contains(string(encoded), "PRIVATE_EXEC_ARGUMENT") || strings.Contains(string(encoded), "test executor stopped") {
					t.Fatal("terminal arguments/errors entered audit history")
				}
			})
		}
	}
}

func TestExecDisconnectCancelsKubernetesTransport(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(4 * time.Second):
			http.Error(w, "test timeout", http.StatusServiceUnavailable)
		}
	}))
	defer backend.Close()
	config := &rest.Config{Host: backend.URL}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	s := &api.Server{SecurityAudit: testutil.NewSecurityAudit(), APIKey: "exec-root", Namespace: "kyber-system", RestConfig: config, Clientset: clientset,
		K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(sampleAgentForExec("alpha", "agent-alpha")).Build(),
		Callers:   []api.ScopedCaller{{Name: "integration", Key: "exec-integration", PrincipalID: "principal_exec_test", TenantID: "tenant_test", CredentialID: "credential_exec_test", CredentialGeneration: 1, Scopes: []string{"agents:exec"}, AgentResources: []string{"kyber-system/alpha"}}},
	}
	server := httptest.NewServer(s.BuildHandler())
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/agents/alpha/exec", http.Header{"Authorization": []string{"Bearer exec-integration"}})
	if err != nil {
		t.Fatal("WebSocket upgrade failed")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		_ = conn.Close()
		t.Fatal("exec transport not reached")
	}
	_ = conn.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("client disconnect did not cancel exec transport")
	}
}
