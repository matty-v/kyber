package configexport

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const relNS = "kyber-system"

// helmSecret builds a Secret in Helm's real storage format: the `release`
// value is base64(gzip(json)). Hand-rolled rather than imported so the test
// pins the ENCODING we claim to read, not just our own round trip.
func helmSecret(t *testing.T, name string, revision int, config map[string]any, chartVersion string) *corev1.Secret {
	t.Helper()
	rel := map[string]any{
		"name":    name,
		"version": revision,
		"config":  config,
		"chart":   map[string]any{"metadata": map[string]any{"version": chartVersion}},
		"info":    map[string]any{"status": "deployed"},
	}
	raw, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	var gzipped bytes.Buffer
	zw := gzip.NewWriter(&gzipped)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1." + name + ".v" + itoa(revision),
			Namespace: relNS,
			// Helm's own labels. The reader narrows on owner=helm, so a
			// fixture without them would not be found — which is exactly the
			// behaviour the comment in release.go describes.
			Labels: map[string]string{
				"owner":   "helm",
				"name":    name,
				"status":  "deployed",
				"version": itoa(revision),
			},
		},
		Type: helmReleaseSecretType,
		Data: map[string][]byte{
			"release": []byte(base64.StdEncoding.EncodeToString(gzipped.Bytes())),
		},
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func readerWith(objs ...client.Object) *Reader {
	return &Reader{
		Client:    fake.NewClientBuilder().WithObjects(objs...).Build(),
		Namespace: relNS,
	}
}

func TestReader_ExportsRedactedValuesFromTheHelmRelease(t *testing.T) {
	cfg := map[string]any{
		"api": map[string]any{
			"apiKey":         "sk-live-do-not-export",
			"existingSecret": "kyber-api-credentials",
			"publicURL":      "https://kyber.example",
		},
		"postgresql": map[string]any{
			"auth": map[string]any{"password": "hunter2"},
		},
	}
	r := readerWith(helmSecret(t, "kyber-canary", 3, cfg, "1.0.1"))

	got, err := r.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Available {
		t.Fatalf("Available = false; reason=%q", got.Reason)
	}
	if got.ReleaseName != "kyber-canary" || got.Revision != 3 || got.ChartVersion != "1.0.1" {
		t.Errorf("metadata = %+v, want kyber-canary/3/1.0.1", got)
	}
	if strings.Contains(got.ValuesYAML, "sk-live-do-not-export") {
		t.Error("the API key survived into the exported values")
	}
	if strings.Contains(got.ValuesYAML, "hunter2") {
		t.Error("the postgres password survived into the exported values")
	}
	if !strings.Contains(got.ValuesYAML, "kyber-api-credentials") {
		t.Error("the Secret NAME was dropped; the export can no longer recreate the cluster")
	}
	if !strings.Contains(got.ValuesYAML, "https://kyber.example") {
		t.Error("a non-secret value was dropped")
	}
}

// The operator has to know what they must supply on restore, rather than
// finding out when the rebuilt cluster fails to start.
func TestReader_ReportsWhatItRedacted(t *testing.T) {
	cfg := map[string]any{
		"api":        map[string]any{"apiKey": "x", "webhookSecret": "y"},
		"postgresql": map[string]any{"auth": map[string]any{"password": "z"}},
	}
	got, err := readerWith(helmSecret(t, "kyber", 1, cfg, "1.0.1")).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"api.apiKey": true, "api.webhookSecret": true, "postgresql.auth.password": true}
	if len(got.RedactedPaths) != len(want) {
		t.Fatalf("RedactedPaths = %v, want %d entries", got.RedactedPaths, len(want))
	}
	for _, p := range got.RedactedPaths {
		if !want[p] {
			t.Errorf("unexpected redacted path %q", p)
		}
	}
}

// Helm keeps every revision. Exporting whatever the API server listed first
// would hand the operator an old config that no longer describes the cluster.
func TestReader_PicksTheNewestRevision(t *testing.T) {
	old := helmSecret(t, "kyber", 1, map[string]any{"replicaCount": 1}, "1.0.0")
	newer := helmSecret(t, "kyber", 2, map[string]any{"replicaCount": 9}, "1.0.1")
	// Revision 10 must beat 9 — string ordering would put "v10" before "v2".
	newest := helmSecret(t, "kyber", 10, map[string]any{"replicaCount": 42}, "1.0.2")

	got, err := readerWith(old, newer, newest).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 10 {
		t.Errorf("Revision = %d, want 10 (numeric ordering, not lexicographic)", got.Revision)
	}
	if !strings.Contains(got.ValuesYAML, "42") {
		t.Errorf("exported the wrong revision's values: %s", got.ValuesYAML)
	}
}

// Every one of Matt's clusters is in this state today — ArgoCD applies the
// manifests and Helm never runs. It must read as a clear explanation, not an
// error.
func TestReader_NoHelmReleaseIsExplainedNotErrored(t *testing.T) {
	got, err := readerWith().Load(context.Background())
	if err != nil {
		t.Fatalf("a non-Helm install should not error: %v", err)
	}
	if got.Available {
		t.Error("Available = true with no release Secret")
	}
	if !strings.Contains(got.Reason, "deploy repo") {
		t.Errorf("reason should point at where the config actually lives; got %q", got.Reason)
	}
}

// Unrelated Secrets in the namespace (there are many — agent creds, tunnel
// creds) must not be mistaken for a release.
func TestReader_IgnoresNonReleaseSecrets(t *testing.T) {
	noise := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dave-oauth", Namespace: relNS},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte("secret")},
	}
	got, err := readerWith(noise).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Available {
		t.Error("an Opaque Secret was treated as a Helm release")
	}
}

// A release installed with no overrides is legitimate; the correct export is
// an empty values file, not a failure.
func TestReader_HandlesReleaseWithNoOverrides(t *testing.T) {
	got, err := readerWith(helmSecret(t, "kyber", 1, nil, "1.0.1")).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Available {
		t.Fatal("Available = false for a release with no overrides")
	}
	if strings.TrimSpace(got.ValuesYAML) != "{}" {
		t.Errorf("ValuesYAML = %q, want an empty mapping", got.ValuesYAML)
	}
}

func TestRevisionFromName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
	}{
		{"sh.helm.release.v1.kyber.v1", 1},
		{"sh.helm.release.v1.kyber.v10", 10},
		{"sh.helm.release.v1.kyber-razer.v247", 247},
		{"garbage", 0},
		{"sh.helm.release.v1.kyber.vX", 0},
	} {
		if got := revisionFromName(tc.name); got != tc.want {
			t.Errorf("revisionFromName(%q) = %d, want %d", tc.name, got, tc.want)
		}
	}
}
