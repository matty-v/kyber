package agent

import (
	"strings"
	"testing"
)

func TestSetupWithManagerRequiresMachineGetter(t *testing.T) {
	r := &AgentReconciler{}
	err := r.SetupWithManager(nil)
	if err == nil || !strings.Contains(err.Error(), "machine getter is required") {
		t.Fatalf("SetupWithManager error = %v, want required MachineGetter failure", err)
	}
}
