package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleVersion_ReturnsAllFields(t *testing.T) {
	// chartVersion is resolved at boot by cmd/control-plane resolveDisplayVersion
	// — the build-injected main.Version (kyber#482), falling back to the chart
	// file — and stored on the Server. The handler must echo whatever it's
	// given, NOT a literal tied to any version source. Use a representative
	// release value (not 0.1.0) so the test asserts pass-through, not a stale
	// constant (kyber#457 AC#6): the live value changes per release, and a
	// hardcoded 0.1.0 assertion would encode the bug.
	const wantChartVersion = "1.2.3"
	s := &Server{
		BuildSHA:     "abc1234",
		BuildDate:    "2026-04-21T17:30:00Z",
		ChartVersion: wantChartVersion,
		Substrate:    "kyber-razer",
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	rr := httptest.NewRecorder()
	s.handleVersion(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got VersionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SHA != "abc1234" {
		t.Errorf("sha = %q, want abc1234", got.SHA)
	}
	if got.BuildDate != "2026-04-21T17:30:00Z" {
		t.Errorf("buildDate = %q, want 2026-04-21T17:30:00Z", got.BuildDate)
	}
	if got.ChartVersion != wantChartVersion {
		t.Errorf("chartVersion = %q, want %q (handler must echo the injected value)", got.ChartVersion, wantChartVersion)
	}
	if got.Substrate != "kyber-razer" {
		t.Errorf("substrate = %q, want kyber-razer", got.Substrate)
	}
}

func TestHandleVersion_RejectsNonGet(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/version", nil)
	rr := httptest.NewRecorder()
	s.handleVersion(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestHandleVersion_EmptyServerReturnsEmpties(t *testing.T) {
	// A server built without ldflags / without the chart-version file / without
	// KYBER_NAMESPACE still returns 200 with empty-string fields. The PWA
	// renders "—" for empties rather than erroring.
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	rr := httptest.NewRecorder()
	s.handleVersion(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got VersionResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.SHA != "" || got.BuildDate != "" || got.ChartVersion != "" || got.Substrate != "" {
		t.Errorf("expected all-empty response, got %+v", got)
	}
}
