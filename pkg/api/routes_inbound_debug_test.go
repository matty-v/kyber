package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// debugHarness wires an authenticated handler for POST /api/v1/inbound-debug.
type debugHarness struct {
	handler http.Handler
}

func buildDebugHarness(t *testing.T) *debugHarness {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	srv := &api.Server{
		K8sClient: fakeClient,
		APIKey:    testAPIKey,
		Namespace: "kyber-system",
	}
	return &debugHarness{handler: srv.BuildHandler()}
}

// postDebug serializes body as JSON, sets the API key, and returns the
// recorder.
func postDebug(t *testing.T, h *debugHarness, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/inbound-debug", bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	return rr
}

// debugBinding is a typical operator binding spec used in the tests below.
func debugBinding() kyberv1.AgentInboundBinding {
	return kyberv1.AgentInboundBinding{
		Name:            "github",
		ExistingSecret:  "ignored-by-debug",
		SignatureHeader: "X-Hub-Signature-256",
		SignaturePrefix: "sha256=",
		EventHeader:     "X-GitHub-Event",
		EventPath:       "$.action",
		MatchEvents:     []string{"pull_request.opened"},
		Filters: []kyberv1.AgentInboundFilter{
			{JsonPath: "$.repository.full_name", Equals: "matty-v/kyber"},
		},
		Fields: []kyberv1.AgentInboundField{
			{Label: "repo", JsonPath: "$.repository.full_name"},
			{Label: "title", JsonPath: "$.pull_request.title", Truncate: 5},
		},
		Action: "Review the PR.",
	}
}

// TestDebug_HappyPath: matching payload returns match=true with a fully
// populated trace.
func TestDebug_HappyPath(t *testing.T) {
	h := buildDebugHarness(t)
	body := map[string]any{
		"binding": debugBinding(),
		"payload": json.RawMessage(`{"action":"opened","repository":{"full_name":"matty-v/kyber"},"pull_request":{"title":"Hello world long title"}}`),
		"headers": map[string]string{"X-GitHub-Event": "pull_request"},
	}

	rr := postDebug(t, h, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp api.InboundDebugResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if !resp.Match {
		t.Errorf("expected match=true, got false (dropReason=%q)", resp.DropReason)
	}
	if resp.ResolvedEvent != "pull_request.opened" {
		t.Errorf("resolvedEvent: got %q want pull_request.opened", resp.ResolvedEvent)
	}
	if len(resp.FilterResults) != 1 || !resp.FilterResults[0].Passed {
		t.Errorf("expected 1 passing filter; got %+v", resp.FilterResults)
	}
	if resp.FilterResults[0].ExtractedValue != "matty-v/kyber" {
		t.Errorf("filter extractedValue: got %q", resp.FilterResults[0].ExtractedValue)
	}
	if len(resp.FieldResults) != 2 {
		t.Fatalf("expected 2 field results; got %d", len(resp.FieldResults))
	}
	// First field — no truncation expected.
	if resp.FieldResults[0].Truncated {
		t.Errorf("repo field shouldn't be truncated")
	}
	// Second field — truncate=5 → "Hello world long title" (22 chars) is truncated.
	if !resp.FieldResults[1].Truncated {
		t.Errorf("title field should be truncated; got %+v", resp.FieldResults[1])
	}
	// Raw extractedValue should be the un-truncated string.
	if resp.FieldResults[1].ExtractedValue != "Hello world long title" {
		t.Errorf("title extractedValue should be raw, got %q",
			resp.FieldResults[1].ExtractedValue)
	}
	if resp.Envelope == "" {
		t.Errorf("envelope should be populated on match=true")
	}
	if !strings.Contains(resp.Envelope, "Review the PR.") {
		t.Errorf("envelope missing action text; got %q", resp.Envelope)
	}
}

// TestDebug_FilterRejected: payload doesn't match the filter Equals
// clause → match=false, filterResults shows the failure with a reason.
func TestDebug_FilterRejected(t *testing.T) {
	h := buildDebugHarness(t)
	body := map[string]any{
		"binding": debugBinding(),
		"payload": json.RawMessage(`{"action":"opened","repository":{"full_name":"someone/else"},"pull_request":{"title":"x"}}`),
		"headers": map[string]string{"X-GitHub-Event": "pull_request"},
	}

	rr := postDebug(t, h, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.InboundDebugResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Match {
		t.Errorf("expected match=false on filter mismatch")
	}
	if resp.DropReason != "filter-rejected" {
		t.Errorf("dropReason: got %q want filter-rejected", resp.DropReason)
	}
	if len(resp.FilterResults) != 1 || resp.FilterResults[0].Passed {
		t.Fatalf("expected 1 failing filter; got %+v", resp.FilterResults)
	}
	if resp.FilterResults[0].Reason != "not-equals" {
		t.Errorf("filter reason: got %q want not-equals", resp.FilterResults[0].Reason)
	}
	if resp.Envelope != "" {
		t.Errorf("envelope should be empty on no-match; got %q", resp.Envelope)
	}
}

// TestDebug_UnmatchedEvent: matchEvents excludes the resolved event →
// match=false, dropReason=unmatched-event.
func TestDebug_UnmatchedEvent(t *testing.T) {
	h := buildDebugHarness(t)
	body := map[string]any{
		"binding": debugBinding(),
		// action="closed" → resolvedEvent = "pull_request.closed", not in matchEvents.
		"payload": json.RawMessage(`{"action":"closed","repository":{"full_name":"matty-v/kyber"},"pull_request":{"title":"x"}}`),
		"headers": map[string]string{"X-GitHub-Event": "pull_request"},
	}

	rr := postDebug(t, h, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.InboundDebugResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Match {
		t.Errorf("expected match=false")
	}
	if resp.DropReason != "unmatched-event" {
		t.Errorf("dropReason: got %q want unmatched-event", resp.DropReason)
	}
	if resp.ResolvedEvent != "pull_request.closed" {
		t.Errorf("resolvedEvent: got %q", resp.ResolvedEvent)
	}
}

// TestDebug_FieldExtraction_Nested: nested values surface with the right
// truncation flag.
func TestDebug_FieldExtraction_Nested(t *testing.T) {
	h := buildDebugHarness(t)
	binding := debugBinding()
	binding.Filters = nil // skip filters; just exercise field extraction.
	binding.MatchEvents = nil

	body := map[string]any{
		"binding": binding,
		"payload": json.RawMessage(`{"repository":{"full_name":"matty-v/kyber"},"pull_request":{"title":"abc"}}`),
		"headers": map[string]string{"X-GitHub-Event": "pull_request"},
	}

	rr := postDebug(t, h, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.InboundDebugResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.Match {
		t.Fatalf("want match=true")
	}
	// title="abc" with Truncate=5 → no truncation flag.
	if len(resp.FieldResults) != 2 {
		t.Fatalf("got %d field results", len(resp.FieldResults))
	}
	if resp.FieldResults[1].Truncated {
		t.Errorf("title %q shouldn't be truncated at 5", resp.FieldResults[1].ExtractedValue)
	}
}

// TestDebug_MissingBindingName: 400 with VALIDATION_ERROR + field hint.
func TestDebug_MissingBindingName(t *testing.T) {
	h := buildDebugHarness(t)
	binding := debugBinding()
	binding.Name = ""
	body := map[string]any{
		"binding": binding,
		"payload": json.RawMessage(`{}`),
	}
	rr := postDebug(t, h, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "binding.name") {
		t.Errorf("body should reference binding.name field; got %s", rr.Body.String())
	}
}

// TestDebug_MissingSignatureHeader: 400 with field hint.
func TestDebug_MissingSignatureHeader(t *testing.T) {
	h := buildDebugHarness(t)
	binding := debugBinding()
	binding.SignatureHeader = ""
	body := map[string]any{
		"binding": binding,
		"payload": json.RawMessage(`{}`),
	}
	rr := postDebug(t, h, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "signatureHeader") {
		t.Errorf("body should reference signatureHeader; got %s", rr.Body.String())
	}
}

// TestDebug_MissingAction: 400 with field hint.
func TestDebug_MissingAction(t *testing.T) {
	h := buildDebugHarness(t)
	binding := debugBinding()
	binding.Action = ""
	body := map[string]any{
		"binding": binding,
		"payload": json.RawMessage(`{}`),
	}
	rr := postDebug(t, h, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "action") {
		t.Errorf("body should reference action; got %s", rr.Body.String())
	}
}

// TestDebug_InvalidJSONPayload: payload is not valid JSON → 400.
func TestDebug_InvalidJSONPayload(t *testing.T) {
	h := buildDebugHarness(t)
	// We can't use map+marshal here because we need a literal invalid
	// payload at the binding level. Build the JSON by hand.
	bindingJSON, _ := json.Marshal(debugBinding())
	body := []byte(`{"binding":` + string(bindingJSON) + `,"payload":not-json}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/inbound-debug", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestDebug_PayloadOmitted: empty payload → 400.
func TestDebug_PayloadOmitted(t *testing.T) {
	h := buildDebugHarness(t)
	bindingJSON, _ := json.Marshal(debugBinding())
	body := []byte(`{"binding":` + string(bindingJSON) + `}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/inbound-debug", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "payload") {
		t.Errorf("body should mention payload field; got %s", rr.Body.String())
	}
}

// TestDebug_MethodNotAllowed: GET → 405.
func TestDebug_MethodNotAllowed(t *testing.T) {
	h := buildDebugHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/inbound-debug", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rr.Code)
	}
}

// TestDebug_NoAuth: no API key → 401 (auth middleware).
func TestDebug_NoAuth(t *testing.T) {
	h := buildDebugHarness(t)
	body, _ := json.Marshal(map[string]any{"binding": debugBinding(), "payload": json.RawMessage(`{}`)})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/inbound-debug", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}
