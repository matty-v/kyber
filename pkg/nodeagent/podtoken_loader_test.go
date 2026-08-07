package nodeagent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPodToken_ReadsAndTrims(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pod-token")
	if err := os.WriteFile(path, []byte("signed-token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KYBER_POD_TOKEN_PATH", path)

	if got := LoadPodToken(); got != "signed-token-value" {
		t.Errorf("LoadPodToken = %q, want trimmed %q", got, "signed-token-value")
	}
}

func TestLoadPodToken_MissingFile_Empty(t *testing.T) {
	t.Setenv("KYBER_POD_TOKEN_PATH", filepath.Join(t.TempDir(), "does-not-exist"))
	if got := LoadPodToken(); got != "" {
		t.Errorf("LoadPodToken on missing file = %q, want empty", got)
	}
}

func TestNewResourceReporter_LoadsPodToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pod-token")
	if err := os.WriteFile(path, []byte("node-agent-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KYBER_POD_TOKEN_PATH", path)
	t.Setenv("KYBER_CONTROL_PLANE_INTERNAL_URL", "http://cp:8082")
	t.Setenv("KYBER_NODE_NAME", "node-01")

	r := NewResourceReporter()
	if r == nil {
		t.Fatal("NewResourceReporter returned nil")
	}
	if r.PodToken != "node-agent-token" {
		t.Errorf("reporter PodToken = %q, want %q", r.PodToken, "node-agent-token")
	}
}
