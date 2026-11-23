package hermes_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestNativeActivityPlugin(t *testing.T) {
	// Read source before the subprocess so Go's test cache tracks this dependency.
	source, err := os.ReadFile("activity-plugin/__init__.py")
	if err != nil {
		t.Fatal(err)
	}
	code := string(source) + `
class FixtureRegistry:
 def __init__(self): self.jobs=[]
 def list_sessions(self): return self.jobs
registry=FixtureRegistry()
observer=WorkObserver(registry)
assert observer.snapshot()['complete']
assert observer.begin(session_id='a',turn_id='1') is None
assert observer.begin(session_id='a',turn_id='2') is None
assert observer.snapshot()['foreground']==2
registry.jobs=[{'session_id':'job','status':'running','command':'private-command'}]
assert observer.snapshot()['background']==1
assert 'private-command' not in str(observer.snapshot())
observer.end(session_id='a',turn_id='1',failed=True)
observer.end(session_id='a',turn_id='2',interrupted=True)
assert observer.snapshot()['foreground']==0
assert observer.snapshot()['background']==1
registry.jobs[0]['status']='exited'
assert observer.snapshot()['background']==0
registry.jobs[0].update(status='running',detached=True)
assert not observer.snapshot()['complete']
`
	cmd := exec.Command("python3", "-B", "-c", code)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native plugin: %v\n%s", err, out)
	}
}

func TestNativeActivityPluginInstallDoesNotFollowLinks(t *testing.T) {
	source, err := os.ReadFile("configure-hermes.py")
	if err != nil {
		t.Fatal(err)
	}
	code := `import sys, types, tempfile
from pathlib import Path
sys.modules['yaml']=types.ModuleType('yaml')
namespace={'__name__':'fixture'}
exec(sys.stdin.read(),namespace)
with tempfile.TemporaryDirectory() as directory:
 root=Path(directory)
 source=root/'source'; source.mkdir()
 (source/'__init__.py').write_text('platform observer')
 (source/'plugin.yaml').write_text('name: kyber-platform-activity')
 home=root/'home'; home.mkdir()
 destination=home/'plugins'/'kyber-platform-activity'
 destination.mkdir(parents=True)
 sentinel=root/'identity-sentinel';sentinel.write_text('keep identity')
 (destination/'__init__.py').symlink_to(sentinel)
 assert namespace['install_activity_plugin'](home,source)
 assert sentinel.read_text()=='keep identity'
 assert (destination/'__init__.py').read_text()=='platform observer'
 other=root/'other';other.mkdir()
 (other/'plugins').symlink_to(destination,target_is_directory=True)
 assert not namespace['install_activity_plugin'](other,source)
`
	cmd := exec.Command("python3", "-B", "-c", code)
	cmd.Stdin = strings.NewReader(string(source))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native plugin install: %v\n%s", err, out)
	}
}
