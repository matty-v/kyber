package agent

import (
	"os"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
	"github.com/matty-v/kyber/pkg/runtimes/claudecode"
)

// TestMain sets the KYBER_CONTROL_PLANE_INTERNAL_URL env var for the entire
// package so pod_builder tests exercise realistic URLs rather than empty strings.
func TestMain(m *testing.M) {
	os.Setenv("KYBER_CONTROL_PLANE_INTERNAL_URL", "http://test-control-plane:8082")
	os.Exit(m.Run())
}

// testAgent returns a minimal Agent for pod builder tests.
func testAgent() *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dave",
			Namespace: "kyber-system",
		},
		Spec: kyberv1.AgentSpec{
			Machine: "node-01",
			Runtime: "claude-code",
			Model:   "claude-sonnet-4",
			Resources: kyberv1.AgentResources{
				CPU:    resource.MustParse("1"),
				Memory: resource.MustParse("2Gi"),
				Disk:   resource.MustParse("50Gi"),
			},
			Secrets: kyberv1.AgentSecrets{
				AuthType: kyberv1.AgentAuthTypeOAuth,
			},
		},
	}
}

// testAdapter returns a stub runtimes.Adapter with known values for assertions.
func testAdapter() pkgruntimes.Adapter {
	liveness := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"pgrep", "-f", "claude"},
			},
		},
		InitialDelaySeconds: 30,
		PeriodSeconds:       30,
		FailureThreshold:    3,
	}
	readiness := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"pgrep", "-f", "claude"},
			},
		},
		InitialDelaySeconds: 15,
		PeriodSeconds:       10,
		FailureThreshold:    3,
	}
	return pkgruntimes.NewStubAdapter(
		"ghcr.io/matty-v/agent-claude-code:latest",
		[]string{"/usr/local/bin/start-claude.sh"},
		[]corev1.EnvVar{
			{Name: "CLAUDE_MODEL", Value: "claude-sonnet-4"},
		},
		[]pkgruntimes.SecretMount{
			{
				Name:          "telegram-token",
				MountPath:     "/secrets/telegram",
				ProviderClass: "dave-telegram-token",
			},
		},
		liveness,
		readiness,
		30,
		"/persist/session-brief.json",
		"/persist/session-state.json",
		"CLAUDE_MODEL",
	)
}

// derefProfile renders a profile pointer for a failure message. Printing the
// pointer itself shows an address, which tells whoever hits this nothing about
// what was actually set.
func derefProfile(p *corev1.AppArmorProfileType) string {
	if p == nil {
		return "<unset>"
	}
	return string(*p)
}

func TestBuildPodSpec_AgentIsolation(t *testing.T) {
	tests := []struct {
		name           string
		privileged     string
		userNamespaces string
		seccomp        string
		wantPrivileged bool
		wantHostUsers  *bool
		wantSeccomp    *corev1.SeccompProfileType
		apparmor       string
		wantAppArmor   *corev1.AppArmorProfileType
	}{
		{
			// kyber#78: user namespaces are the DEFAULT, not an opt-in. Without
			// them the agent's SYS_ADMIN is valid against the node.
			name:           "secure defaults",
			wantPrivileged: false,
			wantHostUsers:  ptrTo(false),
			wantSeccomp:    ptrTo(corev1.SeccompProfileTypeRuntimeDefault),
			// Unconfined is REQUIRED, not a relaxation. containerd's default
			// AppArmor profile denies mount(2), so a confined agent cannot bind
			// its durable root and fail-closes on every boot. This took all 8
			// falcon agents down on v1.0.6.
			wantAppArmor: ptrTo(corev1.AppArmorProfileTypeUnconfined),
		},
		{
			name:           "unconfined seccomp compatibility fallback",
			seccomp:        "unconfined",
			wantPrivileged: false,
			wantHostUsers:  ptrTo(false),
			wantSeccomp:    ptrTo(corev1.SeccompProfileTypeUnconfined),
			wantAppArmor:   ptrTo(corev1.AppArmorProfileTypeUnconfined),
		},
		{
			name:           "user namespaces explicitly enabled",
			userNamespaces: "true",
			wantPrivileged: false,
			wantHostUsers:  ptrTo(false),
			wantSeccomp:    ptrTo(corev1.SeccompProfileTypeRuntimeDefault),
			wantAppArmor:   ptrTo(corev1.AppArmorProfileTypeUnconfined),
		},
		{
			// The conspicuous, deliberate opt-out. An operator can still run
			// agents on a cluster that cannot do user namespaces, but only by
			// saying so — nothing falls back to this on its own.
			name:           "user namespaces explicitly disabled",
			userNamespaces: "false",
			wantPrivileged: false,
			wantHostUsers:  nil,
			wantSeccomp:    ptrTo(corev1.SeccompProfileTypeRuntimeDefault),
			// Still unconfined: dropping the user namespace does not restore the
			// ability to mount. Verified on falcon — the bind failed with
			// hostUsers unset too, so AppArmor is the only thing in the way.
			wantAppArmor: ptrTo(corev1.AppArmorProfileTypeUnconfined),
		},
		{
			name:           "legacy privileged rollback disables user namespaces",
			privileged:     "true",
			userNamespaces: "true",
			seccomp:        "RuntimeDefault",
			wantPrivileged: true,
			wantHostUsers:  nil,
			wantSeccomp:    nil,
			// A privileged container is already AppArmor-unconfined via the
			// runtime; setting the field would be noise.
			wantAppArmor: nil,
		},
		{
			// The deliberate opt-back-in, for a cluster whose AppArmor policy
			// permits mount. Anything other than "Unconfined" confines.
			name:           "apparmor confinement explicitly restored",
			apparmor:       "RuntimeDefault",
			wantPrivileged: false,
			wantHostUsers:  ptrTo(false),
			wantSeccomp:    ptrTo(corev1.SeccompProfileTypeRuntimeDefault),
			wantAppArmor:   ptrTo(corev1.AppArmorProfileTypeRuntimeDefault),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KYBER_AGENT_PRIVILEGED", tc.privileged)
			t.Setenv("KYBER_AGENT_USER_NAMESPACES", tc.userNamespaces)
			t.Setenv("KYBER_AGENT_SECCOMP_PROFILE", tc.seccomp)
			t.Setenv("KYBER_AGENT_APPARMOR_PROFILE", tc.apparmor)

			pod, err := BuildPodSpec(testAgent(), testAdapter(), "node-01")
			if err != nil {
				t.Fatalf("BuildPodSpec: %v", err)
			}
			securityContext := pod.Containers[0].SecurityContext
			if securityContext == nil || securityContext.Privileged == nil {
				t.Fatal("agent security context must set privileged explicitly")
			}
			if got := *securityContext.Privileged; got != tc.wantPrivileged {
				t.Errorf("privileged = %v, want %v", got, tc.wantPrivileged)
			}
			if !reflect.DeepEqual(pod.HostUsers, tc.wantHostUsers) {
				t.Errorf("hostUsers = %v, want %v", pod.HostUsers, tc.wantHostUsers)
			}
			var gotSeccomp *corev1.SeccompProfileType
			if securityContext.SeccompProfile != nil {
				gotSeccomp = &securityContext.SeccompProfile.Type
			}
			if !reflect.DeepEqual(gotSeccomp, tc.wantSeccomp) {
				t.Errorf("seccomp = %v, want %v", gotSeccomp, tc.wantSeccomp)
			}
			var gotAppArmor *corev1.AppArmorProfileType
			if securityContext.AppArmorProfile != nil {
				gotAppArmor = &securityContext.AppArmorProfile.Type
			}
			if !reflect.DeepEqual(gotAppArmor, tc.wantAppArmor) {
				t.Errorf("apparmor = %s, want %s (containerd's default profile denies mount(2); a confined agent cannot bind its durable root)",
					derefProfile(gotAppArmor), derefProfile(tc.wantAppArmor))
			}
			if got := securityContext.Capabilities.Add; !reflect.DeepEqual(got, []corev1.Capability{"SYS_ADMIN"}) {
				t.Errorf("added capabilities = %v, want [SYS_ADMIN]", got)
			}
			if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
				t.Error("agent pod must disable ServiceAccount token automount")
			}
		})
	}
}

// TestBuildPodSpec_PreStopHook verifies the container's preStop lifecycle hook
// is wired from the adapter: absent when the adapter returns nil, and set to the
// adapter's exact argv otherwise. The real Claude Code adapter returns the
// Telegram getUpdates slot-release hook (see runtimes.Adapter.PreStopCommand).
func TestBuildPodSpec_PreStopHook(t *testing.T) {
	agent := testAgent()

	// Stub adapter returns no preStop command → container has no Lifecycle.
	pod, err := BuildPodSpec(agent, testAdapter(), "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec (stub): %v", err)
	}
	if lc := pod.Containers[0].Lifecycle; lc != nil {
		t.Errorf("stub adapter returns nil PreStopCommand; container.Lifecycle should be nil, got %+v", lc)
	}

	// Real Claude Code adapter returns the slot-release hook → the container's
	// preStop must be wired to exactly that argv.
	cc := claudecode.NewClaudeCodeAdapter()
	pod, err = BuildPodSpec(agent, cc, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec (claude-code): %v", err)
	}
	lc := pod.Containers[0].Lifecycle
	if lc == nil || lc.PreStop == nil || lc.PreStop.Exec == nil {
		t.Fatalf("claude-code adapter: container must have a preStop exec hook; got %+v", lc)
	}
	if !reflect.DeepEqual(lc.PreStop.Exec.Command, cc.PreStopCommand()) {
		t.Errorf("preStop command mismatch:\n got  %v\n want %v", lc.PreStop.Exec.Command, cc.PreStopCommand())
	}
}

// TestBuildPodSpec_SessionResumeEnv verifies spec.sessionResume=true reaches
// the agent container as KYBER_SESSION_RESUME=true (kyber#118) — the launch
// scripts key their resume-vs-fresh decision on it.
func TestBuildPodSpec_SessionResumeEnv(t *testing.T) {
	agent := testAgent()
	agent.Spec.SessionResume = true
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}
	for _, e := range pod.Containers[0].Env {
		if e.Name == "KYBER_SESSION_RESUME" {
			if e.Value != "true" {
				t.Errorf("KYBER_SESSION_RESUME: got %q, want %q", e.Value, "true")
			}
			return
		}
	}
	t.Error("KYBER_SESSION_RESUME env var not found")
}

func TestBuildPodSpec_Image(t *testing.T) {
	agent := testAgent()
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	if len(pod.Containers) == 0 {
		t.Fatal("pod has no containers")
	}
	got := pod.Containers[0].Image
	want := "ghcr.io/matty-v/agent-claude-code:latest"
	if got != want {
		t.Errorf("container image: got %q, want %q", got, want)
	}
}

func TestBuildPodSpec_EnvVars(t *testing.T) {
	agent := testAgent()
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	envMap := make(map[string]string)
	for _, e := range pod.Containers[0].Env {
		envMap[e.Name] = e.Value
	}

	// AGENT_NAME must always be set from the Agent's name.
	if v, ok := envMap["AGENT_NAME"]; !ok {
		t.Error("AGENT_NAME env var not found")
	} else if v != "dave" {
		t.Errorf("AGENT_NAME: got %q, want %q", v, "dave")
	}
	if v, ok := envMap["KYBER_STARTUP_PROMPT"]; !ok {
		t.Error("KYBER_STARTUP_PROMPT env var not found")
	} else if v != "" {
		t.Errorf("KYBER_STARTUP_PROMPT: got %q, want empty", v)
	}

	// kyber#118: rendered even when disabled, so the launch scripts see an
	// explicit "false" rather than distinguishing unset from disabled.
	if v, ok := envMap["KYBER_SESSION_RESUME"]; !ok {
		t.Error("KYBER_SESSION_RESUME env var not found")
	} else if v != "false" {
		t.Errorf("KYBER_SESSION_RESUME: got %q, want %q (spec default)", v, "false")
	}

	// Adapter env vars must be injected.
	if _, ok := envMap["CLAUDE_MODEL"]; !ok {
		t.Error("CLAUDE_MODEL env var not found (from adapter)")
	}

	// KYBER_REFRESH_TOKEN_URL must be injected with the agent-specific URL.
	// kyber#257 migrated the in-pod token-reporter / credential-syncer to
	// fan through the sidecar, but start-claude.sh's boot-time OAuth push
	// (which runs before the sidecar is up) still uses this env var.
	wantRefreshURL := "http://test-control-plane:8082/internal/agents/dave/refresh-token"
	if v, ok := envMap["KYBER_REFRESH_TOKEN_URL"]; !ok {
		t.Error("KYBER_REFRESH_TOKEN_URL env var not found")
	} else if v != wantRefreshURL {
		t.Errorf("KYBER_REFRESH_TOKEN_URL: got %q, want %q", v, wantRefreshURL)
	}

	// TZ should be omitted when KYBER_AGENT_TIMEZONE is unset — the test
	// helper does not set the env var, so the pod env should not contain TZ.
	if v, ok := envMap["TZ"]; ok {
		t.Errorf("TZ env var unexpectedly present (got %q); should be omitted when KYBER_AGENT_TIMEZONE is unset", v)
	}
}

// TestBuildPodSpec_TZEnvVar_SetWhenConfigured verifies that the controller
// injects the pod-wide TZ env var when KYBER_AGENT_TIMEZONE is set on the
// control plane. This is the kyber#177 follow-up: CRON_TZ in /etc/cron.d/
// is silently ignored by Debian vixie cron, so the timezone has to come in
// via the daemon's environment instead.
func TestBuildPodSpec_TZEnvVar_SetWhenConfigured(t *testing.T) {
	t.Setenv("KYBER_AGENT_TIMEZONE", "America/Denver")

	agent := testAgent()
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	envMap := make(map[string]string)
	for _, e := range pod.Containers[0].Env {
		envMap[e.Name] = e.Value
	}

	if v, ok := envMap["TZ"]; !ok {
		t.Error("TZ env var not found; cron will fall back to UTC")
	} else if v != "America/Denver" {
		t.Errorf("TZ: got %q, want %q", v, "America/Denver")
	}
}

func TestBuildPodSpec_PVMount(t *testing.T) {
	agent := testAgent()
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	// Check that the persist volume mount exists at /persist.
	var found bool
	for _, vm := range pod.Containers[0].VolumeMounts {
		if vm.Name == "persist" && vm.MountPath == "/persist" {
			found = true
			break
		}
	}
	if !found {
		t.Error("PV mount at /persist not found in container volumeMounts")
	}

	// Check that the corresponding PVC volume is declared.
	var pvFound bool
	for _, v := range pod.Volumes {
		if v.Name == "persist" && v.PersistentVolumeClaim != nil {
			wantClaimName := "agent-dave-pv"
			if v.PersistentVolumeClaim.ClaimName != wantClaimName {
				t.Errorf("PVC claimName: got %q, want %q", v.PersistentVolumeClaim.ClaimName, wantClaimName)
			}
			pvFound = true
			break
		}
	}
	if !pvFound {
		t.Error("PVC volume 'persist' not found in pod volumes")
	}
}

func TestBuildPodSpec_SecretMounts(t *testing.T) {
	agent := testAgent()
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	// Check that the secret volume mount exists.
	var mountFound bool
	for _, vm := range pod.Containers[0].VolumeMounts {
		if vm.Name == "telegram-token" && vm.MountPath == "/secrets/telegram" && vm.ReadOnly {
			mountFound = true
			break
		}
	}
	if !mountFound {
		t.Error("secret mount 'telegram-token' not found in container volumeMounts")
	}

	// Check that the CSI secret volume is declared.
	var volFound bool
	for _, v := range pod.Volumes {
		if v.Name == "telegram-token" && v.CSI != nil {
			if v.CSI.Driver != "secrets-store.csi.k8s.io" {
				t.Errorf("CSI driver: got %q, want secrets-store.csi.k8s.io", v.CSI.Driver)
			}
			volFound = true
			break
		}
	}
	if !volFound {
		t.Error("CSI secret volume 'telegram-token' not found in pod volumes")
	}
}

func TestBuildPodSpec_NodeAffinity(t *testing.T) {
	agent := testAgent()
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	if pod.Affinity == nil || pod.Affinity.NodeAffinity == nil {
		t.Fatal("pod has no node affinity set")
	}

	required := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil || len(required.NodeSelectorTerms) == 0 {
		t.Fatal("no required node selector terms found")
	}

	var found bool
	for _, term := range required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == "kubernetes.io/hostname" &&
				expr.Operator == corev1.NodeSelectorOpIn &&
				len(expr.Values) == 1 && expr.Values[0] == "node-01" {
				found = true
			}
		}
	}
	if !found {
		t.Error("node affinity targeting 'node-01' via kubernetes.io/hostname not found")
	}
}

func TestBuildPodSpec_SecurityContext(t *testing.T) {
	agent := testAgent()
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	sc := pod.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("container has no securityContext")
	}
	if sc.Capabilities == nil {
		t.Fatal("securityContext has no capabilities")
	}

	var hasSysAdmin bool
	for _, cap := range sc.Capabilities.Add {
		if cap == corev1.Capability("SYS_ADMIN") {
			hasSysAdmin = true
			break
		}
	}
	if !hasSysAdmin {
		t.Error("SYS_ADMIN capability not found in container securityContext.capabilities.add")
	}
}

func TestBuildPodSpec_InitContainer(t *testing.T) {
	agent := testAgent()
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	if len(pod.InitContainers) == 0 {
		t.Fatal("pod has no init containers (expected session-brief init container)")
	}

	var found bool
	for _, ic := range pod.InitContainers {
		if ic.Name == "session-brief" {
			found = true
			// Verify it mounts the persist volume.
			var mountFound bool
			for _, vm := range ic.VolumeMounts {
				if vm.Name == "persist" && vm.MountPath == "/persist" {
					mountFound = true
					break
				}
			}
			if !mountFound {
				t.Error("session-brief init container does not mount /persist")
			}
			// Verify it has a command.
			if len(ic.Command) == 0 && len(ic.Args) == 0 {
				t.Error("session-brief init container has no command or args")
			}
			// Verify the briefURL contains the injected control-plane URL.
			wantURLSubstr := "http://test-control-plane:8082/internal/agents/dave/session-brief"
			if len(ic.Args) > 0 && ic.Args[0] != "" {
				arg := ic.Args[0]
				if len(arg) > 0 {
					found2 := false
					for i := 0; i+len(wantURLSubstr) <= len(arg); i++ {
						if arg[i:i+len(wantURLSubstr)] == wantURLSubstr {
							found2 = true
							break
						}
					}
					if !found2 {
						t.Errorf("session-brief init container arg does not contain %q; got: %q", wantURLSubstr, arg)
					}
				}
			}
		}
	}
	if !found {
		t.Error("session-brief init container not found")
	}
}

// kyber#78: agent pods get NO host device and NO hostPath volume at all.
//
// This replaced the old TestBuildPodSpec_FuseDev, which asserted the opposite.
// /dev/fuse went away with the overlay: persistence is now a durable directory
// on the agent's own PVC, and a hostPath device could not be delivered to a
// user-namespaced pod anyway — the kubelet idmaps hostPath volumes and
// devtmpfs rejects idmapped mounts, so the pod failed to start.
func TestBuildPodSpec_NoHostDevices(t *testing.T) {
	pod, err := BuildPodSpec(testAgent(), testAdapter(), "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	for _, v := range pod.Volumes {
		if v.HostPath != nil {
			t.Errorf("agent pod declares hostPath volume %q -> %s; agents must not reach the node filesystem",
				v.Name, v.HostPath.Path)
		}
	}
	for _, vm := range pod.Containers[0].VolumeMounts {
		if strings.HasPrefix(vm.MountPath, "/dev/") {
			t.Errorf("agent container mounts %q at %s; agents get no host devices", vm.Name, vm.MountPath)
		}
	}
}

// The entrypoint must be told which persistence model to run and whether an
// unisolated start is acceptable. It re-derives the boundary from
// /proc/self/uid_map regardless, but these are what distinguish an operator's
// deliberate opt-out from an unnoticed regression.
func TestBuildPodSpec_PersistenceAndSandboxEnv(t *testing.T) {
	tests := []struct {
		name              string
		privileged        string
		userNamespaces    string
		persistenceMode   string
		wantMode          string
		wantRequireUserNS string
	}{
		{
			name:              "secure defaults",
			wantMode:          "rootfs",
			wantRequireUserNS: "true",
		},
		{
			name:              "user namespaces opted out",
			userNamespaces:    "false",
			wantMode:          "rootfs",
			wantRequireUserNS: "false",
		},
		{
			name:              "privileged rollback",
			privileged:        "true",
			wantMode:          "rootfs",
			wantRequireUserNS: "false",
		},
		{
			name:              "overlay persistence rollback",
			persistenceMode:   "overlay",
			wantMode:          "overlay",
			wantRequireUserNS: "true",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KYBER_AGENT_PRIVILEGED", tc.privileged)
			t.Setenv("KYBER_AGENT_USER_NAMESPACES", tc.userNamespaces)
			t.Setenv("KYBER_AGENT_PERSISTENCE_MODE", tc.persistenceMode)

			pod, err := BuildPodSpec(testAgent(), testAdapter(), "node-01")
			if err != nil {
				t.Fatalf("BuildPodSpec: %v", err)
			}
			env := map[string]string{}
			for _, e := range pod.Containers[0].Env {
				env[e.Name] = e.Value
			}
			if got := env["KYBER_PERSISTENCE_MODE"]; got != tc.wantMode {
				t.Errorf("KYBER_PERSISTENCE_MODE = %q, want %q", got, tc.wantMode)
			}
			if got := env["KYBER_REQUIRE_USER_NAMESPACE"]; got != tc.wantRequireUserNS {
				t.Errorf("KYBER_REQUIRE_USER_NAMESPACE = %q, want %q", got, tc.wantRequireUserNS)
			}
		})
	}
}

func TestBuildPodSpec_ResourceRequirements(t *testing.T) {
	agent := testAgent()
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	res := pod.Containers[0].Resources
	cpu := resource.MustParse("1")
	mem := resource.MustParse("2Gi")

	if res.Requests.Cpu().Cmp(cpu) != 0 {
		t.Errorf("CPU request: got %v, want %v", res.Requests.Cpu(), &cpu)
	}
	if res.Requests.Memory().Cmp(mem) != 0 {
		t.Errorf("memory request: got %v, want %v", res.Requests.Memory(), &mem)
	}
	if res.Limits.Cpu().Cmp(cpu) != 0 {
		t.Errorf("CPU limit: got %v, want %v", res.Limits.Cpu(), &cpu)
	}
	if res.Limits.Memory().Cmp(mem) != 0 {
		t.Errorf("memory limit: got %v, want %v", res.Limits.Memory(), &mem)
	}
}

func TestBuildPodSpec_Labels(t *testing.T) {
	agent := testAgent()
	adapter := testAdapter()

	// Labels are set on the Pod's ObjectMeta, not the PodSpec.
	// We call BuildPodSpec to verify it succeeds, then check labels via the helper.
	if _, err := BuildPodSpec(agent, adapter, "node-01"); err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	labels := AgentPodLabels(agent, adapter)

	wantAgent := "dave"
	wantRuntime := "stub"
	if labels["kyber.io/agent"] != wantAgent {
		t.Errorf("label kyber.io/agent: got %q, want %q", labels["kyber.io/agent"], wantAgent)
	}
	if labels["kyber.io/runtime"] != wantRuntime {
		t.Errorf("label kyber.io/runtime: got %q, want %q", labels["kyber.io/runtime"], wantRuntime)
	}
	if labels["app.kubernetes.io/part-of"] != "kyber" {
		t.Errorf("label app.kubernetes.io/part-of: got %q, want kyber", labels["app.kubernetes.io/part-of"])
	}
	if labels["app.kubernetes.io/component"] != "agent" {
		t.Errorf("label app.kubernetes.io/component: got %q, want agent", labels["app.kubernetes.io/component"])
	}
}

func TestAgentPodLabels_AuthType(t *testing.T) {
	tests := []struct {
		name     string
		authType kyberv1.AgentAuthType
		want     string
	}{
		{"oauth", kyberv1.AgentAuthTypeOAuth, "oauth"},
		{"api-key", kyberv1.AgentAuthTypeAPIKey, "api-key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := &kyberv1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "kyber-system"},
				Spec: kyberv1.AgentSpec{
					Secrets: kyberv1.AgentSecrets{AuthType: tc.authType},
				},
			}
			adapter := testAdapter()
			labels := AgentPodLabels(agent, adapter)
			got, ok := labels["kyber.io/auth-type"]
			if !ok {
				t.Fatal("kyber.io/auth-type label not found")
			}
			if got != tc.want {
				t.Errorf("kyber.io/auth-type: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildPodSpec_GracePeriod(t *testing.T) {
	agent := testAgent()
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	if pod.TerminationGracePeriodSeconds == nil {
		t.Fatal("terminationGracePeriodSeconds not set")
	}
	if *pod.TerminationGracePeriodSeconds != 30 {
		t.Errorf("terminationGracePeriodSeconds: got %d, want 30", *pod.TerminationGracePeriodSeconds)
	}
}

func TestPVCName(t *testing.T) {
	got := PVCName("dave")
	want := "agent-dave-pv"
	if got != want {
		t.Errorf("PVCName: got %q, want %q", got, want)
	}
}

func TestAgentPodName(t *testing.T) {
	got := AgentPodName("dave")
	want := "agent-dave"
	if got != want {
		t.Errorf("AgentPodName: got %q, want %q", got, want)
	}
}

// TestOffsetsPVCName pins the dedicated transcript-offsets PVC name (kyber#467).
// It must be distinct from the persist PVC (PVCName) so the two volumes never
// collide, and stable so the reconciler's ensure/GC path can target it.
func TestOffsetsPVCName(t *testing.T) {
	got := OffsetsPVCName("dave")
	want := "agent-dave-offsets-pv"
	if got != want {
		t.Errorf("OffsetsPVCName: got %q, want %q", got, want)
	}
	if got == PVCName("dave") {
		t.Errorf("offsets PVC name must differ from the persist PVC name %q", PVCName("dave"))
	}
}

// TestBuildOffsetsPVC_Defaults is the kyber#467 durable-checkpoint AC at the
// PVC level: a small RWO claim, tiny default size (the data is line-count
// integers — sub-1KB even at Lando scale), and a distinguishing label so the
// ops/GC tooling can select it apart from the persist PVC.
func TestBuildOffsetsPVC_Defaults(t *testing.T) {
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			// Disk is the BIG persist disk; the offsets PVC must NOT inherit it.
			Resources: kyberv1.AgentResources{Disk: resource.MustParse("50Gi")},
		},
	}
	pvc := BuildOffsetsPVC(agent, "", "")
	if pvc.Name != "agent-dave-offsets-pv" {
		t.Errorf("name: got %q, want agent-dave-offsets-pv", pvc.Name)
	}
	if pvc.Namespace != "kyber-system" {
		t.Errorf("namespace: got %q, want kyber-system", pvc.Namespace)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("access modes: got %v, want [ReadWriteOnce]", pvc.Spec.AccessModes)
	}
	// Empty size → the tiny 10Mi default, NOT the agent's 50Gi persist disk.
	gotSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	wantSize := resource.MustParse(defaultTranscriptOffsetsSize)
	if gotSize.Cmp(wantSize) != 0 {
		t.Errorf("storage request: got %s, want %s (the tiny offsets default)", gotSize.String(), wantSize.String())
	}
	if pvc.Labels["kyber.io/volume"] != "transcript-offsets" {
		t.Errorf("missing distinguishing label kyber.io/volume=transcript-offsets; got %v", pvc.Labels)
	}
	if pvc.Labels["kyber.io/agent"] != "dave" {
		t.Errorf("missing kyber.io/agent label; got %v", pvc.Labels)
	}
}

// TestBuildOffsetsPVC_EmptyStorageClass_OmitsField is Ackbar's deploy AC: the
// offsets PVC must default to the cluster default StorageClass (local-path on
// ALL targets, including gcp) — never kyber-pd, whose 1Gi PD minimum wastes
// four orders of magnitude on sub-1KB checkpoints.
func TestBuildOffsetsPVC_EmptyStorageClass_OmitsField(t *testing.T) {
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "kyber-system"}}
	pvc := BuildOffsetsPVC(agent, "", "")
	if pvc.Spec.StorageClassName != nil {
		t.Errorf("StorageClassName: got %q, want nil (cluster default = local-path everywhere)", *pvc.Spec.StorageClassName)
	}
}

// TestBuildOffsetsPVC_NamedStorageClass_SetsField allows a per-cluster override
// (the Helm value can still pin one) while the default stays empty.
func TestBuildOffsetsPVC_NamedStorageClass_SetsField(t *testing.T) {
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "kyber-system"}}
	pvc := BuildOffsetsPVC(agent, "local-path", "")
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "local-path" {
		t.Errorf("StorageClassName not set to the provided override")
	}
}

// TestBuildOffsetsPVC_CustomSize honors an explicit size and falls back safely
// on a malformed one (never panics, never inherits the persist disk).
func TestBuildOffsetsPVC_CustomSize(t *testing.T) {
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "kyber-system"}}

	pvc := BuildOffsetsPVC(agent, "", "20Mi")
	got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if got.Cmp(resource.MustParse("20Mi")) != 0 {
		t.Errorf("explicit size: got %s, want 20Mi", got.String())
	}

	// Malformed size must fall back to the default, not panic.
	pvc = BuildOffsetsPVC(agent, "", "not-a-size")
	got = pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if got.Cmp(resource.MustParse(defaultTranscriptOffsetsSize)) != 0 {
		t.Errorf("malformed size: got %s, want the %s default", got.String(), defaultTranscriptOffsetsSize)
	}
}

// TestBuildPodSpec_IdentityRepo_NotConfigured verifies that when spec.identityRepo.repo
// is empty, no kyber-github volume, mount, or env vars are added — agents without an
// identity repo must be indistinguishable from the pre-feature pod shape.
func TestBuildPodSpec_IdentityRepo_NotConfigured(t *testing.T) {
	agent := testAgent() // no identityRepo set
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	for _, v := range pod.Volumes {
		if v.Name == "kyber-github" {
			t.Error("kyber-github volume unexpectedly present when identityRepo not configured")
		}
	}
	for _, vm := range pod.Containers[0].VolumeMounts {
		if vm.Name == "kyber-github" {
			t.Error("kyber-github volumeMount unexpectedly present when identityRepo not configured")
		}
	}
	for _, e := range pod.Containers[0].Env {
		if e.Name == IdentityRepoEnvVar || e.Name == "KYBER_IDENTITY_TOKEN_PATH" {
			t.Errorf("unexpected identity-repo env var %q when identityRepo not configured", e.Name)
		}
	}
}

// TestBuildPodSpec_IdentityRepo_Configured verifies the env wiring when
// spec.identityRepo.repo is set. As of kyber#509 git auth rides the generic PAT
// user-secret, so the pod gets ONLY the KYBER_IDENTITY_REPO slug env — no
// per-agent <name>-github Secret volume, mount, or KYBER_IDENTITY_TOKEN_PATH.
func TestBuildPodSpec_IdentityRepo_Configured(t *testing.T) {
	agent := testAgent()
	agent.Spec.IdentityRepo.Repo = "matty-v/chewie-agent"
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	// No kyber-github Secret volume must be delivered anymore.
	for _, v := range pod.Volumes {
		if v.Name == "kyber-github" {
			t.Error("kyber-github volume must not be present after kyber#509 (git auth is the PAT user-secret)")
		}
	}
	// No kyber-github mount on the container.
	for _, vm := range pod.Containers[0].VolumeMounts {
		if vm.Name == "kyber-github" {
			t.Error("kyber-github volumeMount must not be present after kyber#509")
		}
	}

	// Env: KYBER_IDENTITY_REPO present; KYBER_IDENTITY_TOKEN_PATH gone.
	envMap := make(map[string]string, len(pod.Containers[0].Env))
	for _, e := range pod.Containers[0].Env {
		envMap[e.Name] = e.Value
	}
	if v, ok := envMap[IdentityRepoEnvVar]; !ok || v != "matty-v/chewie-agent" {
		t.Errorf("env %s: got %q (ok=%v), want %q", IdentityRepoEnvVar, v, ok, "matty-v/chewie-agent")
	}
	if v, ok := envMap["KYBER_IDENTITY_TOKEN_PATH"]; ok {
		t.Errorf("env KYBER_IDENTITY_TOKEN_PATH must be absent after kyber#509, got %q", v)
	}
}

func TestBuildPVC_EmptyStorageClass_OmitsField(t *testing.T) {
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Resources: kyberv1.AgentResources{Disk: resource.MustParse("20Gi")},
		},
	}
	pvc := BuildPVC(agent, "")
	if pvc.Spec.StorageClassName != nil {
		t.Errorf("StorageClassName: got %q, want nil (cluster default)", *pvc.Spec.StorageClassName)
	}
}

func TestBuildPVC_NamedStorageClass_SetsField(t *testing.T) {
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Resources: kyberv1.AgentResources{Disk: resource.MustParse("20Gi")},
		},
	}
	pvc := BuildPVC(agent, "kyber-pd")
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "kyber-pd" {
		got := "<nil>"
		if pvc.Spec.StorageClassName != nil {
			got = *pvc.Spec.StorageClassName
		}
		t.Errorf("StorageClassName: got %q, want %q", got, "kyber-pd")
	}
}

// TestBuildPodSpec_UserSecretsKVEnvFrom asserts the kv user-secrets Secret is
// always projected into the container envFrom with the USER_ prefix (#75).
// The projection must be unconditional — the controller eagerly creates the
// Secret empty, so BuildPodSpec never needs to branch on whether it exists.
func TestBuildPodSpec_UserSecretsKVEnvFrom(t *testing.T) {
	agent := testAgent()
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	wantSecretName := UserSecretKVName(agent.Name)
	var found bool
	for _, ef := range pod.Containers[0].EnvFrom {
		if ef.SecretRef == nil || ef.SecretRef.Name != wantSecretName {
			continue
		}
		if ef.Prefix != UserSecretsEnvPrefix {
			t.Errorf("envFrom prefix: got %q, want %q", ef.Prefix, UserSecretsEnvPrefix)
		}
		found = true
		break
	}
	if !found {
		t.Errorf("envFrom secretRef %q not found on container", wantSecretName)
	}
}

// TestBuildPodSpec_UserSecretsFileVolumeMount asserts the file user-secrets
// Secret is always mounted as a volume at /user-secrets (#75).
func TestBuildPodSpec_UserSecretsFileVolumeMount(t *testing.T) {
	agent := testAgent()
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	wantSecretName := UserSecretFilesName(agent.Name)

	var volFound bool
	for _, v := range pod.Volumes {
		if v.Name != "user-secrets-files" {
			continue
		}
		if v.Secret == nil || v.Secret.SecretName != wantSecretName {
			t.Errorf("user-secrets-files volume secretName: got %+v, want %q", v.Secret, wantSecretName)
		}
		volFound = true
		break
	}
	if !volFound {
		t.Error("user-secrets-files volume not found on pod")
	}

	var mountFound bool
	for _, vm := range pod.Containers[0].VolumeMounts {
		if vm.Name != "user-secrets-files" {
			continue
		}
		if vm.MountPath != UserSecretsMountPath {
			t.Errorf("user-secrets-files mountPath: got %q, want %q", vm.MountPath, UserSecretsMountPath)
		}
		if !vm.ReadOnly {
			t.Error("user-secrets-files mount should be read-only")
		}
		mountFound = true
		break
	}
	if !mountFound {
		t.Error("user-secrets-files volumeMount not found on container")
	}
}

// TestBuildPodSpec_AgentJobsConfigMapVolumeMount asserts the <agent>-jobs
// ConfigMap is always mounted as a volume at /kyber/jobs-src (#135). Mounted
// unconditionally — the controller eagerly creates the ConfigMap so the
// reference resolves even when spec.jobs is empty.
func TestBuildPodSpec_AgentJobsConfigMapVolumeMount(t *testing.T) {
	agent := testAgent()
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	wantCMName := JobsConfigMapName(agent.Name)

	var volFound bool
	for _, v := range pod.Volumes {
		if v.Name != "agent-jobs" {
			continue
		}
		if v.ConfigMap == nil || v.ConfigMap.Name != wantCMName {
			t.Errorf("agent-jobs volume ConfigMap ref: got %+v, want name=%q", v.ConfigMap, wantCMName)
		}
		volFound = true
		break
	}
	if !volFound {
		t.Error("agent-jobs volume not found on pod")
	}

	var mountFound bool
	for _, vm := range pod.Containers[0].VolumeMounts {
		if vm.Name != "agent-jobs" {
			continue
		}
		if vm.MountPath != JobsSourceMountPath {
			t.Errorf("agent-jobs mountPath: got %q, want %q", vm.MountPath, JobsSourceMountPath)
		}
		if !vm.ReadOnly {
			t.Error("agent-jobs mount should be read-only")
		}
		mountFound = true
		break
	}
	if !mountFound {
		t.Error("agent-jobs volumeMount not found on container")
	}
}

func TestUserSecretNames(t *testing.T) {
	if got, want := UserSecretKVName("dave"), "dave-user-secrets-kv"; got != want {
		t.Errorf("UserSecretKVName: got %q, want %q", got, want)
	}
	if got, want := UserSecretFilesName("dave"), "dave-user-secrets-files"; got != want {
		t.Errorf("UserSecretFilesName: got %q, want %q", got, want)
	}
}
