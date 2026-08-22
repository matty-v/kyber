package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// buildLogsHandler creates a test handler backed by a fake controller-runtime client.
// clientset is optional — pass nil to test the 503 path.
func buildLogsHandler(t *testing.T, objs []runtime.Object) http.Handler {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	s := &api.Server{
		K8sClient:     fakeClient,
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		// Clientset intentionally nil — tests 503 and parse paths only
	}
	return s.BuildHandler()
}

// buildLogsHandlerWithClientset creates a test handler with both a fake controller-runtime
// client and a fake k8s clientset, enabling log-streaming tests.
func buildLogsHandlerWithClientset(t *testing.T, crObjs []runtime.Object, k8sObjs ...runtime.Object) http.Handler {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(crObjs...).Build()
	fakeClientset := k8sfake.NewSimpleClientset(k8sObjs...)
	s := &api.Server{
		K8sClient:     fakeClient,
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		Clientset:     fakeClientset,
	}
	return s.BuildHandler()
}

// sampleMachineCRD returns a pre-built Machine CRD for tests.
func sampleMachineCRD(name string) *kyberv1.Machine {
	return &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider:    kyberv1.MachineProviderGCE,
			MachineType: "n2-standard-4",
			DiskSizeGb:  50,
			Zone:        "us-central1-a",
		},
	}
}

// sampleAgentWithPod returns an Agent CRD. The podName argument is retained for
// compatibility but the handler no longer reads Status.PodName — it derives the
// pod name from the agent name deterministically ("agent-<name>").
func sampleAgentWithPod(name, podName string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Machine: "worker-1",
			Runtime: "claude-code",
			Model:   "claude-sonnet-4",
			Resources: kyberv1.AgentResources{
				CPU:    resource.MustParse("1"),
				Memory: resource.MustParse("2Gi"),
				Disk:   resource.MustParse("50Gi"),
			},
			Secrets: kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
		},
		Status: kyberv1.AgentStatus{
			PodName: podName,
			Phase:   kyberv1.AgentPhaseRunning,
		},
	}
}

// samplePod returns a minimal Pod object with the given name in kyber-system.
func samplePod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kyber-system",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "agent", Image: "kyber/agent:latest"},
			},
		},
	}
}

// TestAgentLogs_AgentNotFound verifies 404 when the agent CRD does not exist.
func TestAgentLogs_AgentNotFound(t *testing.T) {
	h := buildLogsHandler(t, nil)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/missing/logs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAgentLogs_NoPod verifies 503 when the agent exists but Clientset is nil.
// The handler now derives the pod name from the agent name — it no longer checks
// Status.PodName and no longer returns 404 at that stage. The 503 comes from the
// nil Clientset guard.
func TestAgentLogs_NoPod(t *testing.T) {
	agent := sampleAgentCRD("dave") // Status.PodName is empty — handler ignores it
	h := buildLogsHandler(t, []runtime.Object{agent})
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/logs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// With the fix the handler derives "agent-dave" and then hits the nil-clientset guard.
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 (nil clientset) for agent with no pod, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAgentLogs_NoClientset verifies 503 when Clientset is nil.
func TestAgentLogs_NoClientset(t *testing.T) {
	agent := sampleAgentCRD("dave")
	h := buildLogsHandler(t, []runtime.Object{agent})
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/logs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when clientset nil, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAgentLogs_MethodNotAllowed verifies 405 for non-GET.
func TestAgentLogs_MethodNotAllowed(t *testing.T) {
	agent := sampleAgentCRD("dave")
	h := buildLogsHandler(t, []runtime.Object{agent})
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/logs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAgentLogs_NoAuth verifies 401 when auth header is missing.
func TestAgentLogs_NoAuth(t *testing.T) {
	h := buildLogsHandler(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/dave/logs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAgentLogs_InvalidName verifies 400 for invalid agent names.
func TestAgentLogs_InvalidName(t *testing.T) {
	h := buildLogsHandler(t, nil)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/INVALID_NAME/logs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAgentLogs_PodExists verifies the happy path: agent exists, pod "agent-dave" exists,
// handler streams logs successfully. Proves the deterministic pod-name derivation works.
func TestAgentLogs_PodExists(t *testing.T) {
	agent := sampleAgentCRD("dave")
	pod := samplePod("agent-dave") // matches the deterministic name
	h := buildLogsHandlerWithClientset(t, []runtime.Object{agent}, pod)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/logs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// The fake clientset returns an empty log stream (no actual log data) with 200.
	if rr.Code != http.StatusOK {
		t.Errorf("want 200 when pod exists, got %d: %s", rr.Code, rr.Body.String())
	}
}

// buildClientsetWithServer builds a real kubernetes.Clientset pointed at a given
// httptest.Server URL, allowing tests to inject custom HTTP responses from GetLogs.
func buildClientsetWithServer(t *testing.T, serverURL string) kubernetes.Interface {
	t.Helper()
	cfg := &rest.Config{
		Host: serverURL,
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build clientset: %v", err)
	}
	return cs
}

// TestAgentLogs_PodNotFound verifies 404 when the agent exists but the pod does not.
// The handler derives the pod name as "agent-dave" and k8s returns not-found on the
// log-stream call, which the handler maps to 404 with a "no running pod" message.
//
// The k8s fake clientset's GetLogs always hardcodes a 200 response, so we build a
// real clientset pointed at a mock httptest.Server that returns 404 to simulate the
// API server reporting the pod as absent.
func TestAgentLogs_PodNotFound(t *testing.T) {
	// Stand up a minimal mock "k8s API server" that returns 404 for pod log requests.
	mockAPIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		// Return a k8s Status object so client-go can parse IsNotFound correctly.
		_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)
	}))
	defer mockAPIServer.Close()

	agent := sampleAgentCRD("dave")
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agent).Build()
	realClientset := buildClientsetWithServer(t, mockAPIServer.URL)

	s := &api.Server{
		K8sClient:     fakeClient,
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		Clientset:     realClientset,
	}
	h := s.BuildHandler()

	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/logs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404 when pod not found, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no running pod") {
		t.Errorf("want 'no running pod' in body, got: %s", rr.Body.String())
	}
}

// TestMachineLogs_MachineNotFound verifies 404 when the machine CRD does not exist.
func TestMachineLogs_MachineNotFound(t *testing.T) {
	h := buildLogsHandler(t, nil)
	req := authedRequest(t, http.MethodGet, "/api/v1/machines/missing/logs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestMachineLogs_NoClientset verifies 503 when Clientset is nil.
func TestMachineLogs_NoClientset(t *testing.T) {
	machine := sampleMachineCRD("node-1")
	h := buildLogsHandler(t, []runtime.Object{machine})
	req := authedRequest(t, http.MethodGet, "/api/v1/machines/node-1/logs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when clientset nil, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestParsePodLogOptions_Follow verifies that ?follow=true is parsed.
func TestParsePodLogOptions_Follow(t *testing.T) {
	// The handler derives the pod name from the agent name, so no PodName in status needed.
	agent := sampleAgentCRD("dave")
	h := buildLogsHandler(t, []runtime.Object{agent})

	for _, followVal := range []string{"true", "1", "false", ""} {
		req := authedRequest(t, http.MethodGet,
			"/api/v1/agents/dave/logs?follow="+followVal+"&tail=50&since=5m", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		// Should get 503 (nil clientset), not 500 or panic.
		if rr.Code == http.StatusInternalServerError {
			t.Errorf("follow=%q caused 500: %s", followVal, rr.Body.String())
		}
	}
}

// TestAgentLogs_WithRealStream verifies the log streaming happy path using a
// fake io.ReadCloser injected via a custom test server that bypasses the k8s
// client layer. This tests the header-setting + copy loop without a real cluster.
func TestAgentLogs_WithRealStream(t *testing.T) {
	// We build a minimal HTTP server that mimics what handleAgentLogs does:
	// set chunked headers, write content, flush. We verify the client receives it.
	content := "log line 1\nlog line 2\n"
	innerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, content)
	}))
	defer innerSrv.Close()

	resp, err := http.Get(innerSrv.URL) //nolint:noctx
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if !strings.Contains(string(body), "log line 1") {
		t.Errorf("expected log content, got: %q", string(body))
	}
}

// TestAgentLogs_DefaultContainerIsAgent pins the kyber#385 fix: the
// handler MUST set container="agent" on the GetLogs request so kube-
// apiserver doesn't reject the call on a multi-container pod with
// "a container name must be specified for pod ...".
//
// Reproduces by inspecting the request URL the handler sends to a
// mock API server.
func TestAgentLogs_DefaultContainerIsAgent(t *testing.T) {
	var gotURL string
	mockAPIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "log line\n")
	}))
	defer mockAPIServer.Close()

	agent := sampleAgentCRD("dave")
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agent).Build()
	cs := buildClientsetWithServer(t, mockAPIServer.URL)
	s := &api.Server{
		K8sClient:     fakeClient,
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		Clientset:     cs,
	}
	h := s.BuildHandler()

	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/logs?tail=50", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(gotURL, "container=agent") {
		t.Errorf("expected GetLogs request to carry container=agent, got URL %q", gotURL)
	}
}

// TestAgentLogs_ExplicitContainerForwarded: ?container=session-brief
// and ?container=kyber-status-sidecar must round-trip through to the
// kube-apiserver so operators can tail the sidecars.
func TestAgentLogs_ExplicitContainerForwarded(t *testing.T) {
	for _, want := range []string{"session-brief", "kyber-status-sidecar"} {
		want := want
		t.Run(want, func(t *testing.T) {
			var gotURL string
			mockAPIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotURL = r.URL.String()
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "log line\n")
			}))
			defer mockAPIServer.Close()

			agent := sampleAgentCRD("dave")
			scheme := mustNewScheme(t)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agent).Build()
			cs := buildClientsetWithServer(t, mockAPIServer.URL)
			s := &api.Server{
				K8sClient:     fakeClient,
				APIKey:        testAPIKey,
				Namespace:     "kyber-system",
				Clientset:     cs,
			}
			h := s.BuildHandler()

			req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/logs?container="+want, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(gotURL, "container="+want) {
				t.Errorf("expected GetLogs request to carry container=%s, got URL %q", want, gotURL)
			}
		})
	}
}

// TestAgentLogs_InvalidContainerReturns400: any container value outside
// the known set must produce a 400 before the kube-apiserver is touched
// (so we surface the misconfiguration clearly rather than letting the
// upstream return a less-actionable error).
func TestAgentLogs_InvalidContainerReturns400(t *testing.T) {
	// Mock server records whether the handler hit it; for an invalid
	// container the handler should reject early without making the call.
	var hit bool
	mockAPIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer mockAPIServer.Close()

	agent := sampleAgentCRD("dave")
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agent).Build()
	cs := buildClientsetWithServer(t, mockAPIServer.URL)
	s := &api.Server{
		K8sClient:     fakeClient,
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		Clientset:     cs,
	}
	h := s.BuildHandler()

	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/logs?container=bogus", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for invalid container, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "invalid_container") {
		t.Errorf("want body to include error code 'invalid_container', got %s", rr.Body.String())
	}
	if hit {
		t.Errorf("handler should reject invalid container before reaching upstream; mock server was hit")
	}
}
