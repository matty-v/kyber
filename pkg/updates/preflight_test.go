package updates

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func node(name, kubelet, runtimeVersion string, unschedulable bool) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion:          kubelet,
				ContainerRuntimeVersion: runtimeVersion,
			},
		},
	}
}

func clientWith(t *testing.T, nodes ...*corev1.Node) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	objs := make([]runtime.Object, 0, len(nodes))
	for _, n := range nodes {
		objs = append(objs, n)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...)
}

func TestCheckNodeCapability(t *testing.T) {
	tests := []struct {
		name       string
		nodes      []*corev1.Node
		wantRefuse bool
		wantIn     string
	}{
		{
			name:  "supported cluster passes",
			nodes: []*corev1.Node{node("a", "v1.34.6+k3s1", "containerd://2.2.2-bd1.34", false)},
		},
		{
			// The case this exists for: the canary at the time of kyber#78.
			name:       "kubernetes too old refuses",
			nodes:      []*corev1.Node{node("canary", "v1.31.5+k3s1", "containerd://1.7.23-k3s2", false)},
			wantRefuse: true,
			wantIn:     "Kubernetes 1.31 is below the 1.33",
		},
		{
			name:       "containerd too old refuses even on a new kubernetes",
			nodes:      []*corev1.Node{node("a", "v1.34.0", "containerd://1.7.23", false)},
			wantRefuse: true,
			wantIn:     "containerd 1 is below the 2",
		},
		{
			name: "one bad node in a healthy cluster still refuses",
			nodes: []*corev1.Node{
				node("good", "v1.34.6+k3s1", "containerd://2.2.2", false),
				node("old", "v1.31.5+k3s1", "containerd://1.7.23", false),
			},
			wantRefuse: true,
			wantIn:     `Node "old"`,
		},
		{
			// A cordoned node schedules nothing, agents included, so it cannot
			// be the reason an upgrade is refused.
			name: "cordoned old node is ignored",
			nodes: []*corev1.Node{
				node("good", "v1.34.6+k3s1", "containerd://2.2.2", false),
				node("drained", "v1.28.0", "containerd://1.6.0", true),
			},
		},
		{
			// Another CRI may well support user namespaces. Guessing at its
			// version scheme would block upgrades on no evidence.
			name:  "non-containerd runtime is not judged on its version",
			nodes: []*corev1.Node{node("a", "v1.34.0", "cri-o://1.28.0", false)},
		},
		{
			// This gate exists to prevent a foreseeable outage, not to become
			// one. Anything unparseable passes.
			name:  "unparseable versions never block",
			nodes: []*corev1.Node{node("a", "weird", "also-weird", false)},
		},
		{
			name:  "no nodes at all never blocks",
			nodes: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := clientWith(t, tc.nodes...).Build()
			err := CheckNodeCapability(context.Background(), c)
			if tc.wantRefuse && err == nil {
				t.Fatal("expected the upgrade to be refused, got nil")
			}
			if !tc.wantRefuse && err != nil {
				t.Fatalf("expected no refusal, got: %v", err)
			}
			if tc.wantIn != "" && !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("refusal does not mention %q; got: %v", tc.wantIn, err)
			}
			if tc.wantRefuse {
				// The message has to tell an operator what to do, or it is just
				// a blocked button.
				for _, want := range []string{"requireUserNamespace=false", "Upgrade the node"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal does not offer %q; got: %v", want, err)
					}
				}
			}
		})
	}
}

// An operator who set requireUserNamespace=false has already accepted
// unisolated agents. Refusing their upgrade would be a second opinion on a
// decision they made deliberately.
func TestCheckNodeCapability_RespectsTheOptOut(t *testing.T) {
	c := clientWith(t, node("old", "v1.31.5+k3s1", "containerd://1.7.23", false)).Build()

	if err := CheckNodeCapability(context.Background(), c); err == nil {
		t.Fatal("expected a refusal with the guard on")
	}

	t.Setenv("KYBER_AGENT_REQUIRE_USER_NAMESPACE", "false")
	if err := CheckNodeCapability(context.Background(), c); err != nil {
		t.Errorf("with requireUserNamespace=false the preflight must not block; got: %v", err)
	}
}

func TestParseKubeletVersion(t *testing.T) {
	tests := []struct {
		in    string
		major int
		minor int
		ok    bool
	}{
		{"v1.34.6+k3s1", 1, 34, true},
		{"v1.31.5+k3s1", 1, 31, true},
		{"1.33.0", 1, 33, true},
		{"v1.35.4-rc.1", 1, 35, true},
		{"", 0, 0, false},
		{"garbage", 0, 0, false},
		{"v1", 0, 0, false},
	}
	for _, tc := range tests {
		major, minor, ok := parseKubeletVersion(tc.in)
		if ok != tc.ok || (ok && (major != tc.major || minor != tc.minor)) {
			t.Errorf("parseKubeletVersion(%q) = %d,%d,%v; want %d,%d,%v",
				tc.in, major, minor, ok, tc.major, tc.minor, tc.ok)
		}
	}
}

func TestParseRuntimeVersion(t *testing.T) {
	tests := []struct {
		in    string
		name  string
		major int
		ok    bool
	}{
		{"containerd://2.2.2-bd1.34", "containerd", 2, true},
		{"containerd://1.7.23-k3s2", "containerd", 1, true},
		{"cri-o://1.28.0", "cri-o", 1, true},
		{"docker://24.0.7", "docker", 24, true},
		{"nonsense", "", 0, false},
		{"", "", 0, false},
	}
	for _, tc := range tests {
		name, major, ok := parseRuntimeVersion(tc.in)
		if ok != tc.ok || (ok && (name != tc.name || major != tc.major)) {
			t.Errorf("parseRuntimeVersion(%q) = %q,%d,%v; want %q,%d,%v",
				tc.in, name, major, ok, tc.name, tc.major, tc.ok)
		}
	}
}
