package chart

import (
	"os"
	"path/filepath"
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

// archiveStoreDeployment returns the rendered archive-store Deployment.
func archiveStoreDeployment(t *testing.T, out string) string {
	t.Helper()
	for _, doc := range strings.Split(out, "\n---") {
		if strings.Contains(doc, "kind: Deployment") && strings.Contains(doc, "name: kyber-archive-store") {
			return doc
		}
	}
	t.Fatal("no archive-store Deployment rendered")
	return ""
}

// The store's volume is node-local on k3s (local-path) and zonal elsewhere,
// so the store must not land on an agent Machine node that can be deleted.
// Unset, it follows the control plane's placement.
func TestArchiveStoreFollowsControlPlanePlacement(t *testing.T) {
	dep := archiveStoreDeployment(t, helmTemplate(t))
	for _, want := range []string{"nodeSelector:", `node-role.kubernetes.io/control-plane: "true"`, "tolerations:"} {
		if !strings.Contains(dep, want) {
			t.Errorf("default archive store lacks %q:\n%s", want, dep)
		}
	}

	dep = archiveStoreDeployment(t, helmTemplateWithValues(t, `
agentArchives:
  storage:
    builtin:
      nodeSelector:
        kyber.io/role: archive
`))
	if !strings.Contains(dep, "kyber.io/role: archive") || strings.Contains(dep, `node-role.kubernetes.io/control-plane: "true"`) {
		t.Errorf("explicit archive-store nodeSelector not used:\n%s", dep)
	}
}

// EKS has no default StorageClass: the preset must name one for the archive
// volume and keep the store on the platform pool.
func TestEKSPresetPlacesArchiveStore(t *testing.T) {
	preset, err := os.ReadFile(filepath.Join(chartDir(t), "examples", "values-eks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := helmTemplateWithValues(t, string(preset))
	dep := archiveStoreDeployment(t, out)
	if !strings.Contains(dep, "kyber.io/role: platform") || strings.Contains(dep, `node-role.kubernetes.io/control-plane: "true"`) {
		t.Errorf("EKS archive store not on the platform pool:\n%s", dep)
	}
	for _, doc := range strings.Split(out, "\n---") {
		if strings.Contains(doc, "kind: PersistentVolumeClaim") && strings.Contains(doc, "name: kyber-archive-store") {
			if !strings.Contains(doc, `storageClassName: "kyber-ebs"`) {
				t.Errorf("EKS archive volume has no StorageClass:\n%s", doc)
			}
			return
		}
	}
	t.Fatal("no archive-store PVC rendered")
}
