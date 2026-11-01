package startcodexshell_test

import (
	"os"
	"os/exec"
	"testing"
)

func TestNativeEffortProbe(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required")
	}
	// Go cannot observe files opened by the Python child. Register these
	// inputs so a changed native probe cannot reuse an earlier test result.
	for _, path := range []string{"check-effort.py", "check_effort_test.py"} {
		if _, err := os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("python3", "-B", "check_effort_test.py").CombinedOutput(); err != nil {
		t.Fatalf("native effort probe: %v\n%s", err, out)
	}
}
