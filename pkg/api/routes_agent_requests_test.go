package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/requeststore"
)

const (
	requestWriteKey = "request-write-key"
	requestReadKey  = "request-read-key"
	lifecycleKey    = "lifecycle-key"
)

type requestHarness struct {
	handler http.Handler
	store   requeststore.Store
}

func buildRequestHarness(t *testing.T, store requeststore.Store, agents ...string) *requestHarness {
	t.Helper()
	scheme := mustNewScheme(t)
	objects := make([]clientObject, 0, len(agents))
	for _, name := range agents {
		objects = append(objects, bareAgent(name))
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, object := range objects {
		builder = builder.WithObjects(object)
	}
	server := &api.Server{
		K8sClient:    builder.Build(),
		APIKey:       testAPIKey,
		Namespace:    "kyber-system",
		RequestStore: store,
		Callers: []api.ScopedCaller{
			{Name: "writer", Key: requestWriteKey, Scopes: []string{"requests:write"}},
			{Name: "reader", Key: requestReadKey, Scopes: []string{"requests:read"}},
			{Name: "pwa", Key: lifecycleKey, Scopes: []string{"lifecycle:write"}},
		},
	}
	return &requestHarness{handler: server.BuildHandler(), store: store}
}

// clientObject is the subset accepted by fake.ClientBuilder.WithObjects.
type clientObject interface {
	client.Object
}

func newRequestMemoryStore(t *testing.T, limits requeststore.Limits) *requeststore.MemoryStore {
	t.Helper()
	store, err := requeststore.NewMemoryStore(limits)
	if err != nil {
		t.Fatalf("NewMemoryStore() error = %v", err)
	}
	return store
}

func (h *requestHarness) do(t *testing.T, method, target, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(encoded))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, req)
	return recorder
}

func TestAgentRequests_SubmitAndReadCompletedResponse(t *testing.T) {
	store := newRequestMemoryStore(t, requeststore.DefaultLimits())
	h := buildRequestHarness(t, store, "kiosk")

	submitted := h.do(t, http.MethodPost, "/api/v1/agents/kiosk/requests", requestWriteKey,
		map[string]string{"prompt": "Run /features", "correlation": "gateway-1"})
	if submitted.Code != http.StatusAccepted {
		t.Fatalf("POST code = %d, body = %s", submitted.Code, submitted.Body.String())
	}
	var created struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		CreatedAt string `json:"createdAt"`
		ExpiresAt string `json:"expiresAt"`
		Response  string `json:"response"`
		Prompt    string `json:"prompt"`
	}
	if err := json.Unmarshal(submitted.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal POST: %v", err)
	}
	if !strings.HasPrefix(created.ID, "req_") || len(created.ID) != 36 || created.Status != "queued" {
		t.Fatalf("POST response = %+v", created)
	}
	if created.CreatedAt == "" || created.ExpiresAt == "" {
		t.Fatalf("POST timestamps missing: %+v", created)
	}
	if created.Response != "" || created.Prompt != "" || strings.Contains(submitted.Body.String(), "Run /features") {
		t.Fatalf("POST leaked prompt/result: %s", submitted.Body.String())
	}
	if location := submitted.Header().Get("Location"); location != "/api/v1/agents/kiosk/requests/"+created.ID {
		t.Fatalf("Location = %q", location)
	}

	if err := store.MarkDispatched(context.Background(), "kiosk", created.ID); err != nil {
		t.Fatalf("MarkDispatched() error = %v", err)
	}
	if err := store.Complete(context.Background(), "kiosk", created.ID, "Kyber runs agents."); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	read := h.do(t, http.MethodGet, "/api/v1/agents/kiosk/requests/"+created.ID, requestReadKey, nil)
	if read.Code != http.StatusOK {
		t.Fatalf("GET code = %d, body = %s", read.Code, read.Body.String())
	}
	var completed struct {
		Status   string `json:"status"`
		Response string `json:"response"`
		Prompt   string `json:"prompt"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &completed); err != nil {
		t.Fatalf("unmarshal GET: %v", err)
	}
	if completed.Status != "completed" || completed.Response != "Kyber runs agents." || completed.Prompt != "" {
		t.Fatalf("GET response = %+v", completed)
	}
}

func TestAgentRequests_RequestScopesAreStrictAndIndependent(t *testing.T) {
	store := newRequestMemoryStore(t, requeststore.DefaultLimits())
	h := buildRequestHarness(t, store, "kiosk")

	tests := []struct {
		name   string
		method string
		target string
		key    string
		body   any
	}{
		{"reader cannot submit", http.MethodPost, "/api/v1/agents/kiosk/requests", requestReadKey, map[string]string{"prompt": "x"}},
		{"writer cannot read", http.MethodGet, "/api/v1/agents/kiosk/requests/req_00000000000000000000000000000000", requestWriteKey, nil},
		{"lifecycle key cannot submit", http.MethodPost, "/api/v1/agents/kiosk/requests", lifecycleKey, map[string]string{"prompt": "x"}},
		{"lifecycle key cannot read", http.MethodGet, "/api/v1/agents/kiosk/requests/req_00000000000000000000000000000000", lifecycleKey, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			response := h.do(t, tc.method, tc.target, tc.key, tc.body)
			if response.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403: %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "requests:") {
				t.Fatalf("403 leaks scope detail: %s", response.Body.String())
			}
		})
	}

	// The legacy key remains full-scope for compatibility.
	legacy := h.do(t, http.MethodPost, "/api/v1/agents/kiosk/requests", testAPIKey, map[string]string{"prompt": "x"})
	if legacy.Code != http.StatusAccepted {
		t.Fatalf("legacy POST code = %d: %s", legacy.Code, legacy.Body.String())
	}
}

func TestAgentRequests_ValidationAndLimits(t *testing.T) {
	limits := requeststore.DefaultLimits()
	limits.MaxPromptBytes = 4
	limits.MaxCorrelationBytes = 4
	limits.MaxOutstanding = 1
	store := newRequestMemoryStore(t, limits)
	h := buildRequestHarness(t, store, "kiosk")

	tests := []struct {
		name string
		body any
		code int
	}{
		{"empty prompt", map[string]string{"prompt": ""}, http.StatusBadRequest},
		{"prompt too large by UTF-8 bytes", map[string]string{"prompt": "ééé"}, http.StatusRequestEntityTooLarge},
		{"correlation too large by UTF-8 bytes", map[string]string{"prompt": "ok", "correlation": "ééé"}, http.StatusRequestEntityTooLarge},
		{"unknown field", map[string]string{"prompt": "ok", "extra": "no"}, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			response := h.do(t, http.MethodPost, "/api/v1/agents/kiosk/requests", requestWriteKey, tc.body)
			if response.Code != tc.code {
				t.Fatalf("code = %d, want %d: %s", response.Code, tc.code, response.Body.String())
			}
		})
	}

	first := h.do(t, http.MethodPost, "/api/v1/agents/kiosk/requests", requestWriteKey, map[string]string{"prompt": "one"})
	if first.Code != http.StatusAccepted {
		t.Fatalf("first POST = %d: %s", first.Code, first.Body.String())
	}
	second := h.do(t, http.MethodPost, "/api/v1/agents/kiosk/requests", requestWriteKey, map[string]string{"prompt": "two"})
	if second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") != "1" {
		t.Fatalf("second POST = %d, Retry-After=%q: %s", second.Code, second.Header().Get("Retry-After"), second.Body.String())
	}
}

func TestAgentRequests_AgentAndRequestIsolation(t *testing.T) {
	store := newRequestMemoryStore(t, requeststore.DefaultLimits())
	h := buildRequestHarness(t, store, "alice", "bob")

	created := h.do(t, http.MethodPost, "/api/v1/agents/alice/requests", requestWriteKey, map[string]string{"prompt": "hello"})
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &body); err != nil || body.ID == "" {
		t.Fatalf("create response = %s, err=%v", created.Body.String(), err)
	}
	wrongAgent := h.do(t, http.MethodGet, "/api/v1/agents/bob/requests/"+body.ID, requestReadKey, nil)
	if wrongAgent.Code != http.StatusNotFound {
		t.Fatalf("cross-agent GET = %d: %s", wrongAgent.Code, wrongAgent.Body.String())
	}
	unknownAgent := h.do(t, http.MethodPost, "/api/v1/agents/ghost/requests", requestWriteKey, map[string]string{"prompt": "hello"})
	if unknownAgent.Code != http.StatusNotFound {
		t.Fatalf("unknown-agent POST = %d: %s", unknownAgent.Code, unknownAgent.Body.String())
	}
}

func TestAgentRequests_StoreUnavailable(t *testing.T) {
	h := buildRequestHarness(t, nil, "kiosk")
	for _, test := range []struct {
		method string
		target string
		key    string
		body   any
	}{
		{http.MethodPost, "/api/v1/agents/kiosk/requests", requestWriteKey, map[string]string{"prompt": "hello"}},
		{http.MethodGet, "/api/v1/agents/kiosk/requests/req_00000000000000000000000000000000", requestReadKey, nil},
	} {
		response := h.do(t, test.method, test.target, test.key, test.body)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s = %d: %s", test.method, test.target, response.Code, response.Body.String())
		}
	}
}

type failingRequestStore struct{}

func (failingRequestStore) Create(context.Context, string, string, string, string) (*requeststore.Request, error) {
	return nil, errors.New("offline")
}
func (failingRequestStore) Get(context.Context, string, string) (*requeststore.Request, error) {
	return nil, errors.New("offline")
}
func (failingRequestStore) MarkDispatched(context.Context, string, string) error {
	return errors.New("offline")
}
func (failingRequestStore) Fail(context.Context, string, string, requeststore.FailureCode) error {
	return errors.New("offline")
}
func (failingRequestStore) Complete(context.Context, string, string, string) error {
	return errors.New("offline")
}

func TestAgentRequests_StoreErrorsAreUnavailable(t *testing.T) {
	h := buildRequestHarness(t, failingRequestStore{}, "kiosk")
	post := h.do(t, http.MethodPost, "/api/v1/agents/kiosk/requests", requestWriteKey, map[string]string{"prompt": "hello"})
	if post.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST code = %d: %s", post.Code, post.Body.String())
	}
	get := h.do(t, http.MethodGet, "/api/v1/agents/kiosk/requests/req_00000000000000000000000000000000", requestReadKey, nil)
	if get.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET code = %d: %s", get.Code, get.Body.String())
	}
}
