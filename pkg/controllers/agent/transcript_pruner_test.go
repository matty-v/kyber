package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// --- Injected container spec -------------------------------------------------

func TestAppendTranscriptPruner_NoOpWhenDisabledOrUnconfigured(t *testing.T) {
	cases := []struct {
		name string
		cfg  TranscriptPrunerConfig
	}{
		{"disabled", TranscriptPrunerConfig{RuntimeImage: "img:v1", Enabled: false, MaxAgeDays: 7}},
		{"empty image", TranscriptPrunerConfig{RuntimeImage: "", Enabled: true, MaxAgeDays: 7}},
		{"non-positive age fails closed", TranscriptPrunerConfig{RuntimeImage: "img:v1", Enabled: true, MaxAgeDays: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
			AppendTranscriptPruner(spec, tc.cfg)
			// kyber#575: the pruner is a native sidecar in InitContainers when it
			// IS injected, so a no-op must leave BOTH lists at their input shape.
			if len(spec.Containers) != 1 || len(spec.InitContainers) != 0 {
				t.Errorf("expected no sidecar injected; got %d containers, %d initContainers",
					len(spec.Containers), len(spec.InitContainers))
			}
		})
	}
}

func TestAppendTranscriptPruner_InjectsLockedDownRWSidecar(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptPruner(spec, TranscriptPrunerConfig{
		AgentName:            "alice",
		RuntimeImage:         "img:v1",
		Enabled:              true,
		MaxAgeDays:           7,
		PruneIntervalMinutes: 60,
	})
	// kyber#575: the pruner is a native sidecar in InitContainers; the regular
	// Containers slice is unchanged (still just the agent).
	if len(spec.Containers) != 1 {
		t.Fatalf("regular containers must be unchanged (agent only); got %d", len(spec.Containers))
	}
	c := mustInitContainerByName(t, spec, TranscriptPrunerContainerName)
	if c.Name != TranscriptPrunerContainerName {
		t.Errorf("container name = %q, want %q", c.Name, TranscriptPrunerContainerName)
	}
	if c.Image != "img:v1" {
		t.Errorf("image = %q, want the agent runtime image img:v1", c.Image)
	}

	// The pruner's persist mount MUST be read-write (this is the whole point —
	// it deletes files). The tailer's, by contrast, must stay read-only.
	var pm *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == "persist" {
			pm = &c.VolumeMounts[i]
		}
	}
	if pm == nil {
		t.Fatal("pruner has no persist mount")
	}
	if pm.ReadOnly {
		t.Error("pruner persist mount is ReadOnly — it must be writable to prune")
	}
	if pm.MountPath != transcriptMountPath {
		t.Errorf("pruner persist mountPath = %q, want %q", pm.MountPath, transcriptMountPath)
	}

	// Locked-down posture, identical to the tailer.
	sc := c.SecurityContext
	if sc == nil {
		t.Fatal("pruner has no security context")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 0 {
		t.Errorf("RunAsUser = %v, want 0 (root, to delete root-owned JSONL)", sc.RunAsUser)
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem must be true")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be false")
	}
	if sc.Privileged != nil && *sc.Privileged {
		t.Error("pruner must not be privileged")
	}

	// Policy is threaded through as env the script reads.
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["PRUNE_MAX_AGE_DAYS"] != "7" {
		t.Errorf("PRUNE_MAX_AGE_DAYS = %q, want 7", env["PRUNE_MAX_AGE_DAYS"])
	}
	if env["PRUNE_INTERVAL_SECONDS"] != "3600" {
		t.Errorf("PRUNE_INTERVAL_SECONDS = %q, want 3600 (60m)", env["PRUNE_INTERVAL_SECONDS"])
	}
	if env["PRUNE_OVERLAY_ROOT"] != transcriptProjectsOverlayRoot {
		t.Errorf("PRUNE_OVERLAY_ROOT = %q, want %q", env["PRUNE_OVERLAY_ROOT"], transcriptProjectsOverlayRoot)
	}
	if env["PRUNE_BIND_ROOT"] != transcriptProjectsBindRoot {
		t.Errorf("PRUNE_BIND_ROOT = %q, want %q", env["PRUNE_BIND_ROOT"], transcriptProjectsBindRoot)
	}
}

// TestAppendTranscriptPruner_LeavesTailerReadOnly is the kyber#446 invariant: the
// tailer's read-only PVC mount must be unaffected by adding the RW pruner.
func TestAppendTranscriptPruner_LeavesTailerReadOnly(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "img:v1"})
	AppendTranscriptPruner(spec, TranscriptPrunerConfig{AgentName: "alice", RuntimeImage: "img:v1", Enabled: true, MaxAgeDays: 7})

	// kyber#575: both sidecars are now native sidecars in InitContainers.
	tailer := findInitContainer(spec, TranscriptTailerContainerName)
	if tailer == nil {
		t.Fatal("tailer container missing")
	}
	for _, vm := range tailer.VolumeMounts {
		if vm.Name == "persist" && !vm.ReadOnly {
			t.Error("REGRESSION (kyber#446): tailer persist mount is no longer ReadOnly after adding the pruner")
		}
	}
}

func TestAppendTranscriptPruner_CrosscheckMountsOffsetsReadOnly(t *testing.T) {
	// Default path: no offsets mount (no coupling).
	def := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptPruner(def, TranscriptPrunerConfig{RuntimeImage: "img:v1", Enabled: true, MaxAgeDays: 7, ArchiveCrosscheck: false})
	defPruner := mustInitContainerByName(t, def, TranscriptPrunerContainerName)
	for _, vm := range defPruner.VolumeMounts {
		if vm.Name == transcriptOffsetsVolumeName {
			t.Error("default path must not mount the offsets volume")
		}
	}
	// Cross-check path: offsets mounted READ-ONLY.
	cc := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptPruner(cc, TranscriptPrunerConfig{RuntimeImage: "img:v1", Enabled: true, MaxAgeDays: 7, ArchiveCrosscheck: true})
	ccPruner := mustInitContainerByName(t, cc, TranscriptPrunerContainerName)
	var found bool
	for _, vm := range ccPruner.VolumeMounts {
		if vm.Name == transcriptOffsetsVolumeName {
			found = true
			if !vm.ReadOnly {
				t.Error("cross-check offsets mount must be ReadOnly")
			}
		}
	}
	if !found {
		t.Error("cross-check path must mount the offsets volume")
	}
	env := map[string]string{}
	for _, e := range ccPruner.Env {
		env[e.Name] = e.Value
	}
	if env["PRUNE_ARCHIVE_CROSSCHECK"] != "true" {
		t.Errorf("PRUNE_ARCHIVE_CROSSCHECK = %q, want true", env["PRUNE_ARCHIVE_CROSSCHECK"])
	}
}

// --- Prune-decision logic (executes the actual sidecar script) ---------------

// runPruneOnce executes transcriptPruneOnceScript against root with the given
// policy env and returns the set of *.jsonl basenames that survive the pass.
func runPruneOnce(t *testing.T, root string, extraEnv map[string]string) map[string]bool {
	t.Helper()
	cmd := exec.Command("/bin/bash", "-c", transcriptPruneOnceScript)
	env := append(os.Environ(),
		"PRUNE_OVERLAY_ROOT="+root,
		"PRUNE_BIND_ROOT=",
		"PRUNE_MAX_AGE_DAYS=7",
		"PRUNE_MAX_BYTES=0",
		"PRUNE_OFFSET_DIR=",
		"PRUNE_ARCHIVE_CROSSCHECK=false",
	)
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prune script failed: %v\n%s", err, out)
	}
	survivors := map[string]bool{}
	entries, _ := filepath.Glob(filepath.Join(root, "*.jsonl"))
	for _, e := range entries {
		survivors[filepath.Base(e)] = true
	}
	return survivors
}

// writeJSONL writes a file of `lines` newline-terminated rows and back-dates its
// mtime by ageDays.
func writeJSONL(t *testing.T, root, name string, lines, ageDays int) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, name)
	content := ""
	for i := 0; i < lines; i++ {
		content += fmt.Sprintf("{\"i\":%d}\n", i)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPruneOnce_ArchivedPastPolicyPruned_RecentAndActiveRetained(t *testing.T) {
	root := t.TempDir()
	writeJSONL(t, root, "old-archived.jsonl", 3, 10) // 10d old → past 7d policy → prune
	writeJSONL(t, root, "recent.jsonl", 3, 1)        // 1d old → recent → retain
	writeJSONL(t, root, "active.jsonl", 3, 0)        // newest → active → retain unconditionally

	survivors := runPruneOnce(t, root, nil)

	if survivors["old-archived.jsonl"] {
		t.Error("old-archived.jsonl (10d, past 7d policy) should have been pruned")
	}
	if !survivors["recent.jsonl"] {
		t.Error("recent.jsonl (1d) must be retained")
	}
	if !survivors["active.jsonl"] {
		t.Error("active.jsonl (newest) must be retained unconditionally")
	}
}

func TestPruneOnce_NeverPrunesTheActiveFileEvenIfOld(t *testing.T) {
	// Every file is older than the policy; the newest (still old) is the active
	// session and must survive — never leave an agent with zero session files.
	root := t.TempDir()
	writeJSONL(t, root, "older.jsonl", 3, 30)
	writeJSONL(t, root, "newest-but-old.jsonl", 3, 9) // newest of the set → active

	survivors := runPruneOnce(t, root, nil)

	if survivors["older.jsonl"] {
		t.Error("older.jsonl should have been pruned")
	}
	if !survivors["newest-but-old.jsonl"] {
		t.Error("the newest file is the active session and must never be pruned")
	}
}

func TestPruneOnce_SizeBudgetEnforcedOldestFirst(t *testing.T) {
	// Three archived (old) files ~1KB each + a fresh active file. With a ~1.5KB
	// budget, the oldest archived files are pruned until total is under budget;
	// the active file is never counted against pruning.
	root := t.TempDir()
	writeJSONL(t, root, "a-oldest.jsonl", 50, 30)
	writeJSONL(t, root, "b-mid.jsonl", 50, 20)
	writeJSONL(t, root, "c-newer-archived.jsonl", 50, 10)
	writeJSONL(t, root, "active.jsonl", 50, 0)

	// Measure one file's size to set a budget that forces pruning some-but-not-all.
	fi, err := os.Stat(filepath.Join(root, "a-oldest.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	per := fi.Size()
	// Budget = 2.5 files worth → must prune the oldest archived file(s) until the
	// total (4 files) drops to <= budget, i.e. prune at least the 2 oldest.
	budget := per*5/2 + 1

	survivors := runPruneOnce(t, root, map[string]string{
		"PRUNE_MAX_AGE_DAYS": "7",
		"PRUNE_MAX_BYTES":    fmt.Sprintf("%d", budget),
	})

	if !survivors["active.jsonl"] {
		t.Error("active file must always be retained")
	}
	if survivors["a-oldest.jsonl"] {
		t.Error("oldest archived file should be pruned first under the size budget")
	}
	// Total surviving transcript bytes must be at or under the budget.
	var total int64
	for name := range survivors {
		fi, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			continue
		}
		total += fi.Size()
	}
	if total > budget {
		names := make([]string, 0, len(survivors))
		for n := range survivors {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Errorf("surviving total %d bytes exceeds budget %d (survivors=%v)", total, budget, names)
	}
}

func TestPruneOnce_CrosscheckRetainsUnarchivedOldFile(t *testing.T) {
	// An OLD file (past the age policy) that the cross-check cannot confirm was
	// fully shipped must be RETAINED — the "un-archived → retained" AC. A second
	// old file WITH a complete checkpoint is pruned, proving the gate is real.
	root := t.TempDir()
	offsetDir := t.TempDir()

	unshipped := writeJSONL(t, root, "old-unshipped.jsonl", 5, 10)
	shipped := writeJSONL(t, root, "old-shipped.jsonl", 5, 10)
	writeJSONL(t, root, "active.jsonl", 5, 0)

	// Checkpoint key is md5(absolute path), value = shipped line count. Only the
	// "shipped" file gets a complete checkpoint (>= its 5 lines); the other none.
	writeCheckpoint(t, offsetDir, shipped, 5)
	_ = unshipped // intentionally no checkpoint

	survivors := runPruneOnce(t, root, map[string]string{
		"PRUNE_ARCHIVE_CROSSCHECK": "true",
		"PRUNE_OFFSET_DIR":         offsetDir,
	})

	if !survivors["old-unshipped.jsonl"] {
		t.Error("old file with no ship checkpoint must be retained under cross-check (un-archived → retained)")
	}
	if survivors["old-shipped.jsonl"] {
		t.Error("old file confirmed fully shipped should be pruned under cross-check")
	}
	if !survivors["active.jsonl"] {
		t.Error("active file must always be retained")
	}
}

func writeCheckpoint(t *testing.T, offsetDir, filePath string, shippedLines int) {
	t.Helper()
	// Mirror the script's checkpoint path: md5sum of the file path.
	out, err := exec.Command("/bin/bash", "-c",
		fmt.Sprintf("printf '%%s' %q | md5sum | cut -d' ' -f1", filePath)).Output()
	if err != nil {
		t.Fatal(err)
	}
	key := string(out)
	key = key[:len(key)-1] // drop trailing newline
	if err := os.WriteFile(filepath.Join(offsetDir, key), []byte(fmt.Sprintf("%d\n", shippedLines)), 0o644); err != nil {
		t.Fatal(err)
	}
}
