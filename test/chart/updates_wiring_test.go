// Chart wiring for update checking.
//
// The control plane can only tell whether self-upgrade is safe here by reading
// its OWN Deployment's ownership annotations, which needs two things the chart
// must supply: the Deployment's name (it cannot guess `fullname`), and get
// access on apps/deployments. Either one missing degrades silently — ownership
// reads as "unknown", the cluster refuses to self-upgrade, and nothing errors.
// That is the safe direction, but it is a capability loss nobody would notice,
// so it is pinned here.
package chart

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestUpdates_EnabledWiresEnvAndDeploymentName(t *testing.T) {
	rendered := helmTemplate(t)
	deploy := findControlPlaneDeployment(t, rendered)
	env := envNames(container(t, deploy))

	if got := env["KYBER_UPDATES_ENABLED"]; got != "true" {
		t.Errorf("KYBER_UPDATES_ENABLED = %q, want \"true\"", got)
	}
	if got := env["KYBER_UPDATES_REPO"]; got == "" {
		t.Error("KYBER_UPDATES_REPO is empty; the checker would poll the compiled-in default rather than the configured repo")
	}
	// The name must match the Deployment the chart actually renders, or
	// ownership detection Gets a resource that does not exist.
	meta, _ := deploy["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if got := env["KYBER_CONTROL_PLANE_DEPLOYMENT"]; got != name {
		t.Errorf("KYBER_CONTROL_PLANE_DEPLOYMENT = %q, but the Deployment is named %q — ownership detection would read a non-existent resource and silently report managedBy=unknown", got, name)
	}
}

func TestUpdates_DisabledOmitsAllWiring(t *testing.T) {
	rendered := helmTemplate(t, "updates.enabled=false")
	env := envNames(container(t, findControlPlaneDeployment(t, rendered)))

	for _, k := range []string{
		"KYBER_UPDATES_ENABLED",
		"KYBER_UPDATES_REPO",
		"KYBER_UPDATES_CADENCE_SECONDS",
		"KYBER_UPDATE_POLICY_CONFIGMAP",
		"KYBER_CONTROL_PLANE_DEPLOYMENT",
	} {
		if _, present := env[k]; present {
			t.Errorf("%s is set with updates.enabled=false", k)
		}
	}
}

// Read-only, but NOT get-only.
//
// The checker reads through the manager's cached client, and controller-runtime
// registers an informer for any type the cached client touches — informers
// LIST+WATCH. An earlier version of this rule granted get only; that wedges the
// informer in WaitForCacheSync, so the startup check never returns and every
// GET /api/v1/updates hangs. The clusterrole's own secrets and namespaces rules
// document the same trap, one of them citing a production incident.
//
// Read-only is enforced by the ABSENCE of write verbs, which is what this test
// asserts — not by withholding list/watch.
func TestUpdates_ClusterRoleGrantsReadVerbsForDeployments(t *testing.T) {
	rendered := helmTemplate(t)

	var rule map[string]any
	for _, doc := range strings.Split(rendered, "\n---\n") {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil || m == nil {
			continue
		}
		if m["kind"] != "ClusterRole" {
			continue
		}
		rules, _ := m["rules"].([]any)
		for _, r := range rules {
			rm, _ := r.(map[string]any)
			if rm == nil {
				continue
			}
			res, _ := rm["resources"].([]any)
			groups, _ := rm["apiGroups"].([]any)
			if !containsStr(res, "deployments") || !containsStr(groups, "apps") {
				continue
			}
			rule = rm
		}
	}
	if rule == nil {
		t.Fatal("no ClusterRole rule for apps/deployments — ownership detection would be denied by RBAC and every cluster would report managedBy=unknown")
	}

	verbs, _ := rule["verbs"].([]any)
	for _, want := range []string{"get", "list", "watch"} {
		if !containsStr(verbs, want) {
			t.Errorf("the deployments rule is missing %q — the cached client's informer needs LIST+WATCH or every read blocks forever", want)
		}
	}
	for _, forbidden := range []string{"create", "update", "patch", "delete"} {
		if containsStr(verbs, forbidden) {
			t.Errorf("the deployments rule grants %q; ownership detection only reads", forbidden)
		}
	}
}

func containsStr(list []any, want string) bool {
	for _, v := range list {
		if s, _ := v.(string); s == want {
			return true
		}
	}
	return false
}

// The policy ConfigMap must NOT be templated. The settings that govern
// upgrades cannot be re-rendered by an upgrade; the control plane creates this
// resource itself on first write. See pkg/updates/policy.go.
func TestUpdates_PolicyConfigMapIsNotChartTemplated(t *testing.T) {
	rendered := helmTemplate(t)
	for _, doc := range strings.Split(rendered, "\n---\n") {
		if strings.Contains(doc, "kind: ConfigMap") && strings.Contains(doc, "name: kyber-update-policy") {
			t.Fatal("the chart renders the update-policy ConfigMap; a helm upgrade would then re-render the policy that governs upgrades. It must be control-plane-owned.")
		}
	}
}
