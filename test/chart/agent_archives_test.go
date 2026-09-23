package chart

import (
	"strings"
	"testing"
)

// Disk export (MAT-87) must work on every installation target without object
// storage, so the default chart renders the built-in archive store and points
// the control plane at it.
func TestAgentArchivesDefaultToBuiltinStore(t *testing.T) {
	out := helmTemplate(t)
	for _, want := range []string{
		"name: kyber-archive-store",
		"/usr/local/bin/kyber-archive-store",
		`name: KYBER_ARCHIVE_STORE_BACKEND
              value: "builtin"`,
		"name: KYBER_ARCHIVE_STORE_URL",
		"name: KYBER_ARCHIVE_TOOL_IMAGE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default render is missing %q", want)
		}
	}
}

func TestAgentArchivesObjectStorageSkipsBuiltinStore(t *testing.T) {
	out := helmTemplateWithValues(t, `
agentArchives:
  storage:
    backend: s3
    s3:
      endpoint: https://s3.example
      bucket: archives
      existingCredentialsSecret: archive-creds
`)
	if strings.Contains(out, "/usr/local/bin/kyber-archive-store") {
		t.Error("s3 backend still renders the built-in archive store")
	}
	for _, want := range []string{"name: KYBER_ARCHIVE_S3_BUCKET", "name: archive-creds"} {
		if !strings.Contains(out, want) {
			t.Errorf("s3 render is missing %q", want)
		}
	}
}

func TestAgentArchivesDisabled(t *testing.T) {
	out := helmTemplateWithValues(t, `
agentArchives:
  enabled: false
`)
	if strings.Contains(out, "/usr/local/bin/kyber-archive-store") {
		t.Error("disabled archives still render the archive store")
	}
	if !strings.Contains(out, `name: KYBER_ARCHIVE_STORE_BACKEND
              value: "disabled"`) {
		t.Error("disabled archives do not tell the control plane")
	}
}
