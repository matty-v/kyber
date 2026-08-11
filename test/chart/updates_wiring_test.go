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

// Read-only by construction. If a later change needs to WRITE Deployments it
// should have to update this test deliberately, not inherit the permission.
func TestUpdates_ClusterRoleGrantsGetOnlyForDeployments(t *testing.T) {
	rendered := helmTemplate(t)

	var found bool
	for _, doc := range strings.Split(rendered, "\n---\n") {
		if !strings.Contains(doc, "kind: ClusterRole") {
			continue
		}
		if !strings.Contains(doc, `resources: ["deployments"]`) {
			continue
		}
		found = true
		if !strings.Contains(doc, `verbs: ["get"]`) {
			t.Error("the deployments rule grants more than get; ownership detection only reads")
		}
	}
	if !found {
		t.Error("no ClusterRole rule for apps/deployments — ownership detection would be denied by RBAC and every cluster would report managedBy=unknown")
	}
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
