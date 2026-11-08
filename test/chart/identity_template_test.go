package chart

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIdentityTemplateConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, value, want string }{
		{"default", "", "matty-v/kyber-agent-template"},
		{"custom", "source-org/custom-template", "source-org/custom-template"},
		{"empty", "", "matty-v/kyber-agent-template"},
		{"unset", "null", "matty-v/kyber-agent-template"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := []string{"identityRepo.defaultOwner=destination-org"}
			if tc.value != "" || tc.name == "empty" {
				settings = append(settings, "identityRepo.defaultTemplate="+tc.value)
			}
			env := controlPlaneContainerEnvLiterals(t, helmTemplate(t, settings...))
			if env["KYBER_IDENTITY_REPO_TEMPLATE"] != tc.want || env["KYBER_IDENTITY_REPO_OWNER"] != "destination-org" {
				t.Fatalf("template=%q owner=%q", env["KYBER_IDENTITY_REPO_TEMPLATE"], env["KYBER_IDENTITY_REPO_OWNER"])
			}
		})
	}
}

func TestIdentityTemplateRejectsInvalidConfiguration(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm unavailable")
	}
	for _, value := range []string{`"https://github.com/source/repo"`, "false", "0", "[]", "{}"} {
		t.Run(value, func(t *testing.T) {
			values := filepath.Join(t.TempDir(), "values.yaml")
			if err := os.WriteFile(values, []byte("identityRepo:\n  defaultTemplate: "+value+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			args := append(helmTemplateArgs(t), "-f", values)
			output, err := exec.Command("helm", args...).CombinedOutput()
			if err == nil || !strings.Contains(string(output), "identityRepo.defaultTemplate must be an owner/name") {
				t.Fatalf("invalid template render: %v, %s", err, output)
			}
		})
	}
}
