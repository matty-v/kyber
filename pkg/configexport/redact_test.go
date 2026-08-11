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
		if !IsSecretPath(path) {
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
		if IsSecretPath(path) {
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
		if !IsSecretPath(path) {
			t.Errorf("IsSecretPath(%q) = false; unrecognised credential-looking keys must fail closed", path)
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
// This is the test that matters most. It walks the CHART'S OWN values.yaml and
// requires every credential-looking path to be classified — either redacted,
// or explicitly named as a reference. A new secret value added to the chart
// fails CI here until someone decides which it is.
//
// Without this, the classification above is a snapshot of what was secret on
// the day it was written, and the export quietly starts leaking the next
// credential anyone adds.
func TestClassification_EveryCredentialLookingChartValueIsClassified(t *testing.T) {
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
		lower := strings.ToLower(leaf)

		looksLikeCredential := false
		for _, sub := range credentialSubstrings {
			if strings.Contains(lower, sub) {
				looksLikeCredential = true
				break
			}
		}
		if !looksLikeCredential {
			return
		}
		// Classified if it is redacted, or explicitly kept as a reference.
		if IsSecretPath(path) || referenceKeys[lower] {
			return
		}
		unclassified = append(unclassified, path)
	})

	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Fatalf("these chart values look like credentials but are not classified:\n  %s\n\n"+
			"Add each to secretPaths (it holds a credential) or referenceKeys (it names one) in redact.go.",
			strings.Join(unclassified, "\n  "))
	}
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
