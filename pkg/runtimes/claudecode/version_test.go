package claudecode

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestClaudeCodeVersion_ChartMatchesDockerfile asserts the chart's
// runtime.claudeCode.version matches the Dockerfile's CLAUDE_CODE_VERSION
// ARG default. Chart is the source of truth — CI reads it via yq and
// passes the value as a build-arg. The Dockerfile default is the fallback
// for ad-hoc local builds (`docker build` without --build-arg). The two
// must stay in sync, otherwise a chart bump produces an image whose
// installed version disagrees with what the chart claims is intended.
//
// kyber#175 Phase 2 PR-A.
func TestClaudeCodeVersion_ChartMatchesDockerfile(t *testing.T) {
	root := repoRoot(t)

	chart := readChartClaudeCodeVersion(t, filepath.Join(root, "deploy", "helm", "kyber", "values.yaml"))
	docker := readDockerfileARGDefault(t, filepath.Join(root, "images", "claude-code", "Dockerfile"))

	if chart == "" {
		t.Fatal("chart's runtime.claudeCode.version is empty — kyber#175 Phase 2 PR-A field is missing from values.yaml")
	}
	if docker == "" {
		t.Fatal("Dockerfile's ARG CLAUDE_CODE_VERSION default is empty")
	}
	if chart != docker {
		t.Errorf("claude-code version drift between chart and Dockerfile:\n"+
			"  values.yaml runtime.claudeCode.version = %q\n"+
			"  Dockerfile  ARG CLAUDE_CODE_VERSION    = %q\n"+
			"Chart is the source of truth — bump both together.",
			chart, docker)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatal("repo root not found (no go.mod walking up)")
		}
		wd = parent
	}
}

func readChartClaudeCodeVersion(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var v struct {
		Runtime struct {
			ClaudeCode struct {
				Version string `json:"version"`
			} `json:"claudeCode"`
		} `json:"runtime"`
	}
	if err := yaml.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return strings.TrimSpace(v.Runtime.ClaudeCode.Version)
}

var dockerfileARGRE = regexp.MustCompile(`(?m)^ARG\s+CLAUDE_CODE_VERSION=([^\s#]+)`)

func readDockerfileARGDefault(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := dockerfileARGRE.FindSubmatch(data)
	if m == nil {
		return ""
	}
	// Strip optional surrounding quotes so a future Dockerfile of the form
	// `ARG CLAUDE_CODE_VERSION="2.1.119"` still compares equal to the
	// chart's unquoted YAML scalar.
	return strings.Trim(strings.TrimSpace(string(m[1])), `"`)
}
