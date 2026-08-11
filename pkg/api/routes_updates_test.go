package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/updates"
)

const (
	updNS     = "kyber-system"
	updDeploy = "kyber-control-plane"
)

// updatesServer builds a Server with a working checker backed by a stub feed.
func updatesServer(t *testing.T, currentVersion string, objs ...client.Object) (*Server, *httptest.Server) {
	t.Helper()
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.0","html_url":"https://example.invalid/r","draft":false,"prerelease":false}`))
	}))
	t.Cleanup(feed.Close)

	c := fake.NewClientBuilder().WithObjects(objs...).Build()
	store := &updates.Store{Client: c, Namespace: updNS}
	checker := &updates.Checker{
		Feed:                   &updates.FeedClient{BaseURL: feed.URL, HTTPClient: feed.Client()},
		Store:                  store,
		K8sClient:              c,
		Namespace:              updNS,
		ControlPlaneDeployment: updDeploy,
		CurrentVersion:         currentVersion,
	}
	return &Server{UpdateChecker: checker, UpdateStore: store}, feed
}

func helmOwnedDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        updDeploy,
			Namespace:   updNS,
			Annotations: map[string]string{"meta.helm.sh/release-name": "kyber"},
		},
	}
}

func doUpdates(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rr := httptest.NewRecorder()
	s.handleUpdates(rr, r)
	return rr
}

func decodeStatus(t *testing.T, rr *httptest.ResponseRecorder) updates.Status {
	t.Helper()
	var st updates.Status
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v; body=%s", err, rr.Body.String())
	}
	return st
}

func TestUpdates_GetReturnsStatus(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeployment())
	rr := doUpdates(t, s, http.MethodGet, "/api/v1/updates", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	st := decodeStatus(t, rr)
	if st.CurrentVersion != "1.0.1" {
		t.Errorf("currentVersion = %q, want %q", st.CurrentVersion, "1.0.1")
	}
	if st.Policy.Channel != updates.ChannelStable {
		t.Errorf("policy.channel = %q, want the default %q", st.Policy.Channel, updates.ChannelStable)
	}
}

// applySupported must be an explicit false in the contract, not an absent
// field the PWA has to infer from a 404 when someone presses a button.
func TestUpdates_GetAdvertisesThatApplyIsUnsupported(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeployment())
	rr := doUpdates(t, s, http.MethodGet, "/api/v1/updates", "")

	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	v, present := raw["applySupported"]
	if !present {
		t.Fatal("applySupported missing from the response; the PWA needs it to decide whether to offer an install button")
	}
	if v != false {
		t.Errorf("applySupported = %v, want false — this build cannot install anything", v)
	}
}

// There must be no apply route. A 404 here is the guard against someone
// wiring a button to an endpoint that would silently do nothing.
func TestUpdates_NoApplyRouteExists(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeployment())
	rr := doUpdates(t, s, http.MethodPost, "/api/v1/updates/apply", "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("POST /api/v1/updates/apply = %d, want 404 (no apply path in this build)", rr.Code)
	}
}

func TestUpdates_CheckNowPollsSynchronously(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeployment())
	rr := doUpdates(t, s, http.MethodPost, "/api/v1/updates/check", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	st := decodeStatus(t, rr)
	if !st.UpdateAvailable || st.LatestVersion != "1.2.0" {
		t.Errorf("check-now returned %+v; want the freshly-polled result inline, not an ack to poll behind", st)
	}
}

func TestUpdates_PutPolicyPersistsAndReturnsStatus(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeployment())
	rr := doUpdates(t, s, http.MethodPut, "/api/v1/updates/policy",
		`{"mode":"manual","pinnedVersion":"1.0.1"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	st := decodeStatus(t, rr)
	if st.Policy.Mode != updates.ModeManual || st.Policy.PinnedVersion != "1.0.1" {
		t.Errorf("returned policy = %+v, want the saved one", st.Policy)
	}
	// Persisted, not just echoed.
	stored, err := s.UpdateStore.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stored.Mode != updates.ModeManual || stored.PinnedVersion != "1.0.1" {
		t.Errorf("stored policy = %+v, want the saved one", stored)
	}
}

// A PUT carrying one card's worth of fields must not erase the rest. The PWA
// sends what changed, not the whole document.
func TestUpdates_PutPolicyOmittedFieldsAreLeftAlone(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeployment())
	if rr := doUpdates(t, s, http.MethodPut, "/api/v1/updates/policy",
		`{"mode":"notify","window":"0 2 * * *","timeZone":"America/Denver"}`); rr.Code != http.StatusOK {
		t.Fatalf("setup PUT failed: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doUpdates(t, s, http.MethodPut, "/api/v1/updates/policy", `{"mode":"manual"}`); rr.Code != http.StatusOK {
		t.Fatalf("second PUT failed: %d %s", rr.Code, rr.Body.String())
	}
	stored, err := s.UpdateStore.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stored.Window != "0 2 * * *" || stored.TimeZone != "America/Denver" {
		t.Errorf("omitted fields were erased: %+v", stored)
	}
	if stored.Mode != updates.ModeManual {
		t.Errorf("mode = %q, want the updated %q", stored.Mode, updates.ModeManual)
	}
}

// Explicit null is how the PWA clears a pin.
func TestUpdates_PutPolicyNullClearsAField(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeployment())
	if rr := doUpdates(t, s, http.MethodPut, "/api/v1/updates/policy",
		`{"mode":"notify","pinnedVersion":"1.0.1"}`); rr.Code != http.StatusOK {
		t.Fatalf("setup PUT failed: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doUpdates(t, s, http.MethodPut, "/api/v1/updates/policy",
		`{"pinnedVersion":null}`); rr.Code != http.StatusOK {
		t.Fatalf("clearing PUT failed: %d %s", rr.Code, rr.Body.String())
	}
	stored, err := s.UpdateStore.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stored.PinnedVersion != "" {
		t.Errorf("pinnedVersion = %q, want cleared", stored.PinnedVersion)
	}
}

// The refusal must reach the operator with the reason attached — a bare 400
// would leave them guessing why the mode they picked was rejected.
func TestUpdates_PutPolicyRejectsAutoWithAnExplanation(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeployment())
	rr := doUpdates(t, s, http.MethodPut, "/api/v1/updates/policy", `{"mode":"auto"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "silently do nothing") {
		t.Errorf("rejection should explain WHY auto is refused and what to use instead; got %s", body)
	}
}

func TestUpdates_PutPolicyRejectsMainChannelWithAnExplanation(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeployment())
	rr := doUpdates(t, s, http.MethodPut, "/api/v1/updates/policy", `{"channel":"main"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "never find a build") {
		t.Errorf("rejection should explain WHY main is refused; got %s", rr.Body.String())
	}
}

func TestUpdates_PutPolicyRejectsBadJSONAndWrongTypes(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeployment())
	if rr := doUpdates(t, s, http.MethodPut, "/api/v1/updates/policy", `not json`); rr.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON = %d, want 400", rr.Code)
	}
	if rr := doUpdates(t, s, http.MethodPut, "/api/v1/updates/policy", `{"mode":42}`); rr.Code != http.StatusBadRequest {
		t.Errorf("wrong field type = %d, want 400", rr.Code)
	}
}

func TestUpdates_MethodNotAllowed(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeployment())
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/updates"},
		{http.MethodGet, "/api/v1/updates/policy"},
		{http.MethodGet, "/api/v1/updates/check"},
	} {
		if rr := doUpdates(t, s, tc.method, tc.path, ""); rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, rr.Code)
		}
	}
}

// Update checking off (or unconfigured) must be a clean 503, not a panic on a
// nil checker.
func TestUpdates_DisabledReturns503(t *testing.T) {
	s := &Server{}
	for _, path := range []string{"/api/v1/updates", "/api/v1/updates/policy", "/api/v1/updates/check"} {
		rr := doUpdates(t, s, http.MethodGet, path, "")
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with no checker = %d, want 503", path, rr.Code)
		}
	}
}

// The ArgoCD clamp has to survive the trip through the API, since it is what
// the UI keys its install button off.
func TestUpdates_ArgoCDManagedClusterReportsItCannotSelfUpgrade(t *testing.T) {
	argo := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      updDeploy,
			Namespace: updNS,
			Annotations: map[string]string{
				"argocd.argoproj.io/tracking-id": "kyber-razer:apps/Deployment:kyber-system/kyber-control-plane",
			},
		},
	}
	s, _ := updatesServer(t, "1.0.1", argo)
	st := decodeStatus(t, doUpdates(t, s, http.MethodPost, "/api/v1/updates/check", ""))

	if st.CanSelfUpgrade {
		t.Error("canSelfUpgrade = true on an ArgoCD-managed cluster")
	}
	if st.ManagedBy != updates.ManagedByArgoCD {
		t.Errorf("managedBy = %q, want %q", st.ManagedBy, updates.ManagedByArgoCD)
	}
	if st.Reason == "" {
		t.Error("reason is empty; the card needs something honest to show in place of an install button")
	}
	if !st.UpdateAvailable {
		t.Error("updateAvailable = false; an ArgoCD cluster should still be told a release exists")
	}
}
