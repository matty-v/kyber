package machine

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func mustQ(s string) resource.Quantity {
	return resource.MustParse(s)
}

// metav1FromName is a tiny helper to keep the table compact.
func metav1FromName(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name}
}

func TestComputeAssignable(t *testing.T) {
	tests := []struct {
		name        string
		observed    kyberv1.MachineCapacity
		reservation kyberv1.MachineCapacity
		wantCPU     string
		wantMem     string
		wantDisk    string
	}{
		{
			name:        "razer-shape: 20 CPU / 12Gi / 200Gi observed minus 1/1Gi/10Gi reservation",
			observed:    kyberv1.MachineCapacity{CPU: mustQ("20"), Memory: mustQ("12Gi"), EphemeralStorage: mustQ("200Gi")},
			reservation: kyberv1.MachineCapacity{CPU: mustQ("1"), Memory: mustQ("1Gi"), EphemeralStorage: mustQ("10Gi")},
			wantCPU:     "19",
			wantMem:     "11Gi",
			wantDisk:    "190Gi",
		},
		{
			name:        "zero reservation: assignable equals observed",
			observed:    kyberv1.MachineCapacity{CPU: mustQ("4"), Memory: mustQ("8Gi"), EphemeralStorage: mustQ("100Gi")},
			reservation: kyberv1.MachineCapacity{},
			wantCPU:     "4",
			wantMem:     "8Gi",
			wantDisk:    "100Gi",
		},
		{
			name:        "reservation exceeds observed: clamped to zero",
			observed:    kyberv1.MachineCapacity{CPU: mustQ("500m"), Memory: mustQ("256Mi"), EphemeralStorage: mustQ("5Gi")},
			reservation: kyberv1.MachineCapacity{CPU: mustQ("1"), Memory: mustQ("1Gi"), EphemeralStorage: mustQ("10Gi")},
			wantCPU:     "0",
			wantMem:     "0",
			wantDisk:    "0",
		},
		{
			name:        "pre-PR-C node (no ephemeral-storage): disk stays zero",
			observed:    kyberv1.MachineCapacity{CPU: mustQ("4"), Memory: mustQ("8Gi")},
			reservation: kyberv1.MachineCapacity{CPU: mustQ("1"), Memory: mustQ("1Gi"), EphemeralStorage: mustQ("10Gi")},
			wantCPU:     "3",
			wantMem:     "7Gi",
			wantDisk:    "0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeAssignable(tc.observed, tc.reservation)
			if got.CPU.Cmp(mustQ(tc.wantCPU)) != 0 {
				t.Errorf("CPU: got %q, want %q", got.CPU.String(), tc.wantCPU)
			}
			if got.Memory.Cmp(mustQ(tc.wantMem)) != 0 {
				t.Errorf("Memory: got %q, want %q", got.Memory.String(), tc.wantMem)
			}
			if got.EphemeralStorage.Cmp(mustQ(tc.wantDisk)) != 0 {
				t.Errorf("EphemeralStorage: got %q, want %q", got.EphemeralStorage.String(), tc.wantDisk)
			}
		})
	}
}

func TestComputeAvailable(t *testing.T) {
	mkAgent := func(name, machine, cpu, mem, disk string) kyberv1.Agent {
		return kyberv1.Agent{
			ObjectMeta: metav1FromName(name),
			Spec: kyberv1.AgentSpec{
				Machine: machine,
				Resources: kyberv1.AgentResources{
					CPU:    mustQ(cpu),
					Memory: mustQ(mem),
					Disk:   mustQ(disk),
				},
			},
		}
	}

	assignable := kyberv1.MachineCapacity{CPU: mustQ("19"), Memory: mustQ("11Gi"), EphemeralStorage: mustQ("190Gi")}

	tests := []struct {
		name        string
		machineName string
		agents      []kyberv1.Agent
		wantCPU     string
		wantMem     string
		wantDisk    string
	}{
		{
			name:        "no agents: available equals assignable",
			machineName: "razer",
			agents:      nil,
			wantCPU:     "19",
			wantMem:     "11Gi",
			wantDisk:    "190Gi",
		},
		{
			name:        "two agents on this machine: subtract all three resources",
			machineName: "razer",
			agents: []kyberv1.Agent{
				mkAgent("alice", "razer", "2", "4Gi", "50Gi"),
				mkAgent("bob", "razer", "1", "2Gi", "20Gi"),
			},
			wantCPU:  "16",
			wantMem:  "5Gi",
			wantDisk: "120Gi",
		},
		{
			name:        "agents on other machines ignored (incl. disk)",
			machineName: "razer",
			agents: []kyberv1.Agent{
				mkAgent("alice", "razer", "2", "4Gi", "50Gi"),
				mkAgent("bob", "other-machine", "100", "100Gi", "1Ti"),
			},
			wantCPU:  "17",
			wantMem:  "7Gi",
			wantDisk: "140Gi",
		},
		{
			name:        "over-allocation clamps to zero across all resources",
			machineName: "razer",
			agents: []kyberv1.Agent{
				mkAgent("alice", "razer", "30", "20Gi", "300Gi"),
			},
			wantCPU:  "0",
			wantMem:  "0",
			wantDisk: "0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeAvailable(assignable, tc.agents, tc.machineName)
			if got.CPU.Cmp(mustQ(tc.wantCPU)) != 0 {
				t.Errorf("CPU: got %q, want %q", got.CPU.String(), tc.wantCPU)
			}
			if got.Memory.Cmp(mustQ(tc.wantMem)) != 0 {
				t.Errorf("Memory: got %q, want %q", got.Memory.String(), tc.wantMem)
			}
			if got.EphemeralStorage.Cmp(mustQ(tc.wantDisk)) != 0 {
				t.Errorf("EphemeralStorage: got %q, want %q", got.EphemeralStorage.String(), tc.wantDisk)
			}
		})
	}
}
