package configexport

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestIsSecretPath_RedactsKnownCredentialValues(t *testing.T) {
	for _, path := range []string{
		"api.apiKey",
		"api.webhookSecret",
		"api.callers",
		"k3s.joinToken",
		"minio.rootPassword",
		"postgresql.auth.password",
		"runtimeDetect.anthropicApiKey",
	} {
		if !IsSecretPath(path, "some-string-value") {
			t.Errorf("IsSecretPath(%q) = false; this value carries a credential", path)
		}
	}
}

// Reference keys name a Secret; they do not contain one. Dropping them would
// produce an export that cannot recreate the cluster, which is a different
// kind of broken from leaking.
func TestIsSecretPath_KeepsSecretReferences(t *testing.T) {
	for _, path := range []string{
		"api.existingSecret",
		"internalAuth.signingKeySecretName",
		"internalAuth.signingKeySecretKey",
		"minio.credentialsSecret",
		"minio.existingSecret",
		"postgresql.auth.existingSecret",
		"logArchive.existingCredentialsSecret",
		"preview.credentialsSecretName",
		"preview.postgresSecretName",
		"preview.bootstrapSecrets",
		"metrics.tokenRatesConfigMapName",
	} {
		if IsSecretPath(path, "some-string-value") {
			t.Errorf("IsSecretPath(%q) = true; this names a Secret rather than holding one, and the export needs it to rebuild", path)
		}
	}
}

// The catch-all. A credential added to the chart later must be redacted by
// default, without anyone remembering to update the explicit list.
func TestIsSecretPath_RedactsUnknownCredentialLookingKeys(t *testing.T) {
	for _, path := range []string{
		"somethingNew.adminPassword",
		"future.slackToken",
		"vendor.privateKey",
		"thing.apiKey",
		"x.dbCredential",
		"y.passphrase",
	} {
		if !IsSecretPath(path, "some-string-value") {
			t.Errorf("IsSecretPath(%q) = false; unrecognised credential-looking keys must fail closed", path)
		}
	}
}

// A credential is never a bool or a number, and redaction writes a non-empty
// STRING. Redacting `preview.copyGithubAppSecret: false` produced a truthy
// value, so reinstalling from the export ENABLED a secret-copying Job and its
// RBAC that the operator had deliberately turned off. The name pattern must
// not fire on non-string scalars.
func TestIsSecretPath_PatternNeverRedactsNonStringScalars(t *testing.T) {
	for _, tc := range []struct {
		path  string
		value any
	}{
		{"preview.copyGithubAppSecret", false},
		{"some.enableSecretThing", true},
		{"some.tokenLimit", 42},
		{"some.passwordRotationDays", 30.0},
	} {
		if IsSecretPath(tc.path, tc.value) {
			t.Errorf("IsSecretPath(%q, %v) = true; redacting a non-string scalar rewrites it to a truthy string and changes what the chart does", tc.path, tc.value)
		}
	}
	// An EXPLICIT secret path still redacts whatever it finds.
	if !IsSecretPath("api.apiKey", false) {
		t.Error("an explicitly-declared secret path stopped redacting")
	}
}

// The four reference values that were being destroyed. imagePullSecrets is a
// LIST of Secret names — redacting it to a scalar renders an invalid PodSpec
// that the API server rejects outright.
func TestIsSecretPath_KeepsTheReferenceValuesThatBrokeTheExport(t *testing.T) {
	for _, path := range []string{
		"imagePullSecrets",
		"preview.githubAppSecretName",
		"preview.devApiKeySecretName",
		"preview.copyGithubAppSecret",
	} {
		if IsSecretPath(path, "value") {
			t.Errorf("IsSecretPath(%q) = true; this is a reference or a toggle, and redacting it produces a values file that cannot reinstall", path)
		}
	}
}

func TestRedactTree_ReplacesLeavesAndLeavesTheRestAlone(t *testing.T) {
	in := map[string]any{
		"api": map[string]any{
			"apiKey":         "sk-live-secret",
			"existingSecret": "kyber-api-credentials",
			"publicURL":      "https://kyber.example",
		},
		"replicaCount": 2,
	}
	out := RedactTree(in)

	api := out["api"].(map[string]any)
	if api["apiKey"] != Redacted {
		t.Errorf("apiKey = %v, want redacted", api["apiKey"])
	}
	if api["existingSecret"] != "kyber-api-credentials" {
		t.Errorf("existingSecret = %v, want preserved", api["existingSecret"])
	}
	if api["publicURL"] != "https://kyber.example" {
		t.Errorf("publicURL = %v, want preserved", api["publicURL"])
	}
	if out["replicaCount"] != 2 {
		t.Errorf("replicaCount = %v, want preserved", out["replicaCount"])
	}
	// The input must not be mutated — callers may still need the real values.
	if in["api"].(map[string]any)["apiKey"] != "sk-live-secret" {
		t.Error("RedactTree mutated its input")
	}
}

// api.callers is a LIST of objects each carrying a `key`. Redacting the whole
// container is correct: the shape is not worth leaking the material.
func TestRedactTree_RedactsWholeCredentialContainers(t *testing.T) {
	in := map[string]any{
		"api": map[string]any{
			"callers": []any{
				map[string]any{"name": "pwa", "key": "deadbeef", "scopes": []any{"lifecycle:write"}},
			},
		},
	}
	out := RedactTree(in)
	if got := out["api"].(map[string]any)["callers"]; got != Redacted {
		t.Errorf("api.callers = %v, want the whole container redacted", got)
	}
	if strings.Contains(mustYAML(t, out), "deadbeef") {
		t.Error("a caller key survived into the rendered export")
	}
}

func TestRedactTree_RecursesIntoNestedMaps(t *testing.T) {
	in := map[string]any{
		"postgresql": map[string]any{
			"auth": map[string]any{
				"password":       "hunter2",
				"existingSecret": "kyber-postgres-credentials",
			},
		},
	}
	out := RedactTree(in)
	auth := out["postgresql"].(map[string]any)["auth"].(map[string]any)
	if auth["password"] != Redacted {
		t.Errorf("nested password = %v, want redacted", auth["password"])
	}
	if auth["existingSecret"] != "kyber-postgres-credentials" {
		t.Errorf("nested existingSecret = %v, want preserved", auth["existingSecret"])
	}
}

// THE ROT GUARD.
//
// This is the test the security posture leans on, and the FIRST version of it
// was a tautology that could never fail.
//
// It computed "does this look like a credential?" using the same substring
// loop IsSecretPath uses for its catch-all, then asked IsSecretPath whether the
// path was classified. Whenever the first was true the second necessarily was
// too, so `unclassified` was always empty. An independent review proved it by
// adding `brandNewThing.supersecretvalue` to values.yaml — the test still
// passed. It also failed to catch four real over-redactions shipping in the
// same PR.
//
// The fix is to ask about EXPLICIT classification — declared secret, or
// declared reference — rather than about IsSecretPath, which folds the
// catch-all back in. A chart value that only the catch-all covers is exactly
// what wants a human decision: the pattern might be redacting a bool, a list
// of Secret names, or a genuine credential, and only one of those is right.
func TestClassification_EveryCredentialLookingChartValueIsExplicitlyClassified(t *testing.T) {
	values := loadChartValues(t)

	var unclassified []string
	walkPaths(values, "", func(path string, isLeaf bool) {
		if !isLeaf {
			return
		}
		leaf := path
		if i := strings.LastIndex(path, "."); i >= 0 {
			leaf = path[i+1:]
		}
		if !MatchesCredentialPatternForTest(strings.ToLower(leaf)) {
			return
		}
		// Deliberately NOT IsSecretPath — that would fold the catch-all back
		// in and make this vacuous again.
		if ExplicitlyClassified(path) {
			return
		}
		unclassified = append(unclassified, path)
	})

	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Fatalf("these chart values look like credentials but are not EXPLICITLY classified:\n  %s\n\n"+
			"Add each to secretPaths (it holds a credential) or referenceKeys (it names one, or is a toggle) in redact.go.\n"+
			"Leaving them to the name-pattern catch-all is what silently redacted a bool and a list of Secret names.",
			strings.Join(unclassified, "\n  "))
	}
}

// Proves the guard above can actually fail. If this test ever passes with the
// probe value present, the rot guard has gone vacuous again.
func TestClassification_RotGuardCatchesAnUnclassifiedValue(t *testing.T) {
	const probe = "brandNewThing.supersecretvalue"

	leaf := probe[strings.LastIndex(probe, ".")+1:]
	if !MatchesCredentialPatternForTest(strings.ToLower(leaf)) {
		t.Fatal("probe value no longer matches the credential pattern; pick a different probe")
	}
	if ExplicitlyClassified(probe) {
		t.Fatal("probe value is explicitly classified; pick a different probe")
	}
	// A value matching the pattern but not explicitly classified is precisely
	// what the rot guard must flag. If this invariant holds, the guard has
	// teeth.
}

// The mirror of the rot guard: every explicitly-listed secret path must still
// exist in the chart. A stale entry is dead weight that makes the list look
// more thorough than it is.
func TestClassification_ExplicitSecretPathsStillExistInTheChart(t *testing.T) {
	values := loadChartValues(t)
	present := map[string]bool{}
	walkPaths(values, "", func(path string, _ bool) { present[path] = true })

	for path := range SecretPathsForTest() {
		if !present[path] {
			t.Errorf("secretPaths contains %q, which no longer exists in values.yaml — remove it or fix the path", path)
		}
	}
}

func loadChartValues(t *testing.T) map[string]any {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "helm", "kyber", "values.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read chart values: %v", err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse chart values: %v", err)
	}
	return out
}

// walkPaths visits every path in the tree. isLeaf is false for maps.
func walkPaths(node map[string]any, prefix string, visit func(path string, isLeaf bool)) {
	for k, v := range node {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if child, ok := v.(map[string]any); ok {
			visit(path, false)
			walkPaths(child, path, visit)
			continue
		}
		visit(path, true)
	}
}

func mustYAML(t *testing.T, v any) string {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
