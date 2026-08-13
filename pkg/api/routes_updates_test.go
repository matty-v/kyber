package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

// helmOwnedDeploymentWithImage is helmOwnedDeployment plus a container, which
// the applier needs: the upgrade Job runs the image the control plane is
// running right now.
func helmOwnedDeploymentWithImage(image string) *appsv1.Deployment {
	d := helmOwnedDeployment()
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "control-plane", Image: image}}
	return d
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
		t.Errorf("applySupported = %v, want false — no applier is configured on this install", v)
	}
}

// With no applier the route still EXISTS and answers 503. A 404 would read as
// "this version of Kyber has no such feature", which is a different diagnosis
// from "this install has not enabled it" and sends the operator looking in the
// wrong place.
func TestUpdates_ApplyWithoutAnApplierIs503(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeployment())
	rr := doUpdates(t, s, http.MethodPost, "/api/v1/updates/apply", "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("POST /api/v1/updates/apply = %d, want 503 when self-upgrade is not configured", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "selfUpgrade") {
		t.Errorf("503 body should name the chart value to turn on, got: %s", rr.Body.String())
	}
}

// withApplier wires a configured applier onto a server built by updatesServer,
// sharing its fake client so the control-plane Deployment is visible to both.
func withApplier(s *Server) *Server {
	s.UpdateApplier = &updates.Applier{
		Client:                 s.UpdateStore.Client,
		Namespace:              updNS,
		ControlPlaneDeployment: updDeploy,
		ReleaseName:            "kyber",
		ChartRef:               "oci://ghcr.io/matty-v/charts/kyber",
		ServiceAccount:         "kyber-self-upgrade",
		HealthURL:              "http://kyber-control-plane:8080/healthz",
	}
	s.UpdateChecker.Applier = s.UpdateApplier
	return s
}

func TestUpdates_ApplyStartsAnUpgradeAndReports202(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeploymentWithImage("ghcr.io/matty-v/kyber-control-plane:1.0.1"))
	withApplier(s)

	// Seed the checker so "latest" is known — apply with no body means
	// "install the latest you have seen".
	doUpdates(t, s, http.MethodPost, "/api/v1/updates/check", "")

	rr := doUpdates(t, s, http.MethodPost, "/api/v1/updates/apply", "")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var run updates.Run
	if err := json.Unmarshal(rr.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v; body=%s", err, rr.Body.String())
	}
	if run.TargetVersion != "1.2.0" {
		t.Errorf("targetVersion = %q, want the latest release 1.2.0", run.TargetVersion)
	}
	if run.JobName == "" {
		t.Error("run has no job name; the operator has nothing to look at")
	}

	// The status payload must carry the run, so the PWA can poll one endpoint.
	st := decodeStatus(t, doUpdates(t, s, http.MethodGet, "/api/v1/updates", ""))
	if !st.ApplySupported {
		t.Error("applySupported = false with an applier configured")
	}
	if st.LastRun == nil || st.LastRun.JobName != run.JobName {
		t.Errorf("status.lastRun = %+v, want the run just started", st.LastRun)
	}
}

// An ArgoCD-managed cluster reports applySupported:true (the control plane CAN
// install) and canSelfUpgrade:false (this cluster may not). Collapsing the two
// would tell the operator the feature does not exist.
func TestUpdates_ApplyRefusedOnArgoCDClusterIs409(t *testing.T) {
	argo := helmOwnedDeploymentWithImage("ghcr.io/matty-v/kyber-control-plane:1.0.1")
	argo.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "kyber:apps/Deployment:kyber-system/cp"}
	s, _ := updatesServer(t, "1.0.1", argo)
	withApplier(s)
	doUpdates(t, s, http.MethodPost, "/api/v1/updates/check", "")

	rr := doUpdates(t, s, http.MethodPost, "/api/v1/updates/apply", `{"version":"1.2.0"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "ArgoCD") {
		t.Errorf("refusal should explain ArgoCD, got: %s", rr.Body.String())
	}

	st := decodeStatus(t, doUpdates(t, s, http.MethodGet, "/api/v1/updates", ""))
	if !st.ApplySupported {
		t.Error("applySupported = false; the control plane can install, this cluster just may not")
	}
	if st.CanSelfUpgrade {
		t.Error("canSelfUpgrade = true on an ArgoCD-managed cluster")
	}
}

func TestUpdates_ApplyWithNoKnownVersionIs400(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeploymentWithImage("ghcr.io/matty-v/kyber-control-plane:1.0.1"))
	withApplier(s)
	// No check has run, so no latest version is known.
	rr := doUpdates(t, s, http.MethodPost, "/api/v1/updates/apply", "")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 when there is no version to install; body=%s", rr.Code, rr.Body.String())
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

// The main channel is the canary's, and switching to it is a supported
// operator action now that build.yml publishes a chart per merge.
func TestUpdates_PutPolicyAcceptsMainChannel(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeployment())
	rr := doUpdates(t, s, http.MethodPut, "/api/v1/updates/policy", `{"channel":"main"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if st := decodeStatus(t, rr); st.Policy.Channel != updates.ChannelMain {
		t.Errorf("policy.channel = %q, want %q", st.Policy.Channel, updates.ChannelMain)
	}
}

// A pin the channel would never offer is refused with an explanation rather
// than accepted into a cluster that then silently never moves.
func TestUpdates_PutPolicyRejectsPrereleasePinOnStable(t *testing.T) {
	s, _ := updatesServer(t, "1.0.1", helmOwnedDeployment())
	rr := doUpdates(t, s, http.MethodPut, "/api/v1/updates/policy",
		`{"channel":"stable","pinnedVersion":"1.0.2-25-gfd47d00"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "pre-release") {
		t.Errorf("rejection should explain why; got %s", rr.Body.String())
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
