package agent_base_test

import (
	"os/exec"
	"testing"
)

func TestRuntimeCapabilityNativeConfig(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required")
	}
	if output, err := exec.Command("python3", "runtime_capabilities_test.py").CombinedOutput(); err != nil {
		t.Fatalf("native capability contract: %v\n%s", err, output)
	}
}
