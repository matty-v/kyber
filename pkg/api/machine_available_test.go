package api

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func mustQ(s string) resource.Quantity { return resource.MustParse(s) }

func TestMachineAvailable_CapacityLowerThanNode(t *testing.T) {
	m := &kyberv1.Machine{
		Spec: kyberv1.MachineSpec{
			Capacity: kyberv1.MachineCapacity{CPU: mustQ("4"), Memory: mustQ("16Gi")},
		},
		Status: kyberv1.MachineStatus{
			ObservedCapacity: &kyberv1.MachineCapacity{
				CPU: mustQ("20"), Memory: mustQ("100Gi"),
			},
		},
	}
	got := machineAvailable(m, nil)
	if got.CPU.Cmp(mustQ("4")) != 0 || got.Memory.Cmp(mustQ("16Gi")) != 0 {
		t.Errorf("with capacity < node: got {%s, %s}, want {4, 16Gi}",
			got.CPU.String(), got.Memory.String())
	}
}

func TestMachineAvailable_NodeLowerThanCapacity(t *testing.T) {
	m := &kyberv1.Machine{
		Spec: kyberv1.MachineSpec{
			Capacity: kyberv1.MachineCapacity{CPU: mustQ("16"), Memory: mustQ("64Gi")},
		},
		Status: kyberv1.MachineStatus{
			ObservedCapacity: &kyberv1.MachineCapacity{
				CPU: mustQ("4"), Memory: mustQ("8Gi"),
			},
		},
	}
	got := machineAvailable(m, nil)
	if got.CPU.Cmp(mustQ("4")) != 0 || got.Memory.Cmp(mustQ("8Gi")) != 0 {
		t.Errorf("with node < capacity: got {%s, %s}, want {4, 8Gi}",
			got.CPU.String(), got.Memory.String())
	}
}

func TestMachineAvailable_SubtractsAgentRequests(t *testing.T) {
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "local"},
		Spec: kyberv1.MachineSpec{
			Capacity: kyberv1.MachineCapacity{CPU: mustQ("4"), Memory: mustQ("16Gi")},
		},
	}
	agents := []kyberv1.Agent{
		{Spec: kyberv1.AgentSpec{
			Machine:   "local",
			Resources: kyberv1.AgentResources{CPU: mustQ("1"), Memory: mustQ("2Gi")},
		}},
		{Spec: kyberv1.AgentSpec{
			Machine:   "local",
			Resources: kyberv1.AgentResources{CPU: mustQ("500m"), Memory: mustQ("4Gi")},
		}},
		{Spec: kyberv1.AgentSpec{
			Machine:   "other",
			Resources: kyberv1.AgentResources{CPU: mustQ("8"), Memory: mustQ("32Gi")},
		}},
	}
	got := machineAvailable(m, agents)
	// 4 - 1 - 0.5 = 2.5 (written as 2500m by resource.Quantity canonical form).
	if got.CPU.Cmp(mustQ("2500m")) != 0 {
		t.Errorf("cpu = %s, want 2500m", got.CPU.String())
	}
	if got.Memory.Cmp(mustQ("10Gi")) != 0 {
		t.Errorf("memory = %s, want 10Gi", got.Memory.String())
	}
}

func TestMachineAvailable_NoAllocatable_UsesCapacityOnly(t *testing.T) {
	m := &kyberv1.Machine{
		Spec: kyberv1.MachineSpec{
			Capacity: kyberv1.MachineCapacity{CPU: mustQ("4"), Memory: mustQ("16Gi")},
		},
	}
	got := machineAvailable(m, nil)
	if got.CPU.Cmp(mustQ("4")) != 0 {
		t.Errorf("cpu = %s, want 4", got.CPU.String())
	}
	if got.Memory.Cmp(mustQ("16Gi")) != 0 {
		t.Errorf("memory = %s, want 16Gi", got.Memory.String())
	}
}

func TestMachineAvailable_OverSubscribed_FloorsAtZero(t *testing.T) {
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "local"},
		Spec: kyberv1.MachineSpec{
			Capacity: kyberv1.MachineCapacity{CPU: mustQ("2"), Memory: mustQ("4Gi")},
		},
	}
	agents := []kyberv1.Agent{
		{Spec: kyberv1.AgentSpec{
			Machine:   "local",
			Resources: kyberv1.AgentResources{CPU: mustQ("3"), Memory: mustQ("8Gi")},
		}},
	}
	got := machineAvailable(m, agents)
	if got.CPU.Sign() != 0 || got.Memory.Sign() != 0 {
		t.Errorf("expected both floored to zero; got {%s, %s}",
			got.CPU.String(), got.Memory.String())
	}
}

func TestMachineAvailableExcluding_AddsBackExcludedAgent(t *testing.T) {
	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderMock,
			Capacity: kyberv1.MachineCapacity{
				CPU:    mustQ("4"),
				Memory: mustQ("8Gi"),
			},
		},
	}
	agents := []kyberv1.Agent{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"},
			Spec: kyberv1.AgentSpec{
				Machine: "local",
				Resources: kyberv1.AgentResources{
					CPU:    mustQ("1"),
					Memory: mustQ("2Gi"),
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "kyber-system"},
			Spec: kyberv1.AgentSpec{
				Machine: "local",
				Resources: kyberv1.AgentResources{
					CPU:    mustQ("1"),
					Memory: mustQ("2Gi"),
				},
			},
		},
	}

	// Excluding alice: available = 4 - 1 = 3 cpu, 8Gi - 2Gi = 6Gi memory.
	avail := MachineAvailableExcluding(machine, agents, "alice")
	if avail.CPU.Cmp(mustQ("3")) != 0 {
		t.Errorf("CPU = %s, want 3", avail.CPU.String())
	}
	if avail.Memory.Cmp(mustQ("6Gi")) != 0 {
		t.Errorf("Memory = %s, want 6Gi", avail.Memory.String())
	}
}

func TestMachineAvailableExcluding_UnknownExcludeActsLikeAvailable(t *testing.T) {
	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderMock,
			Capacity: kyberv1.MachineCapacity{
				CPU:    mustQ("4"),
				Memory: mustQ("8Gi"),
			},
		},
	}
	agents := []kyberv1.Agent{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"},
			Spec: kyberv1.AgentSpec{
				Machine: "local",
				Resources: kyberv1.AgentResources{
					CPU:    mustQ("1"),
					Memory: mustQ("2Gi"),
				},
			},
		},
	}
	excluded := MachineAvailableExcluding(machine, agents, "does-not-exist")
	plain := machineAvailable(machine, agents)
	if excluded.CPU.Cmp(plain.CPU) != 0 || excluded.Memory.Cmp(plain.Memory) != 0 {
		t.Errorf("unknown exclude should equal machineAvailable; got %+v vs %+v", excluded, plain)
	}
}

// Test_machineAvailable_PrefersStatusAvailableCapacity verifies that when the
// machine is in a live phase AND the controller has populated
// Status.AvailableCapacity, the helper reads it directly and ignores
// Spec.Capacity / agent list. Post-#140 happy path.
func Test_machineAvailable_PrefersStatusAvailableCapacity(t *testing.T) {
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "razer"},
		Spec: kyberv1.MachineSpec{
			Capacity: kyberv1.MachineCapacity{
				CPU:    resource.MustParse("100"),
				Memory: resource.MustParse("100Gi"),
			},
		},
		Status: kyberv1.MachineStatus{
			Phase: kyberv1.MachinePhaseRunning,
			AvailableCapacity: &kyberv1.MachineCapacity{
				CPU:    resource.MustParse("2"),
				Memory: resource.MustParse("4Gi"),
			},
		},
	}
	agents := []kyberv1.Agent{{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       kyberv1.AgentSpec{Machine: "razer", Resources: kyberv1.AgentResources{CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi")}},
	}}

	got := machineAvailable(m, agents)
	if got.CPU.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("CPU: got %q, want 2 (status-direct, agents must be ignored)", got.CPU.String())
	}
	if got.Memory.Cmp(resource.MustParse("4Gi")) != 0 {
		t.Errorf("Memory: got %q, want 4Gi", got.Memory.String())
	}
}

// Test_machineAvailable_LegacyFallback_NilAvailable verifies the on-the-fly
// computation path when Status.AvailableCapacity is nil (in-flight upgrade
// window or pre-first-reconcile machine).
func Test_machineAvailable_LegacyFallback_NilAvailable(t *testing.T) {
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "razer"},
		Spec: kyberv1.MachineSpec{
			Capacity: kyberv1.MachineCapacity{
				CPU:    resource.MustParse("4"),
				Memory: resource.MustParse("8Gi"),
			},
		},
		Status: kyberv1.MachineStatus{
			Phase: kyberv1.MachinePhaseRunning,
			ObservedCapacity: &kyberv1.MachineCapacity{
				CPU:    resource.MustParse("4"),
				Memory: resource.MustParse("8Gi"),
			},
		},
	}
	agents := []kyberv1.Agent{{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       kyberv1.AgentSpec{Machine: "razer", Resources: kyberv1.AgentResources{CPU: resource.MustParse("1"), Memory: resource.MustParse("2Gi")}},
	}}

	got := machineAvailable(m, agents)
	if got.CPU.Cmp(resource.MustParse("3")) != 0 {
		t.Errorf("CPU: got %q, want 3", got.CPU.String())
	}
	if got.Memory.Cmp(resource.MustParse("6Gi")) != 0 {
		t.Errorf("Memory: got %q, want 6Gi", got.Memory.String())
	}
}

// Test_machineAvailable_PhaseGate_FailedDoesNotTrustAvailableCapacity is the
// regression for #140's stale-capacity-on-Failed concern. When a node
// disappears, ActionMarkFailed transitions the machine to Failed without
// clearing Status.AvailableCapacity. The API consumer must NOT trust that
// stale field. Instead, it falls back to the on-the-fly Spec.Capacity − agents
// computation, which still returns a coherent budget based on operator intent.
func Test_machineAvailable_PhaseGate_FailedDoesNotTrustAvailableCapacity(t *testing.T) {
	// Machine is Failed (node gone). Spec.Capacity says 4/8Gi (operator intent).
	// Status.AvailableCapacity says 3.5/7Gi (stale — when the node existed).
	// Two agents (alice 1/2Gi, bob 0.5/1Gi) are still bound.
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "razer"},
		Spec: kyberv1.MachineSpec{
			Capacity: kyberv1.MachineCapacity{
				CPU:    resource.MustParse("4"),
				Memory: resource.MustParse("8Gi"),
			},
		},
		Status: kyberv1.MachineStatus{
			Phase: kyberv1.MachinePhaseFailed,
			AvailableCapacity: &kyberv1.MachineCapacity{
				CPU:    resource.MustParse("3500m"),
				Memory: resource.MustParse("7Gi"),
			},
		},
	}
	agents := []kyberv1.Agent{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "alice"},
			Spec:       kyberv1.AgentSpec{Machine: "razer", Resources: kyberv1.AgentResources{CPU: resource.MustParse("1"), Memory: resource.MustParse("2Gi")}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "bob"},
			Spec:       kyberv1.AgentSpec{Machine: "razer", Resources: kyberv1.AgentResources{CPU: resource.MustParse("500m"), Memory: resource.MustParse("1Gi")}},
		},
	}

	got := machineAvailable(m, agents)
	// Want: 4 - 1 - 0.5 = 2.5 CPU; 8Gi - 2Gi - 1Gi = 5Gi.
	// NOT 3500m / 7Gi — that would mean we trusted the stale field.
	if got.CPU.Cmp(resource.MustParse("2500m")) != 0 {
		t.Errorf("CPU: got %q, want 2500m (Phase=Failed must trigger fallback)", got.CPU.String())
	}
	if got.Memory.Cmp(resource.MustParse("5Gi")) != 0 {
		t.Errorf("Memory: got %q, want 5Gi", got.Memory.String())
	}
}

// Test_MachineAvailableExcluding_LiveMachine_AddsBackExcludedAgent is the
// regression for the resize-self bug Chewie caught in code review.
//
// Status.AvailableCapacity (controller-published) has ALL bound agents'
// allocations subtracted. For an in-place resize check, the requesting
// agent's current allocation must be treated as returnable headroom — so
// MachineAvailableExcluding must add it back to the status field.
//
// Without this, the /set-resources 409 check would falsely reject any
// resize request from an agent whose current allocation is non-zero.
func Test_MachineAvailableExcluding_LiveMachine_AddsBackExcludedAgent(t *testing.T) {
	// 4 CPU / 8Gi assignable, alice uses 2 CPU / 2Gi → AvailableCapacity = 2 CPU / 6Gi.
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "razer"},
		Spec: kyberv1.MachineSpec{
			Capacity: kyberv1.MachineCapacity{
				CPU:    mustQ("4"),
				Memory: mustQ("8Gi"),
			},
		},
		Status: kyberv1.MachineStatus{
			Phase: kyberv1.MachinePhaseRunning,
			AvailableCapacity: &kyberv1.MachineCapacity{
				CPU:    mustQ("2"),
				Memory: mustQ("6Gi"),
			},
		},
	}
	agents := []kyberv1.Agent{{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec: kyberv1.AgentSpec{
			Machine:   "razer",
			Resources: kyberv1.AgentResources{CPU: mustQ("2"), Memory: mustQ("2Gi")},
		},
	}}

	got := MachineAvailableExcluding(m, agents, "alice")
	// Want: 2 (status) + 2 (alice's allocation back) = 4 CPU.
	// Want: 6Gi (status) + 2Gi (alice's allocation back) = 8Gi.
	if got.CPU.Cmp(mustQ("4")) != 0 {
		t.Errorf("CPU: got %q, want 4 (alice's allocation must be added back for resize-self)", got.CPU.String())
	}
	if got.Memory.Cmp(mustQ("8Gi")) != 0 {
		t.Errorf("Memory: got %q, want 8Gi", got.Memory.String())
	}
}

// Test_MachineAvailableExcluding_LiveMachine_AgentOnOtherMachine_NotAdded
// guards against incorrectly adding back an agent's allocation when the
// agent is bound to a DIFFERENT machine but happens to share a name.
func Test_MachineAvailableExcluding_LiveMachine_AgentOnOtherMachine_NotAdded(t *testing.T) {
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "razer"},
		Spec: kyberv1.MachineSpec{
			Capacity: kyberv1.MachineCapacity{CPU: mustQ("4"), Memory: mustQ("8Gi")},
		},
		Status: kyberv1.MachineStatus{
			Phase: kyberv1.MachinePhaseRunning,
			AvailableCapacity: &kyberv1.MachineCapacity{
				CPU:    mustQ("3"),
				Memory: mustQ("7Gi"),
			},
		},
	}
	// "alice" is on a different machine — must not be treated as freeable headroom.
	agents := []kyberv1.Agent{{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec: kyberv1.AgentSpec{
			Machine:   "other-machine",
			Resources: kyberv1.AgentResources{CPU: mustQ("100"), Memory: mustQ("100Gi")},
		},
	}}

	got := MachineAvailableExcluding(m, agents, "alice")
	// alice is not on razer; status field is returned unchanged.
	if got.CPU.Cmp(mustQ("3")) != 0 {
		t.Errorf("CPU: got %q, want 3 (cross-machine alice must not add back)", got.CPU.String())
	}
	if got.Memory.Cmp(mustQ("7Gi")) != 0 {
		t.Errorf("Memory: got %q, want 7Gi", got.Memory.String())
	}
}

// Test_MachineAvailableExcluding_LiveMachine_AgentNotFound_StatusUnchanged
// guards the case where excludeName doesn't match any agent.
func Test_MachineAvailableExcluding_LiveMachine_AgentNotFound_StatusUnchanged(t *testing.T) {
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "razer"},
		Spec: kyberv1.MachineSpec{
			Capacity: kyberv1.MachineCapacity{CPU: mustQ("4"), Memory: mustQ("8Gi")},
		},
		Status: kyberv1.MachineStatus{
			Phase: kyberv1.MachinePhaseRunning,
			AvailableCapacity: &kyberv1.MachineCapacity{
				CPU:    mustQ("3"),
				Memory: mustQ("7Gi"),
			},
		},
	}
	agents := []kyberv1.Agent{} // empty — name "ghost" doesn't exist

	got := MachineAvailableExcluding(m, agents, "ghost")
	if got.CPU.Cmp(mustQ("3")) != 0 {
		t.Errorf("CPU: got %q, want 3 (no agent to add back)", got.CPU.String())
	}
	if got.Memory.Cmp(mustQ("7Gi")) != 0 {
		t.Errorf("Memory: got %q, want 7Gi", got.Memory.String())
	}
}

// Test_isLiveMachinePhase covers the helper's truth table.
func Test_isLiveMachinePhase(t *testing.T) {
	live := []kyberv1.MachinePhase{
		kyberv1.MachinePhaseProvisioning,
		kyberv1.MachinePhaseReady,
		kyberv1.MachinePhaseRunning,
	}
	dead := []kyberv1.MachinePhase{
		kyberv1.MachinePhaseFailed,
		kyberv1.MachinePhaseStopping,
		kyberv1.MachinePhaseStopped,
		kyberv1.MachinePhasePreempted,
		kyberv1.MachinePhaseReplacing,
		kyberv1.MachinePhaseDeleted,
		// Empty (pre-first-reconcile) — no Phase yet, must NOT short-circuit
		// on a possibly-uninitialized Status.AvailableCapacity.
		kyberv1.MachinePhase(""),
	}
	for _, p := range live {
		if !isLiveMachinePhase(p) {
			t.Errorf("isLiveMachinePhase(%q) = false, want true", p)
		}
	}
	for _, p := range dead {
		if isLiveMachinePhase(p) {
			t.Errorf("isLiveMachinePhase(%q) = true, want false (stale capacity must trigger fallback)", p)
		}
	}
}
