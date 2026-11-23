package startclaudeshell_test

import (
	"os"
	"os/exec"
	"testing"
)

func TestNativeActivityAdapter(t *testing.T) {
	for _, path := range []string{"activity/helper.py", "activity/helper_test.py", "activity/mod_test.mjs", "activity/mod/hooks/register.js", "activity/mod/hooks/hooks.json", "activity/mod/.claude-plugin/plugin.json"} {
		if _, err := os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range [][]string{{"python3", "-B", "activity/helper_test.py"}, {"node", "--test", "activity/mod_test.mjs"}} {
		if _, err := exec.LookPath(command[0]); err != nil {
			t.Fatalf("native activity qualification requires %s", command[0])
		}
		if out, err := exec.Command(command[0], command[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("native activity adapter: %v\n%s", err, out)
		}
	}
}
