//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Helpers for the agent-sandbox suites (kyber#78). These talk to agent pods
// directly rather than through the API, because every criterion in #78 is
// explicit that inspecting the submitted PodSpec does not count — the checks
// have to run inside a live agent.
//
// Configure with:
//
//	KYBER_E2E_NAMESPACE   namespace holding the agents  (default kyber-system)
//	KYBER_E2E_AGENT_A     first agent name              (required)
//	KYBER_E2E_AGENT_B     second agent name             (required for AC4)
//
// Skipped entirely when the agents are not named, so the suite stays usable
// against a dev cluster, a canary, or nothing at all.

const (
	defaultSandboxNamespace = "kyber-system"
	runtimeContainer        = "agent"
	sandboxExecTimeout      = 60 * time.Second
)

func sandboxNamespace() string {
	if ns := os.Getenv("KYBER_E2E_NAMESPACE"); ns != "" {
		return ns
	}
	return defaultSandboxNamespace
}

// requireAgent returns the configured agent name or skips the test. Two agents
// are needed for the cross-agent criteria; one is enough for the rest.
func requireAgent(t *testing.T, envVar string) string {
	t.Helper()
	name := os.Getenv(envVar)
	if name == "" {
		t.Skipf("%s not set — sandbox suite needs a live agent to inspect", envVar)
	}
	return name
}

func agentPod(name string) string { return "agent-" + name }

// execInContainer runs a command in the agent pod's runtime container, in the
// container's ORIGINAL root — the base image, not the agent's durable root.
//
// Use this only for things that are genuinely about the container: the uid map,
// the capability set, the seccomp state, the mount table. For anything about
// the agent's filesystem or the software it installed, use execInSandbox.
func execInContainer(t *testing.T, agent string, script string) (string, error) {
	t.Helper()
	return kubectlExec(t, agent, []string{"/bin/bash", "-c", script})
}

// execInSandbox runs a command inside the agent's chroot — the durable root on
// its PersistentVolume, which is where the agent actually lives.
//
// The `-r` flag is the load-bearing part, and leaving it out produces a test
// suite that passes while proving nothing.
//
// `kubectl exec` enters the container but not the chroot. `nsenter -t 1 -m`
// enters PID 1's MOUNT NAMESPACE — and lands at that namespace's root, which is
// still the container's ephemeral image overlay, because chroot(2) changes a
// process's root without changing the mount namespace. So the obvious
// invocation silently targets the wrong filesystem: an `apt install` through it
// succeeds, writes into the container overlay, and vanishes when the pod is
// replaced. The first run of the autonomy suite did exactly that and reported
// "package did not survive pod recreation" for a mechanism that was working.
//
// `-r` (--root) sets the root to PID 1's root — the chroot — so writes land on
// the volume. `-w` follows its working directory, which also silences a getcwd
// warning. TestSandboxHarness_ExecInSandboxTargetsTheDurableRoot below asserts
// this is still true, so the mistake cannot come back quietly.
func execInSandbox(t *testing.T, agent string, script string) (string, error) {
	t.Helper()
	return kubectlExec(t, agent, []string{
		"nsenter", "-t", "1", "-m", "-p", "-r", "-w", "--", "/bin/bash", "-c", script,
	})
}

func kubectlExec(t *testing.T, agent string, argv []string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), sandboxExecTimeout)
	defer cancel()

	// The runtime container is named "agent"; the sidecars are native
	// sidecars (initContainers with restartPolicy: Always), so `-c` must be
	// explicit or kubectl picks the first init container.
	args := append([]string{
		"exec", "-n", sandboxNamespace(), agentPod(agent), "-c", runtimeContainer, "--",
	}, argv...)

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		return out, fmt.Errorf("kubectl exec %s: %w (stderr: %s)",
			agent, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// mustExecInSandbox fails the test if the command cannot be run at all.
//
// A command that cannot run is NOT a passing isolation result — kyber#78 AC8 is
// explicit that a crash, hang, or unverified result is a failure. Isolation
// assertions must therefore use this and then check the command's own verdict,
// never treat "exec failed" as "the thing was blocked".
func mustExecInSandbox(t *testing.T, agent, script string) string {
	t.Helper()
	out, err := execInSandbox(t, agent, script)
	if err != nil {
		t.Fatalf("could not run probe inside %s: %v", agent, err)
	}
	return out
}

// probeDenied runs a probe that is expected to be denied and reports whether it
// was. The script must print exactly "REACHED" or "DENIED" and exit 0 either
// way, so that a non-zero exit means the probe itself broke rather than that
// the boundary held.
func probeDenied(t *testing.T, agent, script string) bool {
	t.Helper()
	out := mustExecInSandbox(t, agent, script)
	switch {
	case strings.Contains(out, "REACHED"):
		return false
	case strings.Contains(out, "DENIED"):
		return true
	default:
		t.Fatalf("probe returned neither REACHED nor DENIED (got %q) — "+
			"an unverified result is a failure, not a pass", out)
		return false
	}
}

// tcpProbe builds a probe script for a single host:port. Written so the only
// two outcomes are REACHED and DENIED.
func tcpProbe(host string, port int) string {
	return fmt.Sprintf(
		`if timeout 4 bash -c 'echo > /dev/tcp/%s/%d' 2>/dev/null; `+
			`then echo REACHED; else echo DENIED; fi`, host, port)
}

// podIP returns an agent pod's IP as seen by the cluster.
func podIP(t *testing.T, agent string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), sandboxExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "get", "pod", agentPod(agent),
		"-n", sandboxNamespace(), "-o", "jsonpath={.status.podIP}")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl get pod %s: %v", agentPod(agent), err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		t.Fatalf("agent %s has no pod IP", agent)
	}
	return ip
}

// podNode returns the node an agent pod is scheduled on, so the cross-agent
// tests can report whether they proved the same-node or the different-node
// case rather than silently covering only one.
func podNode(t *testing.T, agent string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), sandboxExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "get", "pod", agentPod(agent),
		"-n", sandboxNamespace(), "-o", "jsonpath={.spec.nodeName}")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl get pod %s: %v", agentPod(agent), err)
	}
	return strings.TrimSpace(string(out))
}

// restartAgentPod deletes the agent's pod and waits for its replacement to be
// running again — the durability check that matters most.
//
// Kyber bounds the Failed → Starting restart loop at three attempts (kyber#75),
// and a deliberate pod deletion spends one of them. That budget is shared with
// every other restart the agent has had, so on a cluster where the agent has
// already been recreated a few times this test would otherwise hang until
// timeout and then fail with nothing useful — which is exactly what it did the
// first time it ran twice in a session.
//
// So: if the agent lands in Failed with the retry limit reached, recover it the
// way an operator would (POST .../start clears the count and buys a fresh
// budget) and carry on. The recovery is logged, never silent.
func restartAgentPod(t *testing.T, agent string, timeout time.Duration) {
	t.Helper()
	ns := sandboxNamespace()
	pod := agentPod(agent)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	oldUID := podField(t, agent, "{.metadata.uid}")

	if err := runWithContext(ctx, "kubectl", "delete", "pod", pod, "-n", ns, "--wait=false"); err != nil {
		t.Fatalf("deleting %s: %v", pod, err)
	}

	recovered := false
	err := WaitForCondition(ctx, timeout, fmt.Sprintf("%s recreated and running", pod), func() (bool, error) {
		if agentCRPhase(agent) == "Failed" && !recovered {
			t.Logf("agent %s hit Kyber's restart-retry limit; clearing it via the API "+
				"(this test spends one restart from a budget of 3)", agent)
			c := newAPIClient(t)
			if _, _, err := c.PostRaw("/api/v1/agents/"+agent+"/start", map[string]any{}); err != nil {
				return false, fmt.Errorf("clearing the restart limit for %s: %w", agent, err)
			}
			recovered = true
			return false, nil
		}
		uid := podFieldNoFail(agent, "{.metadata.uid}")
		if uid == "" || uid == oldUID {
			return false, nil
		}
		phase := podFieldNoFail(agent, "{.status.phase}")
		ready := podFieldNoFail(agent, "{.status.containerStatuses[?(@.name==\"agent\")].ready}")
		return phase == "Running" && ready == "true", nil
	})
	if err != nil {
		t.Fatalf("waiting for %s to come back: %v (agent phase: %s)", pod, err, agentCRPhase(agent))
	}
}

// agentCRPhase reads the Agent CR's phase, which is where Kyber records a
// retry-limit failure — the pod is simply absent at that point, so pod status
// alone cannot tell you why nothing is coming back.
func agentCRPhase(agent string) string {
	ctx, cancel := context.WithTimeout(context.Background(), sandboxExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "get", "agents.kyber.io", agent,
		"-n", sandboxNamespace(), "-o", "jsonpath={.status.phase}")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func podField(t *testing.T, agent, jsonpath string) string {
	t.Helper()
	v := podFieldNoFail(agent, jsonpath)
	if v == "" {
		t.Fatalf("pod %s has no value at %s", agentPod(agent), jsonpath)
	}
	return v
}

func podFieldNoFail(agent, jsonpath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), sandboxExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "get", "pod", agentPod(agent),
		"-n", sandboxNamespace(), "-o", "jsonpath="+jsonpath)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
