package updates

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testDeploy = "kyber-control-plane"

func deployWithAnnotations(anns map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testDeploy,
			Namespace:   testNS,
			Annotations: anns,
			// The chart stamps this label on every resource, including on
			// clusters where ArgoCD applied the manifests and no Helm release
			// exists. Present here so the tests exercise the same misleading
			// signal the real cluster carries.
			Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"},
		},
	}
}

func checkerWith(t *testing.T, currentVersion, feedBody string, objs ...client.Object) *Checker {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(feedBody))
	}))
	t.Cleanup(srv.Close)
	c := fake.NewClientBuilder().WithObjects(objs...).Build()
	return &Checker{
		Feed:                   &FeedClient{BaseURL: srv.URL, HTTPClient: srv.Client()},
		Store:                  &Store{Client: c, Namespace: testNS},
		K8sClient:              c,
		Namespace:              testNS,
		ControlPlaneDeployment: testDeploy,
		CurrentVersion:         currentVersion,
	}
}

const releaseBody = `{"tag_name":"v1.2.0","html_url":"https://example.invalid/r","draft":false,"prerelease":false}`

func TestChecker_ReportsAnAvailableUpdate(t *testing.T) {
	c := checkerWith(t, "1.0.1", releaseBody,
		deployWithAnnotations(map[string]string{helmReleaseAnnotation: "kyber"}))
	got := c.Check(context.Background())

	if !got.UpdateAvailable {
		t.Error("UpdateAvailable = false, want true (1.2.0 > 1.0.1)")
	}
	if got.LatestVersion != "1.2.0" {
		t.Errorf("LatestVersion = %q, want %q", got.LatestVersion, "1.2.0")
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty", got.LastError)
	}
	if got.LastChecked.IsZero() {
		t.Error("LastChecked is zero after a successful check")
	}
	if !got.CanSelfUpgrade {
		t.Error("CanSelfUpgrade = false on a Helm-owned deployment")
	}
}

func TestChecker_UpToDateWhenCurrentMatchesLatest(t *testing.T) {
	c := checkerWith(t, "1.2.0", releaseBody)
	if got := c.Check(context.Background()); got.UpdateAvailable {
		t.Error("UpdateAvailable = true when already on the latest release")
	}
}

// A newer local build than the published release (a dev cluster, or one that
// jumped ahead) is not "an update available".
func TestChecker_NoUpdateWhenCurrentIsNewer(t *testing.T) {
	c := checkerWith(t, "2.0.0", releaseBody)
	if got := c.Check(context.Background()); got.UpdateAvailable {
		t.Error("UpdateAvailable = true when the cluster is ahead of the feed")
	}
}

// Dev and local builds have no injected version. Telling a developer their
// laptop is out of date every hour trains them to ignore the card production
// depends on.
func TestChecker_NoUpdateWhenCurrentVersionIsUnparseable(t *testing.T) {
	for _, v := range []string{"", "dev", "1.2.3-7-gabc123"} {
		c := checkerWith(t, v, releaseBody)
		if got := c.Check(context.Background()); got.UpdateAvailable {
			t.Errorf("currentVersion=%q reported an update; want none", v)
		}
	}
}

// A pinned cluster has already decided not to move. Notifying it is noise.
func TestChecker_PinnedClusterReportsNoUpdateAndSaysWhy(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultConfigMapName, Namespace: testNS},
		Data: map[string]string{
			KeyChannel: string(ChannelStable),
			KeyMode:    string(ModeNotify),
			KeyPinned:  "1.0.1",
		},
	}
	c := checkerWith(t, "1.0.1", releaseBody, cm)
	got := c.Check(context.Background())

	if got.UpdateAvailable {
		t.Error("a pinned cluster reported an available update")
	}
	if got.LatestVersion != "1.2.0" {
		t.Errorf("LatestVersion = %q — a pin should still SHOW what's out there", got.LatestVersion)
	}
	if got.Reason == "" {
		t.Error("Reason is empty; a pinned cluster should explain why it isn't moving")
	}
}

// The ArgoCD clamp. This is the case that decides whether the feature is
// trustworthy: an in-cluster upgrade would be reverted by selfHeal, so Kyber
// must report that it cannot act.
func TestChecker_ArgoCDManagedClusterCannotSelfUpgrade(t *testing.T) {
	c := checkerWith(t, "1.0.1", releaseBody,
		deployWithAnnotations(map[string]string{argoTrackingAnnotation: "kyber-razer:apps/Deployment:kyber-system/kyber-control-plane"}))
	got := c.Check(context.Background())

	if got.ManagedBy != ManagedByArgoCD {
		t.Errorf("ManagedBy = %q, want %q", got.ManagedBy, ManagedByArgoCD)
	}
	if got.CanSelfUpgrade {
		t.Fatal("CanSelfUpgrade = true on an ArgoCD-managed cluster — selfHeal would revert the upgrade")
	}
	if got.Reason == "" {
		t.Error("Reason is empty; the UI needs something honest to show instead of an install button")
	}
	// It must still report the update — the operator bumps their deploy repo.
	if !got.UpdateAvailable {
		t.Error("UpdateAvailable = false; an ArgoCD cluster should still be TOLD about a release")
	}
}

// The chart's managed-by:Helm label is on every resource regardless of who
// applied it, so ownership must key off annotations. Getting this backwards
// would tell Kyber it may self-upgrade on exactly the clusters where it must
// not.
func TestChecker_HelmLabelAloneDoesNotImplyHelmOwnership(t *testing.T) {
	c := checkerWith(t, "1.0.1", releaseBody, deployWithAnnotations(nil))
	got := c.Check(context.Background())

	if got.ManagedBy != ManagedByUnknown {
		t.Errorf("ManagedBy = %q, want %q — the Helm LABEL is not ownership", got.ManagedBy, ManagedByUnknown)
	}
	if got.CanSelfUpgrade {
		t.Error("CanSelfUpgrade = true with no ownership annotation; unknown must not authorise mutation")
	}
}

// Both markers present (a cluster mid-migration): ArgoCD wins, because it is
// still the thing that would revert us.
func TestChecker_ArgoCDWinsWhenBothMarkersPresent(t *testing.T) {
	c := checkerWith(t, "1.0.1", releaseBody, deployWithAnnotations(map[string]string{
		argoTrackingAnnotation: "kyber-razer:apps/Deployment:kyber-system/kyber-control-plane",
		helmReleaseAnnotation:  "kyber",
	}))
	if got := c.Check(context.Background()); got.CanSelfUpgrade {
		t.Error("CanSelfUpgrade = true mid-migration; ArgoCD is still reconciling and would revert")
	}
}

// A failed check must not clear a known-good result, or the card flaps between
// "update available" and "up to date" on every transient blip.
func TestChecker_FailedCheckPreservesLastGoodResultAndRecordsError(t *testing.T) {
	c := checkerWith(t, "1.0.1", releaseBody)
	first := c.Check(context.Background())
	if !first.UpdateAvailable {
		t.Fatal("setup: expected an update on the first check")
	}

	// Point the feed at a dead address.
	c.Feed = &FeedClient{BaseURL: "http://127.0.0.1:1", HTTPClient: &http.Client{}}
	second := c.Check(context.Background())

	if second.LastError == "" {
		t.Error("LastError is empty after a failed check")
	}
	if second.LatestVersion != "1.2.0" {
		t.Errorf("LatestVersion = %q, want the last good %q preserved", second.LatestVersion, "1.2.0")
	}
	if !second.UpdateAvailable {
		t.Error("UpdateAvailable was cleared by a transient failure")
	}
}

// Before the first poll, the card must still render something truthful:
// current version and policy, and explicitly NOT "an update is waiting".
func TestChecker_StatusBeforeFirstPollIsUsableAndClaimsNothing(t *testing.T) {
	c := checkerWith(t, "1.0.1", releaseBody)
	got := c.Status(context.Background())

	if got.CurrentVersion != "1.0.1" {
		t.Errorf("CurrentVersion = %q, want %q", got.CurrentVersion, "1.0.1")
	}
	if got.UpdateAvailable {
		t.Error("UpdateAvailable = true before any check has run")
	}
	if got.Policy.Channel == "" {
		t.Error("Policy is empty; the card should show the default policy immediately")
	}
}

// This build reports; it never installs. The contract says so explicitly so
// the PWA renders the right affordance rather than inferring it from a 404.
func TestChecker_ApplyIsNotSupportedInThisBuild(t *testing.T) {
	c := checkerWith(t, "1.0.1", releaseBody)
	if c.Check(context.Background()).ApplySupported {
		t.Error("ApplySupported = true, but no apply path exists in this build")
	}
	if c.Status(context.Background()).ApplySupported {
		t.Error("ApplySupported = true from Status")
	}
}

// A policy edit must be visible without waiting a full cadence — the operator
// changes a setting and expects the card to reflect it.
func TestChecker_StatusRereadsPolicyBetweenPolls(t *testing.T) {
	c := checkerWith(t, "1.0.1", releaseBody)
	c.Check(context.Background())

	if err := c.Store.Save(context.Background(), Policy{
		Channel: ChannelStable, Mode: ModeManual, PinnedVersion: "1.0.1",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := c.Status(context.Background())
	if got.Policy.Mode != ModeManual || got.Policy.PinnedVersion != "1.0.1" {
		t.Errorf("Status policy = %+v, want the freshly-saved one", got.Policy)
	}
	if got.Reason == "" {
		t.Error("Reason should reflect the newly-set pin without waiting for the next poll")
	}
}

// Regression: pinning a cluster must take effect in the SAME response that
// saves the pin. Status re-reads the policy between polls; if it does not also
// recompute UpdateAvailable, the payload contradicts itself — updateAvailable
// true alongside "this cluster is held at 1.0.1".
func TestChecker_StatusRecomputesAvailabilityAfterAPolicyChange(t *testing.T) {
	c := checkerWith(t, "1.0.1", releaseBody)
	if got := c.Check(context.Background()); !got.UpdateAvailable {
		t.Fatal("setup: expected an update before pinning")
	}

	if err := c.Store.Save(context.Background(), Policy{
		Channel: ChannelStable, Mode: ModeNotify, PinnedVersion: "1.0.1",
	}); err != nil {
		t.Fatal(err)
	}
	got := c.Status(context.Background())
	if got.UpdateAvailable {
		t.Error("updateAvailable stayed true after pinning; the payload contradicts its own reason line")
	}
	if got.Reason == "" {
		t.Error("reason is empty after pinning")
	}

	// And the reverse: clearing the pin must restore it without a fresh poll.
	if err := c.Store.Save(context.Background(), Policy{Channel: ChannelStable, Mode: ModeNotify}); err != nil {
		t.Fatal(err)
	}
	if got := c.Status(context.Background()); !got.UpdateAvailable {
		t.Error("clearing the pin left updateAvailable false until the next poll")
	}
}

// A caller going away mid-poll must not persist "context canceled" as the
// cached error — that renders as "we stopped checking" on a healthy cluster
// for the rest of the cadence.
func TestChecker_CallerCancellationDoesNotPoisonTheCachedStatus(t *testing.T) {
	c := checkerWith(t, "1.0.1", releaseBody)
	if got := c.Check(context.Background()); !got.UpdateAvailable {
		t.Fatal("setup: expected a good first check")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Check(ctx)

	got := c.Status(context.Background())
	if got.LastError != "" {
		t.Errorf("LastError = %q after a cancelled caller; want the previous good state left alone", got.LastError)
	}
	if !got.UpdateAvailable || got.LatestVersion != "1.2.0" {
		t.Errorf("a cancelled caller discarded the last good result: %+v", got)
	}
}

// A feed that is unreachable IS a real failure and must still be recorded —
// the cancellation guard above must not swallow genuine errors.
func TestChecker_RealFeedFailureIsStillRecorded(t *testing.T) {
	c := checkerWith(t, "1.0.1", releaseBody)
	c.Check(context.Background())
	c.Feed = &FeedClient{BaseURL: "http://127.0.0.1:1", HTTPClient: &http.Client{}}

	if got := c.Check(context.Background()); got.LastError == "" {
		t.Error("a genuinely unreachable feed recorded no error")
	}
}

// The publish date is what turns "1.2.0 is out" into a decision — an operator
// weighs a release differently at one hour old and at three months old. It is
// parsed off the feed already; these lock down that it survives onto the
// status an operator actually reads.
const datedReleaseBody = `{"tag_name":"v1.2.0","html_url":"https://example.invalid/r","published_at":"2026-08-12T09:30:00Z","draft":false,"prerelease":false}`

func TestChecker_CarriesTheReleasePublishDate(t *testing.T) {
	c := checkerWith(t, "1.0.1", datedReleaseBody)
	got := c.Check(context.Background())

	if got.LatestPublishedAt == nil {
		t.Fatal("LatestPublishedAt = nil, want the feed's published_at")
	}
	if want := "2026-08-12T09:30:00Z"; got.LatestPublishedAt.UTC().Format(time.RFC3339) != want {
		t.Errorf("LatestPublishedAt = %s, want %s", got.LatestPublishedAt.UTC().Format(time.RFC3339), want)
	}
}

// The chart registry the `main` channel reads publishes no date at all. Absent
// has to stay absent: a client that receives 0001-01-01 renders "released 2025
// years ago" unless every one of them special-cases the zero time.
func TestChecker_PublishDateIsAbsentWhenTheSourceHasNone(t *testing.T) {
	c := checkerWith(t, "1.0.1", releaseBody)
	got := c.Check(context.Background())

	if got.LatestPublishedAt != nil {
		t.Errorf("LatestPublishedAt = %v, want nil when the feed carries no published_at", got.LatestPublishedAt)
	}

	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(blob, []byte("latestPublishedAt")) {
		t.Errorf("an absent publish date was serialized anyway: %s", blob)
	}
}

// Switching channels must not leave the previous source's date behind. A
// cluster that moves from releases to main would otherwise show a real release
// date next to a chart build that has none.
func TestChecker_PublishDateIsClearedWhenTheNewSourceHasNone(t *testing.T) {
	c := checkerWith(t, "1.0.1", datedReleaseBody)
	if got := c.Check(context.Background()); got.LatestPublishedAt == nil {
		t.Fatal("setup: expected a publish date from the dated feed")
	}

	undated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(releaseBody))
	}))
	t.Cleanup(undated.Close)
	c.Feed = &FeedClient{BaseURL: undated.URL, HTTPClient: undated.Client()}

	if got := c.Check(context.Background()); got.LatestPublishedAt != nil {
		t.Errorf("LatestPublishedAt = %v after polling a source with no date, want nil", got.LatestPublishedAt)
	}
}
