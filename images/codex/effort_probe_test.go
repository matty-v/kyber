package startcodexshell_test

import (
	"os/exec"
	"testing"
)

func TestNativeEffortProbe(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required")
	}
	if out, err := exec.Command("python3", "check_effort_test.py").CombinedOutput(); err != nil {
		t.Fatalf("native effort probe: %v\n%s", err, out)
	}
}
