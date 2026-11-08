package chart

import (
	"os/exec"
	"strings"
	"testing"
)

func TestIdentityTemplateConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, value, want string }{
		{"default", "", "matty-v/kyber-agent-template"},
		{"custom", "source-org/custom-template", "source-org/custom-template"},
		{"empty", "", "matty-v/kyber-agent-template"},
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

func TestIdentityTemplateRejectsInvalidSlug(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm unavailable")
	}
	args := append(helmTemplateArgs(t), "--set", "identityRepo.defaultTemplate=https://github.com/source/repo")
	output, err := exec.Command("helm", args...).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "identityRepo.defaultTemplate must be an owner/name") {
		t.Fatalf("invalid template render: %v, %s", err, output)
	}
}
