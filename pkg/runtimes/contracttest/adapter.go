// Package contracttest provides reusable checks for the Kyber v1 interactive
// adapter boundary. It is test infrastructure, not capability discovery or
// live harness certification. See docs/architecture/agent-harness-contract.md.
package contracttest

import (
	"fmt"
	"path"
	"strings"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
	corev1 "k8s.io/api/core/v1"
)

// AuthCase records an adapter-owned credential mapping. The shared checker
// knows no provider names or credential formats. Env names are never values.
type AuthCase struct {
	Mode             kyberv1.AgentAuthType
	CredentialSuffix string
	RequiredEnvKeys  map[string]string
	ForbiddenEnv     []string
}

// CheckAdapter checks the pod-assembly portion of HC-01/02/03/04/05. Call it
// with two differently named Agents to detect accidental cross-agent wiring.
// It deliberately does not invoke returned commands or infer authenticated
// readiness from a probe. Returned errors contain identifiers, never secrets.
func CheckAdapter(a runtimes.Adapter, agent *kyberv1.Agent, auth AuthCase) []error {
	var failures []error
	check := func(ok bool, id, message string) {
		if !ok {
			failures = append(failures, fmt.Errorf("%s: %s", id, message))
		}
	}
	check(a.Type() == agent.Spec.Runtime, "HC-01", "adapter type differs from runtime")
	check(a.Image() != "", "HC-01", "configured image is missing")
	args := a.EntrypointArgs(agent)
	check(len(args) > 0 && args[0] != "", "HC-01", "launch arguments missing")
	check(a.GracefulShutdownSeconds() > 0, "HC-02", "termination budget must be positive")
	for _, p := range []*corev1.Probe{a.LivenessProbe(), a.ReadinessProbe()} {
		check(p != nil, "HC-02", "lifecycle probe missing")
	}
	for _, p := range []string{a.SessionBriefPath(), a.SessionStatePath()} {
		check(strings.HasPrefix(path.Clean(p), "/persist/"), "HC-03", "continuity path must be under persist")
	}
	vars := map[string]corev1.EnvVar{}
	for _, v := range a.EnvVars(agent) {
		_, exists := vars[v.Name]
		check(!exists, "HC-04", "duplicate environment key")
		vars[v.Name] = v
	}
	model, ok := vars[a.ModelEnvVar()]
	check(ok && model.Value == agent.Spec.Model, "HC-04", "configured model was not passed through")
	check(agent.Spec.Secrets.AuthType == auth.Mode, "HC-05", "auth fixture mode mismatch")
	secret := agent.Name + auth.CredentialSuffix
	check(a.CredentialSecretName(agent) == secret, "HC-05", "recovery credential belongs to wrong agent or mode")
	for name, key := range auth.RequiredEnvKeys {
		v, ok := vars[name]
		check(ok && v.Value == "" && v.ValueFrom != nil && v.ValueFrom.SecretKeyRef != nil && v.ValueFrom.SecretKeyRef.Name == secret && v.ValueFrom.SecretKeyRef.Key == key, "HC-05", "credential must reference the selected agent secret")
	}
	for _, name := range auth.ForbiddenEnv {
		_, exists := vars[name]
		check(!exists, "HC-05", "unselected credential mode injected")
	}
	for _, cmd := range [][]string{a.RestartSessionCommand(), a.CompactSessionCommand(), a.PreStopCommand()} {
		check(cmd == nil || (len(cmd) > 0 && cmd[0] != ""), "HC-04", "optional command must be nil or invocable argv")
	}
	return failures
}
