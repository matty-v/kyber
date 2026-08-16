package updates

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Node-capability preflight for self-upgrade (kyber#78).
//
// Since agents run in a user namespace and refuse to start without one, a
// cluster below Kubernetes 1.33 / containerd 2.0 can install this version of
// Kyber perfectly well and then schedule no agents at all.
//
// That failure is correct — the alternative is an agent that believes it is
// isolated and is not — but it arrives at the worst possible moment: after the
// upgrade, on every agent at once, with the operator's fleet already down. The
// check belongs one layer up, where it can still be acted on, so this refuses
// the upgrade instead of letting it land.
//
// Deliberately NOT a hard version gate on Kyber as a whole: an operator who has
// set requireUserNamespace=false has already accepted unisolated agents, and
// blocking their upgrade would be a second opinion on a decision they made.

const (
	// minKubernetesMinor is the first Kubernetes minor whose user-namespace
	// support is on by default. Below it the API accepts pod.spec.hostUsers and
	// silently ignores it.
	minKubernetesMajor = 1
	minKubernetesMinor = 33

	// minContainerdMajor is the first containerd major with the user-namespace
	// support Kubernetes needs. Only enforced when the runtime IS containerd.
	minContainerdMajor = 2
)

// nodeCapabilityProblem is one node that cannot run agents.
type nodeCapabilityProblem struct {
	Node    string
	Kubelet string
	Runtime string
	Why     string
}

// requireUserNamespace mirrors the control plane's own setting. When an
// operator has turned the agent-side guard off, they have accepted unisolated
// agents and this preflight must not override that.
func requireUserNamespace() bool {
	v := os.Getenv("KYBER_AGENT_REQUIRE_USER_NAMESPACE")
	if v == "" {
		return true
	}
	return strings.EqualFold(v, "true")
}

// CheckNodeCapability reports an error when a node cannot run agents under this
// version's sandbox requirements.
//
// A node that cannot be read, or a version string that cannot be parsed, is NOT
// treated as a failure. This gate exists to prevent a foreseeable outage, not to
// become one — refusing every upgrade because a version string had an
// unfamiliar shape would be worse than the problem it guards against.
func CheckNodeCapability(ctx context.Context, c client.Client) error {
	if !requireUserNamespace() {
		return nil
	}
	if c == nil {
		return nil
	}

	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		// Can't see the nodes — say nothing rather than block.
		return nil
	}

	var problems []nodeCapabilityProblem
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.Unschedulable {
			continue // nothing schedules here, agents included
		}
		info := node.Status.NodeInfo
		if why, bad := nodeIsTooOld(info.KubeletVersion, info.ContainerRuntimeVersion); bad {
			problems = append(problems, nodeCapabilityProblem{
				Node:    node.Name,
				Kubelet: info.KubeletVersion,
				Runtime: info.ContainerRuntimeVersion,
				Why:     why,
			})
		}
	}
	if len(problems) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "this upgrade would stop every agent on the cluster: %d of %d node(s) cannot run agents. ",
		len(problems), len(nodes.Items))
	b.WriteString("Agents run in a Linux user namespace and refuse to start without one, which needs " +
		"Kubernetes 1.33+ with containerd 2.0+. ")
	for _, p := range problems {
		fmt.Fprintf(&b, "Node %q: %s (kubelet %s, runtime %s). ", p.Node, p.Why, p.Kubelet, p.Runtime)
	}
	b.WriteString("Upgrade the node(s) first, or set agent.security.requireUserNamespace=false " +
		"to accept unisolated agents deliberately — then this check no longer applies.")
	return fmt.Errorf("%s", b.String())
}

// nodeIsTooOld judges one node. Returns a human reason and whether it is a
// problem. Unparseable input is never a problem.
func nodeIsTooOld(kubeletVersion, runtimeVersion string) (string, bool) {
	if major, minor, ok := parseKubeletVersion(kubeletVersion); ok {
		if major < minKubernetesMajor || (major == minKubernetesMajor && minor < minKubernetesMinor) {
			return fmt.Sprintf("Kubernetes %d.%d is below the %d.%d required for user namespaces",
				major, minor, minKubernetesMajor, minKubernetesMinor), true
		}
	}
	// Only containerd has a floor here. Another CRI (CRI-O, for instance) may
	// well support user namespaces, and guessing at its version scheme would
	// block upgrades for no evidence.
	if name, major, ok := parseRuntimeVersion(runtimeVersion); ok && name == "containerd" {
		if major < minContainerdMajor {
			return fmt.Sprintf("containerd %d is below the %d required for user namespaces",
				major, minContainerdMajor), true
		}
	}
	return "", false
}

// parseKubeletVersion pulls major/minor out of strings like "v1.34.6+k3s1".
func parseKubeletVersion(v string) (int, int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return 0, 0, false
	}
	// Drop any build/vendor suffix: "1.34.6+k3s1" -> "1.34.6".
	if i := strings.IndexAny(v, "+-"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// parseRuntimeVersion splits "containerd://2.2.2-bd1.34" into its runtime name
// and major version.
func parseRuntimeVersion(v string) (string, int, bool) {
	v = strings.TrimSpace(v)
	name, rest, found := strings.Cut(v, "://")
	if !found {
		return "", 0, false
	}
	if i := strings.IndexAny(rest, "+-"); i >= 0 {
		rest = rest[:i]
	}
	parts := strings.Split(rest, ".")
	if len(parts) == 0 {
		return "", 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return "", 0, false
	}
	return strings.ToLower(name), major, true
}
