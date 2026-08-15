//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Agent sandbox security suite — kyber#78 AC3–AC7.
//
// Every check runs inside a live agent. #78 is explicit that PodSpec
// inspection does not satisfy these criteria, and it is right to be: the
// central finding of this work is that Kubernetes accepts
// pod.spec.hostUsers: false on a cluster that cannot honour it and silently
// ignores it, so the rendered spec and the actual boundary can disagree
// completely.
//
// Run against a dev cluster or a canary:
//
//	KYBER_E2E_AGENT_A=alice KYBER_E2E_AGENT_B=bob \
//	  go test -tags e2e -run TestSandbox -timeout 30m ./test/e2e/...

// AC3: the agent holds no host-valid identity or capability.
func TestSandboxSecurity_NoHostValidIdentity(t *testing.T) {
	agent := requireAgent(t, "KYBER_E2E_AGENT_A")

	t.Run("uid_map_is_remapped", func(t *testing.T) {
		// The single most important assertion in this suite. An unremapped map
		// (`0 0 4294967295`) means in-pod root IS node root and every other
		// check in this file is measuring a boundary that does not exist.
		out, err := execInContainer(t, agent, "cat /proc/self/uid_map")
		if err != nil {
			t.Fatalf("reading uid_map: %v", err)
		}
		fields := strings.Fields(out)
		if len(fields) < 3 {
			t.Fatalf("uid_map unreadable: %q", out)
		}
		if fields[0] != "0" {
			t.Fatalf("uid_map does not start at container uid 0: %q", out)
		}
		if fields[1] == "0" {
			t.Fatalf("container uid 0 maps to HOST uid 0 (%q) — the agent is not "+
				"in a user namespace and its SYS_ADMIN is valid against the node", out)
		}
		t.Logf("container uid 0 -> host uid %s (range %s)", fields[1], fields[2])
	})

	t.Run("not_privileged_and_seccomp_active", func(t *testing.T) {
		out, err := execInContainer(t, agent,
			`grep -E '^(CapEff|CapBnd|Seccomp):' /proc/self/status`)
		if err != nil {
			t.Fatalf("reading /proc/self/status: %v", err)
		}
		if strings.Contains(out, "CapBnd:\t0000003fffffffff") {
			t.Error("capability bounding set is the full privileged set")
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "Seccomp:") && strings.HasSuffix(strings.TrimSpace(line), "0") {
				t.Error("Seccomp is 0 — no seccomp filter is applied")
			}
		}
		t.Logf("%s", out)
	})

	t.Run("cannot_create_device_node", func(t *testing.T) {
		// Under a user namespace CAP_MKNOD is namespaced, so mknod itself
		// fails. Outside one it succeeds and the device cgroup denies the
		// open. Either is acceptable; being able to READ the device is not.
		out := mustExecInSandbox(t, agent, `
			t=$(mktemp -d)/sda
			if ! mknod "$t" b 8 0 2>/dev/null; then echo DENIED; exit 0; fi
			if dd if="$t" of=/dev/null bs=512 count=1 2>/dev/null; then echo REACHED; else echo DENIED; fi`)
		if strings.Contains(out, "REACHED") {
			t.Error("agent fabricated a block device node and read from it — host disk is reachable")
		}
	})

	t.Run("no_host_block_devices", func(t *testing.T) {
		out := mustExecInSandbox(t, agent,
			`ls /dev/sd* /dev/nvme* /dev/vd* /dev/mem /dev/kmsg 2>/dev/null | head -5; echo "--"`)
		if devices := strings.TrimSuffix(strings.TrimSpace(out), "--"); strings.TrimSpace(devices) != "" {
			t.Errorf("host devices present in the sandbox: %q", strings.TrimSpace(devices))
		}
	})

	t.Run("cannot_load_kernel_modules", func(t *testing.T) {
		out := mustExecInSandbox(t, agent,
			`if modprobe dummy 2>/dev/null; then echo REACHED; else echo DENIED; fi`)
		if strings.Contains(out, "REACHED") {
			t.Error("agent loaded a kernel module")
		}
	})

	t.Run("cannot_enter_host_namespaces", func(t *testing.T) {
		// PID 1 in the pod is the agent's own entrypoint, so this is really
		// asking whether anything OUTSIDE the pod is visible through /proc.
		out := mustExecInSandbox(t, agent, `
			if [ -e /proc/1/root/etc/machine-id ] && \
			   ! grep -q kyber /proc/1/root/etc/hostname 2>/dev/null && \
			   [ -d /proc/1/root/var/lib/rancher ]; then echo REACHED; else echo DENIED; fi`)
		if strings.Contains(out, "REACHED") {
			t.Error("the node's filesystem is visible through /proc/1/root")
		}
	})

	t.Run("no_runtime_socket", func(t *testing.T) {
		out := mustExecInSandbox(t, agent, `
			for s in /run/containerd/containerd.sock /var/run/docker.sock \
			         /run/k3s/containerd/containerd.sock /var/run/crio/crio.sock; do
			  [ -S "$s" ] && { echo REACHED; exit 0; }
			done; echo DENIED`)
		if strings.Contains(out, "REACHED") {
			t.Error("a container-runtime socket is reachable from the sandbox")
		}
	})
}

// AC5: the agent carries no platform credential.
func TestSandboxSecurity_NoPlatformCredentials(t *testing.T) {
	agent := requireAgent(t, "KYBER_E2E_AGENT_A")

	t.Run("no_service_account_token", func(t *testing.T) {
		out := mustExecInSandbox(t, agent,
			`if [ -r /var/run/secrets/kubernetes.io/serviceaccount/token ]; then echo REACHED; else echo DENIED; fi`)
		if strings.Contains(out, "REACHED") {
			t.Error("agent has a Kubernetes ServiceAccount token")
		}
	})

	t.Run("cannot_reach_cloud_metadata", func(t *testing.T) {
		// The single most valuable target for an escaped agent: node identity,
		// and on GCE an access token for the whole project.
		if !probeDenied(t, agent, tcpProbe("169.254.169.254", 80)) {
			t.Error("cloud metadata service is reachable from the agent")
		}
	})
}

// AC3/AC6: the agent cannot reach the node's control interfaces.
func TestSandboxSecurity_NoControlPlaneReach(t *testing.T) {
	agent := requireAgent(t, "KYBER_E2E_AGENT_A")

	nodeIP := nodeInternalIP(t, podNode(t, agent))

	t.Run("kubelet_unreachable", func(t *testing.T) {
		if !probeDenied(t, agent, tcpProbe(nodeIP, 10250)) {
			t.Errorf("kubelet on %s:10250 is reachable from the agent", nodeIP)
		}
	})

	t.Run("kubernetes_api_unreachable", func(t *testing.T) {
		apiIP := serviceClusterIP(t, "default", "kubernetes")
		if !probeDenied(t, agent, tcpProbe(apiIP, 443)) {
			t.Errorf("Kubernetes API on %s:443 is reachable from the agent", apiIP)
		}
	})

	t.Run("outbound_internet_still_works", func(t *testing.T) {
		// The isolation is worthless if it costs the agent its actual job.
		// AC1 requires ordinary software and source retrieval to keep working.
		out := mustExecInSandbox(t, agent,
			`if curl -sS --max-time 15 -o /dev/null https://github.com; then echo REACHED; else echo DENIED; fi`)
		if strings.Contains(out, "DENIED") {
			t.Error("agent cannot reach the public internet — egress policy is too tight")
		}
	})

	t.Run("dns_still_resolves", func(t *testing.T) {
		out := mustExecInSandbox(t, agent,
			`if getent hosts github.com >/dev/null 2>&1; then echo REACHED; else echo DENIED; fi`)
		if strings.Contains(out, "DENIED") {
			t.Error("agent cannot resolve DNS — egress policy is missing the cluster DNS rule")
		}
	})
}

// AC4: one agent cannot discover or reach another.
func TestSandboxSecurity_AgentToAgentIsolation(t *testing.T) {
	agentA := requireAgent(t, "KYBER_E2E_AGENT_A")
	agentB := requireAgent(t, "KYBER_E2E_AGENT_B")

	nodeA, nodeB := podNode(t, agentA), podNode(t, agentB)
	if nodeA == nodeB {
		// Reported, never silently accepted: #78 asks for both the same-node
		// and the different-node case, and a suite that quietly covers one is
		// how "we could not test it" turns into "it passed".
		t.Logf("NOTE: %s and %s are both on %s — this run proves the SAME-NODE "+
			"case only. The cross-node case is unproven until they are apart.", agentA, agentB, nodeA)
	} else {
		t.Logf("proving the CROSS-NODE case: %s on %s, %s on %s", agentA, nodeA, agentB, nodeB)
	}

	ipB := podIP(t, agentB)

	t.Run("cannot_connect_to_the_other_agent", func(t *testing.T) {
		// Several ports, because a single closed port proves nothing about
		// policy — see the loopback control below.
		for _, port := range []int{22, 80, 8080, 8091, 14004} {
			if !probeDenied(t, agentA, tcpProbe(ipB, port)) {
				t.Errorf("%s reached %s on %s:%d", agentA, agentB, ipB, port)
			}
		}
	})

	t.Run("own_loopback_still_works", func(t *testing.T) {
		// The control that makes the assertion above meaningful: the sidecars
		// talk to the runtime over loopback, so if this fails the ingress
		// policy is over-broad rather than correct.
		out := mustExecInSandbox(t, agentA, `
			(exec 3<>/dev/tcp/127.0.0.1/8091) 2>/dev/null && echo REACHED || echo DENIED`)
		if strings.Contains(out, "DENIED") {
			t.Error("the agent cannot reach its own sidecar over loopback — ingress policy is too broad")
		}
	})

	t.Run("cannot_discover_the_other_agent_by_dns", func(t *testing.T) {
		for _, name := range []string{
			agentPod(agentB),
			agentPod(agentB) + "." + sandboxNamespace() + ".svc.cluster.local",
			agentB + "." + sandboxNamespace() + ".svc.cluster.local",
		} {
			out := mustExecInSandbox(t, agentA,
				fmt.Sprintf(`if getent hosts %s >/dev/null 2>&1; then echo REACHED; else echo DENIED; fi`, name))
			if strings.Contains(out, "REACHED") {
				t.Errorf("%s resolved %s — the other agent is discoverable by DNS", agentA, name)
			}
		}
	})

	t.Run("cannot_read_the_other_agents_volume", func(t *testing.T) {
		out := mustExecInSandbox(t, agentA, fmt.Sprintf(`
			if ls /persist/../%s 2>/dev/null | head -1 | grep -q . ; then echo REACHED; else echo DENIED; fi`,
			agentPod(agentB)))
		if strings.Contains(out, "REACHED") {
			t.Errorf("%s can see %s's volume", agentA, agentB)
		}
	})
}

// AC6: the CNI actually enforces NetworkPolicy.
//
// This is the criterion most likely to be satisfied by a test that proves
// nothing, so it is proved positively: a connection that fails against a closed
// port is indistinguishable from one blocked by policy. The only honest proof
// is to remove the policy and watch the same connection succeed.
//
// Runs in its own throwaway namespace so it never touches a live agent's
// policies.
func TestSandboxSecurity_NetworkPolicyIsEnforced(t *testing.T) {
	requireAgent(t, "KYBER_E2E_AGENT_A") // same skip condition as the rest of the suite

	ns := "kyber-np-proof"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata: {name: %[1]s}
---
apiVersion: v1
kind: Pod
metadata: {name: target, namespace: %[1]s, labels: {role: target}}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  containers:
    - name: c
      image: busybox:1.36
      command: ["sh","-c","echo ok > /tmp/index.html; httpd -f -p 8080 -h /tmp"]
      resources: {requests: {cpu: 10m, memory: 32Mi}}
---
apiVersion: v1
kind: Pod
metadata: {name: client, namespace: %[1]s}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  containers:
    - name: c
      image: busybox:1.36
      command: ["sleep","900"]
      resources: {requests: {cpu: 10m, memory: 32Mi}}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: deny-target, namespace: %[1]s}
spec:
  podSelector: {matchLabels: {role: target}}
  policyTypes: [Ingress]
  ingress: []
`, ns)

	applyManifest(t, ctx, manifest)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = runWithContext(c, "kubectl", "delete", "namespace", ns, "--wait=false")
	})

	if err := runWithContext(ctx, "kubectl", "wait", "--for=condition=Ready",
		"pod/target", "pod/client", "-n", ns, "--timeout=120s"); err != nil {
		t.Fatalf("probe pods never became ready: %v", err)
	}

	targetIP := strings.TrimSpace(kubectlOut(t, ctx, "get", "pod", "target", "-n", ns,
		"-o", "jsonpath={.status.podIP}"))

	fetch := func(from string) string {
		out, _ := kubectlOutErr(t, ctx, "exec", from, "-n", ns, "--",
			"sh", "-c", fmt.Sprintf("timeout 5 wget -qO- http://%s:8080/index.html || echo BLOCKED", targetIP))
		return strings.TrimSpace(out)
	}

	// 1. The port genuinely listens. Without this the rest is meaningless.
	loopback, _ := kubectlOutErr(t, ctx, "exec", "target", "-n", ns, "--",
		"sh", "-c", "timeout 5 wget -qO- http://127.0.0.1:8080/index.html || echo BLOCKED")
	if !strings.Contains(loopback, "ok") {
		t.Fatalf("the probe target is not serving on its own loopback (%q) — "+
			"any 'blocked' result below would be a closed port, not an enforced policy", strings.TrimSpace(loopback))
	}

	// 2. With the policy, a second pod cannot reach it.
	if got := fetch("client"); !strings.Contains(got, "BLOCKED") {
		t.Fatalf("cross-pod request succeeded WITH a deny-ingress policy in place (%q)", got)
	}

	// 3. Remove the policy: the same request must now succeed. This is the
	//    step that distinguishes an enforcing CNI from one that ignores
	//    NetworkPolicy entirely.
	if err := runWithContext(ctx, "kubectl", "delete", "networkpolicy", "deny-target", "-n", ns); err != nil {
		t.Fatalf("deleting the probe policy: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if strings.Contains(fetch("client"), "ok") {
			return // enforcement proven
		}
		if time.Now().After(deadline) {
			t.Fatal("cross-pod request still blocked after the policy was deleted — " +
				"this CNI does not enforce NetworkPolicy, so every network isolation " +
				"result on this cluster is meaningless")
		}
		time.Sleep(3 * time.Second)
	}
}

// AC7: an agent that cannot establish its sandbox does not start.
func TestSandboxSecurity_FailsClosed(t *testing.T) {
	agent := requireAgent(t, "KYBER_E2E_AGENT_A")

	t.Run("persistence_mode_is_durable_root", func(t *testing.T) {
		out, err := execInContainer(t, agent, `cat /persist/kyber/boot-metadata.json`)
		if err != nil {
			t.Fatalf("reading boot metadata: %v", err)
		}
		if strings.Contains(out, "bind-mount-home") {
			t.Error("agent booted in bind-mount-home mode — system-level state is NOT persisting, " +
				"and this mode should no longer be reachable")
		}
		if !strings.Contains(out, `"mode": "rootfs`) {
			t.Errorf("unexpected persistence mode in boot metadata: %s", out)
		}
	})

	t.Run("entrypoint_asserts_the_boundary", func(t *testing.T) {
		// The guard has to be present in the image, not merely have happened
		// to pass on this cluster.
		out, err := execInContainer(t, agent,
			`grep -c 'not running in a user namespace' /usr/local/bin/entrypoint.sh`)
		if err != nil || strings.TrimSpace(out) == "0" {
			t.Error("the entrypoint has no user-namespace assertion — nothing stops this " +
				"agent from running unisolated on a cluster that ignores hostUsers")
		}
	})
}

// --- small kubectl helpers used only by this file ---

func nodeInternalIP(t *testing.T, node string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), sandboxExecTimeout)
	defer cancel()
	return strings.TrimSpace(kubectlOut(t, ctx, "get", "node", node,
		"-o", "jsonpath={.status.addresses[?(@.type==\"InternalIP\")].address}"))
}

func serviceClusterIP(t *testing.T, ns, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), sandboxExecTimeout)
	defer cancel()
	return strings.TrimSpace(kubectlOut(t, ctx, "get", "svc", name, "-n", ns,
		"-o", "jsonpath={.spec.clusterIP}"))
}

func kubectlOut(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	out, err := kubectlOutErr(t, ctx, args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func kubectlOutErr(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.Output()
	return string(out), err
}

func applyManifest(t *testing.T, ctx context.Context, manifest string) {
	t.Helper()
	f, err := os.CreateTemp("", "kyber-e2e-*.yaml")
	if err != nil {
		t.Fatalf("temp manifest: %v", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(manifest); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	f.Close()
	if err := runWithContext(ctx, "kubectl", "apply", "-f", f.Name()); err != nil {
		t.Fatalf("kubectl apply: %v", err)
	}
}

// TestSandboxHarness_ExecInSandboxTargetsTheDurableRoot checks the test harness
// itself before anything trusts it.
//
// execInSandbox has to land in the agent's chroot, on its PersistentVolume. An
// earlier version omitted nsenter's -r flag and landed in the container's
// ephemeral image overlay instead. Everything still "worked" — apt installed,
// files were written — and then none of it survived a pod restart, because none
// of it was ever on the volume. The autonomy suite reported a broken persistence
// mechanism that was in fact fine.
//
// So: write through execInSandbox, then look for the file on the volume from
// the container side. If these two views disagree, every autonomy result in
// this package is meaningless and the suite should say so loudly.
func TestSandboxHarness_ExecInSandboxTargetsTheDurableRoot(t *testing.T) {
	agent := requireAgent(t, "KYBER_E2E_AGENT_A")

	marker := fmt.Sprintf("harness-%d", time.Now().UnixNano())
	path := "/etc/kyber-harness-probe"

	mustExecInSandbox(t, agent, fmt.Sprintf("echo %s > %s", marker, path))
	t.Cleanup(func() { _, _ = execInSandbox(t, agent, "rm -f "+path) })

	// From the container's own root, the durable root is a plain directory.
	out, err := execInContainer(t, agent, "cat /persist/agentroot"+path+" 2>/dev/null || echo MISSING")
	if err != nil {
		t.Fatalf("reading the durable root from the container: %v", err)
	}
	if !strings.Contains(out, marker) {
		t.Fatalf("a write through execInSandbox did not land on the agent's volume "+
			"(got %q). The harness is targeting the container's ephemeral root, so "+
			"every persistence result in this package is invalid.", strings.TrimSpace(out))
	}
}

// TestSandboxSecurity_AgentIsolationAcrossNodes covers the half of AC4 that
// Kyber's own scheduling cannot currently produce.
//
// #78 asks for two hostile agents on the same node AND on different nodes. The
// same-node case is covered by TestSandboxSecurity_AgentToAgentIsolation with
// real agents. The cross-node case cannot be: Kyber's mock compute provider
// permits exactly one Machine, and agent pods are placed on that Machine's
// node, so two agents always land together on a single-provider cluster.
//
// Rather than record the criterion as untested, this proves the part that
// actually differs across nodes — whether the CNI enforces the rendered
// policies when traffic has to traverse the node-to-node overlay rather than
// staying on one bridge. It does that with two pods carrying the real
// `kyber.io/agent` label, so the cluster's actual agent NetworkPolicies select
// them, pinned to different nodes.
//
// What this does NOT prove: that two full Kyber agents, with their sidecars and
// credentials, were scheduled apart. That needs a compute provider that can
// present more than one machine.
func TestSandboxSecurity_AgentIsolationAcrossNodes(t *testing.T) {
	requireAgent(t, "KYBER_E2E_AGENT_A")

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	nodes := strings.Fields(kubectlOut(t, ctx, "get", "nodes",
		"-o", "jsonpath={range .items[*]}{.metadata.name} {end}"))
	if len(nodes) < 2 {
		t.Skipf("cluster has %d node(s); the cross-node case needs two", len(nodes))
	}
	nodeA, nodeB := nodes[0], nodes[1]
	t.Logf("proving cross-node isolation between %s and %s", nodeA, nodeB)

	ns := sandboxNamespace()
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: kyber-xnode-a
  namespace: %[1]s
  labels: {kyber.io/agent: xnode-a}
spec:
  nodeName: %[2]s
  restartPolicy: Never
  automountServiceAccountToken: false
  containers:
    - name: c
      image: busybox:1.36
      command: ["sh","-c","echo ok > /tmp/index.html; httpd -f -p 8080 -h /tmp"]
      resources: {requests: {cpu: 10m, memory: 32Mi}}
---
apiVersion: v1
kind: Pod
metadata:
  name: kyber-xnode-b
  namespace: %[1]s
  labels: {kyber.io/agent: xnode-b}
spec:
  nodeName: %[3]s
  restartPolicy: Never
  automountServiceAccountToken: false
  containers:
    - name: c
      image: busybox:1.36
      command: ["sleep","900"]
      resources: {requests: {cpu: 10m, memory: 32Mi}}
`, ns, nodeA, nodeB)

	applyManifest(t, ctx, manifest)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = runWithContext(c, "kubectl", "delete", "pod", "kyber-xnode-a", "kyber-xnode-b",
			"-n", ns, "--wait=false")
	})

	if err := runWithContext(ctx, "kubectl", "wait", "--for=condition=Ready",
		"pod/kyber-xnode-a", "pod/kyber-xnode-b", "-n", ns, "--timeout=180s"); err != nil {
		t.Fatalf("cross-node probe pods never became ready: %v", err)
	}

	ipA := strings.TrimSpace(kubectlOut(t, ctx, "get", "pod", "kyber-xnode-a", "-n", ns,
		"-o", "jsonpath={.status.podIP}"))

	// The control that makes the denial meaningful: the port must genuinely be
	// serving. A blocked connection to a dead port proves nothing.
	loopback, _ := kubectlOutErr(t, ctx, "exec", "kyber-xnode-a", "-n", ns, "--",
		"sh", "-c", "timeout 5 wget -qO- http://127.0.0.1:8080/index.html || echo BLOCKED")
	if !strings.Contains(loopback, "ok") {
		t.Fatalf("the cross-node probe target is not serving on its own loopback (%q) — "+
			"a denial below would be a closed port, not enforcement", strings.TrimSpace(loopback))
	}

	out, _ := kubectlOutErr(t, ctx, "exec", "kyber-xnode-b", "-n", ns, "--",
		"sh", "-c", fmt.Sprintf("timeout 6 wget -qO- http://%s:8080/index.html || echo BLOCKED", ipA))
	if !strings.Contains(out, "BLOCKED") {
		t.Errorf("an agent-labelled pod on %s reached one on %s (%q) — the agent ingress "+
			"policy is not enforced across nodes", nodeB, nodeA, strings.TrimSpace(out))
	}
}
