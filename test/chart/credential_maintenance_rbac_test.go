package chart

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCredentialMaintenanceRoleFollowsPublishedProtocolAndNamespace(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm required")
	}
	for _, protocol := range []string{"0", "1"} {
		t.Run(protocol, func(t *testing.T) {
			fixture := t.TempDir()
			if err := os.CopyFS(fixture, os.DirFS(chartDir(t))); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(fixture, "Chart.yaml")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var chart map[string]any
			if err := yaml.Unmarshal(raw, &chart); err != nil {
				t.Fatal(err)
			}
			chart["annotations"] = map[string]string{"kyber.io/operator-credential-authority-protocol": protocol}
			raw, err = yaml.Marshal(chart)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"template", "kyber", fixture, "--namespace", "credential-contract", "--set", "selfUpgrade.enabled=false"}
			for _, component := range []string{"controlPlane", "nodeAgent", "agentBase", "claudeCode", "codex", "hermes", "statusSidecar", "discordSidecar", "telegramSidecar", "slackSidecar"} {
				args = append(args, "--set-string", "image."+component+".tag=fixture")
			}
			rendered, err := exec.Command(helm, args...).CombinedOutput()
			if err != nil {
				t.Fatal("chart fixture failed", err)
			}
			roles, bindings := 0, 0
			for _, doc := range strings.Split(string(rendered), "\n---\n") {
				var object map[string]any
				if yaml.Unmarshal([]byte(doc), &object) != nil || object == nil {
					continue
				}
				metadata, _ := object["metadata"].(map[string]any)
				name, _ := metadata["name"].(string)
				if !strings.HasSuffix(name, "-credential-maintenance") {
					continue
				}
				if protocol == "0" {
					t.Fatal("protocol zero gained maintenance permissions")
				}
				if metadata["namespace"] != "credential-contract" {
					t.Fatal("maintenance permission escaped namespace")
				}
				switch object["kind"] {
				case "Role":
					roles++
					rules, _ := object["rules"].([]any)
					if len(rules) != 2 {
						t.Fatal("unexpected maintenance grants")
					}
					for _, rawRule := range rules {
						rule := rawRule.(map[string]any)
						resources := rule["resources"].([]any)
						verbs := rule["verbs"].([]any)
						groups := rule["apiGroups"].([]any)
						if containsStr(resources, "jobs") {
							if !containsStr(groups, "batch") || len(resources) != 1 || len(verbs) != 5 {
								t.Fatal("unexpected Job grant")
							}
							for _, verb := range []string{"get", "list", "watch", "create", "update"} {
								if !containsStr(verbs, verb) {
									t.Fatal("required Job grant absent", verb)
								}
							}
						} else if !containsStr(resources, "replicasets") || !containsStr(groups, "apps") || len(resources) != 1 || len(verbs) != 1 || !containsStr(verbs, "get") {
							t.Fatal("unexpected release verification grant")
						}
					}
				case "RoleBinding":
					bindings++
					ref := object["roleRef"].(map[string]any)
					subjects := object["subjects"].([]any)
					if ref["kind"] != "Role" || ref["name"] != name || len(subjects) != 1 {
						t.Fatal("maintenance role binding mismatch")
					}
					subject := subjects[0].(map[string]any)
					if subject["namespace"] != "credential-contract" || subject["name"] != "kyber-control-plane" || subject["kind"] != "ServiceAccount" {
						t.Fatal("maintenance subject mismatch")
					}
				default:
					t.Fatal("unexpected maintenance RBAC kind")
				}
			}
			if protocol == "1" && (roles != 1 || bindings != 1) {
				t.Fatal("protocol one maintenance RBAC missing")
			}
		})
	}
}
