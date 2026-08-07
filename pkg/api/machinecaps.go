package api

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// defaultVMTypeLiterals is the built-in GCE VM type catalog used when
// KYBER_GCE_VM_TYPE_CATALOG is not set. Operators override the catalog via
// compute.gce.vmTypeCatalog in the Helm chart — no code change required.
//
// Kept in sync with deploy/helm/kyber/values.yaml's vmTypeCatalog so the
// dev/test fallback matches what prod renders. Touching one without the
// other is a footgun (CI tests use this default but prod uses the chart).
var defaultVMTypeLiterals = []vmTypeLiteral{
	// Shared-core / micros (legacy and current generation)
	{Type: "f1-micro", CPU: "1", Memory: "600Mi"},
	{Type: "g1-small", CPU: "1", Memory: "1700Mi"},
	{Type: "e2-micro", CPU: "2", Memory: "1Gi"},
	{Type: "e2-small", CPU: "2", Memory: "2Gi"},
	{Type: "e2-medium", CPU: "2", Memory: "4Gi"},
	// E2 standard (cost-optimized current generation)
	{Type: "e2-standard-2", CPU: "2", Memory: "8Gi"},
	{Type: "e2-standard-4", CPU: "4", Memory: "16Gi"},
	{Type: "e2-standard-8", CPU: "8", Memory: "32Gi"},
	{Type: "e2-standard-16", CPU: "16", Memory: "64Gi"},
	// N1 standard (older Intel, cheap, broad availability)
	{Type: "n1-standard-1", CPU: "1", Memory: "3750Mi"},
	{Type: "n1-standard-2", CPU: "2", Memory: "7500Mi"},
	{Type: "n1-standard-4", CPU: "4", Memory: "15Gi"},
	{Type: "n1-standard-8", CPU: "8", Memory: "30Gi"},
	// N2 standard (newer Intel, better price/perf)
	{Type: "n2-standard-2", CPU: "2", Memory: "8Gi"},
	{Type: "n2-standard-4", CPU: "4", Memory: "16Gi"},
	{Type: "n2-standard-8", CPU: "8", Memory: "32Gi"},
	{Type: "n2-standard-16", CPU: "16", Memory: "64Gi"},
}

type vmTypeLiteral struct {
	Type   string `json:"type"`
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// DefaultGCEVMTypeCatalog returns a fresh copy of the built-in GCE VM type catalog.
// Used as the fallback when KYBER_GCE_VM_TYPE_CATALOG is not set.
// Returns a new map each call so callers cannot corrupt each other's state.
func DefaultGCEVMTypeCatalog() map[string]kyberv1.MachineCapacity {
	return parseLiterals(defaultVMTypeLiterals)
}

// ParseVMTypeCatalog parses a JSON array of VM type entries into a capacity map.
// Expected format: [{"type":"e2-small","cpu":"2","memory":"2Gi"}, ...]
func ParseVMTypeCatalog(jsonStr string) (map[string]kyberv1.MachineCapacity, error) {
	var entries []vmTypeLiteral
	if err := json.Unmarshal([]byte(jsonStr), &entries); err != nil {
		return nil, fmt.Errorf("parsing vmTypeCatalog: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("vmTypeCatalog is empty")
	}
	out := make(map[string]kyberv1.MachineCapacity, len(entries))
	for _, e := range entries {
		if e.Type == "" {
			return nil, fmt.Errorf("vmTypeCatalog entry has empty type")
		}
		if _, dup := out[e.Type]; dup {
			return nil, fmt.Errorf("vmTypeCatalog has duplicate type %q", e.Type)
		}
		cpu, err := resource.ParseQuantity(e.CPU)
		if err != nil {
			return nil, fmt.Errorf("vmTypeCatalog entry %q cpu: %w", e.Type, err)
		}
		mem, err := resource.ParseQuantity(e.Memory)
		if err != nil {
			return nil, fmt.Errorf("vmTypeCatalog entry %q memory: %w", e.Type, err)
		}
		out[e.Type] = kyberv1.MachineCapacity{CPU: cpu, Memory: mem}
	}
	return out, nil
}

func parseLiterals(lits []vmTypeLiteral) map[string]kyberv1.MachineCapacity {
	out := make(map[string]kyberv1.MachineCapacity, len(lits))
	for _, l := range lits {
		out[l.Type] = kyberv1.MachineCapacity{
			CPU:    resource.MustParse(l.CPU),
			Memory: resource.MustParse(l.Memory),
		}
	}
	return out
}
