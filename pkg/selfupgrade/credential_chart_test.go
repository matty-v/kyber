package selfupgrade

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCredentialCompatibilityUsesPublishedImageNotOverrides(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm required for real chart rendering")
	}
	for _, tc := range []struct {
		name, protocol string
		custom, want   bool
	}{
		{"protocol zero remains unavailable", "0", false, false},
		{"compatible fixture", "1", false, true},
		{"custom image cannot borrow claim", "1", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chartDir := t.TempDir()
			if err := os.CopyFS(chartDir, os.DirFS("../../deploy/helm/kyber")); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(chartDir, "Chart.yaml")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var chart map[string]any
			if err := yaml.Unmarshal(raw, &chart); err != nil {
				t.Fatal(err)
			}
			// Test the publisher assertion independently of the current runtime
			// and operator image overrides.
			chart["annotations"] = map[string]string{credentialProtocolAnnotation: tc.protocol, credentialImageAnnotation: officialControlPlaneRepository + ":1.9.0"}
			raw, err = yaml.Marshal(chart)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"template", "kyber", chartDir, "--namespace", "kyber-system"}
			for _, component := range []string{"controlPlane", "nodeAgent", "agentBase", "claudeCode", "codex", "hermes", "statusSidecar", "discordSidecar", "telegramSidecar", "slackSidecar"} {
				args = append(args, "--set-string", "image."+component+".tag=1.9.0")
			}
			if tc.custom {
				args = append(args, "--set-string", "image.controlPlane.repository=custom.invalid/control-plane")
			}
			manifest, err := exec.Command(helm, args...).CombinedOutput()
			if err != nil {
				t.Fatal("helm fixture failed", err)
			} // Do not print manifests.
			_, err = credentialCompatibleDeployment(string(manifest), "kyber-system", "kyber-control-plane")
			if tc.want {
				if err != nil {
					t.Fatal("compatible rendered fixture rejected", err)
				}
			} else if !errors.Is(err, errCredentialCompatibility) {
				t.Fatal("unsafe rendered fixture accepted")
			}
		})
	}
}
