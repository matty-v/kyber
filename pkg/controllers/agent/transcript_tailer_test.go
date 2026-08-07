package agent

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestAppendTranscriptTailer_NoOpWhenImageEmpty mirrors the status-sidecar
// guard: dev installs / unit tests that don't resolve a runtime image must not
// get the sidecar injected (an empty image ref is unschedulable).
func TestAppendTranscriptTailer_NoOpWhenImageEmpty(t *testing.T) {
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent"}},
	}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: ""})
	if len(spec.Containers) != 1 {
		t.Errorf("empty image must not inject; got %d containers", len(spec.Containers))
	}
}

// TestAppendTranscriptTailer_AppendsContainer verifies the sidecar is appended
// with the stable container name (kubectl/PWA tooling targets it predictably,
// satisfying the "container is present on the pod spec" AC).
func TestAppendTranscriptTailer_AppendsContainer(t *testing.T) {
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent"}},
	}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "ghcr.io/matty-v/kyber-runtime:v1"})
	// kyber#575: the tailer is now a native sidecar in InitContainers; the regular
	// Containers slice is unchanged (still just the agent).
	if len(spec.Containers) != 1 {
		t.Fatalf("regular containers must be unchanged (agent only); got %d", len(spec.Containers))
	}
	side := mustInitContainerByName(t, spec, TranscriptTailerContainerName)
	if side.Name != TranscriptTailerContainerName {
		t.Errorf("container name: got %q, want %q", side.Name, TranscriptTailerContainerName)
	}
	if TranscriptTailerContainerName != "transcript-tailer" {
		t.Errorf("container name constant drifted: got %q, want transcript-tailer", TranscriptTailerContainerName)
	}
}

// TestAppendTranscriptTailer_ReusesRuntimeImage is the uid-alignment AC: the
// sidecar must run as the same image (hence same uid) that owns the JSONL files
// on the read-only PVC, so it can read them without loosening file perms.
func TestAppendTranscriptTailer_ReusesRuntimeImage(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	const img = "ghcr.io/matty-v/kyber-runtime:abc123@sha256:deadbeef"
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: img})
	side := mustInitContainerByName(t, spec, TranscriptTailerContainerName)
	if side.Image != img {
		t.Errorf("image: got %q, want the agent runtime image %q (uid alignment)", side.Image, img)
	}
}

// TestAppendTranscriptTailer_ReadOnlyPersistMount is the "cannot mutate agent
// state" AC at the volume level: the persist PVC is mounted read-only at the
// documented mount path.
func TestAppendTranscriptTailer_ReadOnlyPersistMount(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "img:v1"})
	side := mustInitContainerByName(t, spec, TranscriptTailerContainerName)
	var mount *corev1.VolumeMount
	for i := range side.VolumeMounts {
		if side.VolumeMounts[i].Name == "persist" {
			mount = &side.VolumeMounts[i]
			break
		}
	}
	if mount == nil {
		t.Fatal("transcript-tailer must mount the 'persist' volume")
	}
	if !mount.ReadOnly {
		t.Error("persist mount must be ReadOnly (sidecar cannot mutate agent state)")
	}
	if mount.MountPath != transcriptMountPath {
		t.Errorf("mount path: got %q, want %q", mount.MountPath, transcriptMountPath)
	}
}

// TestAppendTranscriptTailer_SecurityHardened is the security AC: non-root,
// readOnlyRootFilesystem, allowPrivilegeEscalation=false.
func TestAppendTranscriptTailer_SecurityHardened(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "img:v1"})
	sc := mustInitContainerByName(t, spec, TranscriptTailerContainerName).SecurityContext
	if sc == nil {
		t.Fatal("security context unset")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem must be true")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be false")
	}
	// kyber#451: the tailer reuses the agent runtime image, whose default USER
	// is root, and the session JSONL it must read is root-owned (the agent
	// container runs privileged as root). So the tailer runs as root (uid 0) —
	// like the agent + session-brief init container (pod_builder.go) — to read
	// those files over the read-only mount. It MUST NOT claim RunAsNonRoot: a
	// root-effective-uid container with RunAsNonRoot=true is rejected by kubelet
	// admission ("container has runAsNonRoot and image will run as root").
	if sc.RunAsUser == nil || *sc.RunAsUser != 0 {
		t.Errorf("RunAsUser must be 0 (root) to read root-owned JSONL; got %v", sc.RunAsUser)
	}
	if sc.RunAsNonRoot != nil && *sc.RunAsNonRoot {
		t.Error("RunAsNonRoot must NOT be true on a root-defaulting image without a non-root RunAsUser (kyber#451 admission regression)")
	}
}

// TestAppendTranscriptTailer_AdmittableAgainstRootRuntimeImage is the kyber#451
// regression guard. The kubelet rejects a container with
// `RunAsNonRoot: true` whose effective user is root (no RunAsUser, root-default
// image) — `CreateContainerConfigError: container has runAsNonRoot and image
// will run as root`. Because the tailer reuses the agent runtime image (root
// USER by default), the only admittable configurations are (a) an explicit
// non-zero RunAsUser, or (b) not asserting RunAsNonRoot. We take (b) + explicit
// RunAsUser:0 (root, to read the root-owned JSONL). This test fails for the
// exact misconfiguration that bricked new-agent bootstrap.
func TestAppendTranscriptTailer_AdmittableAgainstRootRuntimeImage(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "ghcr.io/matty-v/kyber-claude-code:v1"})
	sc := mustInitContainerByName(t, spec, TranscriptTailerContainerName).SecurityContext
	if sc == nil {
		t.Fatal("security context unset")
	}
	// The runtime image defaults to a root USER. Model the kubelet admission
	// rule: reject iff RunAsNonRoot is asserted true while the effective user is
	// root (RunAsUser nil or 0).
	runsAsNonRoot := sc.RunAsNonRoot != nil && *sc.RunAsNonRoot
	effectiveUserIsRoot := sc.RunAsUser == nil || *sc.RunAsUser == 0
	if runsAsNonRoot && effectiveUserIsRoot {
		t.Fatalf("ADMISSION REGRESSION (kyber#451): RunAsNonRoot=true with root effective user — kubelet would reject with CreateContainerConfigError. RunAsUser=%v", sc.RunAsUser)
	}
	// Sanity: the tailer must NOT be privileged (it only reads files) — it is
	// strictly more locked down than the agent container despite sharing its uid.
	if sc.Privileged != nil && *sc.Privileged {
		t.Error("transcript-tailer must not be privileged")
	}
}

// TestAppendTranscriptTailer_ResourceLimits asserts the tailer carries a
// resource budget (10m/64Mi req, 250m/512Mi lim — the 512Mi limit absorbs a
// cold-start full re-ship of a large transcript backlog; the old 64Mi OOM-killed
// big-transcript agents in a crash loop).
func TestAppendTranscriptTailer_ResourceLimits(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "img:v1"})
	side := mustInitContainerByName(t, spec, TranscriptTailerContainerName)

	// Pin the EXACT resource budget, not just presence. A drift of any of these
	// four values must fail this test — in particular a revert of the #466 OOM
	// fix (memory limit 512Mi -> 64Mi) must be caught. Compare parsed quantities
	// with Quantity.Equal so "100m" vs "0.1" style equivalents still match by
	// value, and an unset field (zero quantity) fails the same as a wrong value.
	cases := []struct {
		name string
		got  resource.Quantity
		want string
	}{
		{"CPU request", *side.Resources.Requests.Cpu(), "10m"},
		{"memory request", *side.Resources.Requests.Memory(), "64Mi"},
		{"CPU limit", *side.Resources.Limits.Cpu(), "250m"},
		{"memory limit", *side.Resources.Limits.Memory(), "512Mi"},
	}
	for _, tc := range cases {
		want := resource.MustParse(tc.want)
		if !tc.got.Equal(want) {
			t.Errorf("%s = %s; want %s", tc.name, tc.got.String(), want.String())
		}
	}
}

// TestAppendTranscriptTailer_NoLivenessProbe: the brief/design specify no
// liveness probe (a tail loop has no health endpoint; restarts are
// pod-restart/OOM only).
func TestAppendTranscriptTailer_NoLivenessProbe(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "img:v1"})
	if mustInitContainerByName(t, spec, TranscriptTailerContainerName).LivenessProbe != nil {
		t.Error("transcript-tailer must not declare a liveness probe")
	}
}

// TestAppendTranscriptTailer_CommandShipsBothRootsWithResume is the load-bearing
// behavior AC: the ship loop must scan BOTH mode-dependent PVC roots (overlay +
// bind-mount fallback), resume each file from its persisted line offset (default
// line 1 when no checkpoint, so the head is never lost), and never `tail -n 0`.
func TestAppendTranscriptTailer_CommandShipsBothRootsWithResume(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "img:v1"})
	side := mustInitContainerByName(t, spec, TranscriptTailerContainerName)
	cmd := strings.Join(side.Command, " ") + " " + strings.Join(side.Args, " ")

	if !strings.Contains(cmd, transcriptProjectsOverlayRoot) {
		t.Errorf("command must scan the overlay root %q; cmd=%q", transcriptProjectsOverlayRoot, cmd)
	}
	if !strings.Contains(cmd, transcriptProjectsBindRoot) {
		t.Errorf("command must scan the bind-mount-fallback root %q; cmd=%q", transcriptProjectsBindRoot, cmd)
	}
	// Resume from the persisted line checkpoint: start = shipped + 1 (the line
	// after the last one shipped), defaulting to line 1 when no checkpoint exists
	// (shipped == 0), so the head is never lost.
	if !strings.Contains(cmd, "start=$((shipped + 1))") {
		t.Errorf("command must resume one line past the checkpoint (start=$((shipped + 1))); cmd=%q", cmd)
	}
	if strings.Contains(cmd, "-n 0") {
		t.Errorf("command must NOT use `tail -n 0` (gaps the head of every session); cmd=%q", cmd)
	}
	if !strings.Contains(cmd, "*.jsonl") {
		t.Errorf("command must discover *.jsonl session files; cmd=%q", cmd)
	}
}

// TestAppendTranscriptTailer_SingleProcessNoPerFileFanOut is the kyber#584 core
// model assertion: the tailer is ONE poll loop, not a per-file `tail -F`
// follower-process fan-out. The OOM root cause was a follower process per
// session file (memory scaling with file COUNT); this guard fails if the script
// reverts to `tail -F`, a backgrounded per-file subshell, or a concurrency cap
// (all hallmarks of the old per-file-process model).
func TestAppendTranscriptTailer_SingleProcessNoPerFileFanOut(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "img:v1"})
	cmd := strings.Join(mustInitContainerByName(t, spec, TranscriptTailerContainerName).Command, " ")

	for _, banned := range []string{
		"tail -F",     // a never-EOF follower process per file is the OOM root cause
		"--pid=",      // the old per-file follower's lifetime tie
		"MAX_TAILS",   // a concurrency cap only exists when there ARE concurrent followers
		"COLD_TARGET", // the old cold-tail bookkeeping
		"evict_oldest",
	} {
		if strings.Contains(cmd, banned) {
			t.Errorf("script must NOT contain %q — kyber#584 replaced the per-file `tail -F` fan-out "+
				"with a single-process incremental reader; cmd=%q", banned, cmd)
		}
	}
	// The single loop ships each file in-process via ship_file (no backgrounding).
	if !strings.Contains(cmd, "ship_file") {
		t.Errorf("script must ship each file through the single-process ship_file helper; cmd=%q", cmd)
	}
}

// TestAppendTranscriptTailer_OffsetCheckpointVolume is the kyber#467 durable-fix
// AC: the offsets checkpoint volume is now a DEDICATED per-agent RWO PVC (not the
// pod-lifetime emptyDir of kyber#458, which was wiped on pod recreation and
// triggered a full-backlog re-ship). The mount stays WRITABLE; the agent persist
// PVC stays read-only (#446 intact). The offsets PVC references the dedicated
// OffsetsPVCName claim, NOT the persist PVC.
func TestAppendTranscriptTailer_OffsetCheckpointVolume(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "img:v1"})

	var vol *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == transcriptOffsetsVolumeName {
			vol = &spec.Volumes[i]
			break
		}
	}
	if vol == nil {
		t.Fatalf("pod must have the %q offsets volume", transcriptOffsetsVolumeName)
	}
	// Durability: the offsets volume MUST be a PVC (survives pod recreation), not
	// the old ephemeral emptyDir.
	if vol.EmptyDir != nil {
		t.Error("offsets volume must NOT be an emptyDir (kyber#467: it was lost on pod recreation → full re-ship)")
	}
	if vol.PersistentVolumeClaim == nil {
		t.Fatalf("offsets volume must be a PersistentVolumeClaim (durable across pod recreation)")
	}
	if got, want := vol.PersistentVolumeClaim.ClaimName, OffsetsPVCName("alice"); got != want {
		t.Errorf("offsets PVC claim name: got %q, want %q (dedicated offsets PVC)", got, want)
	}
	// It must reference the OFFSETS claim, never the persist PVC.
	if vol.PersistentVolumeClaim.ClaimName == PVCName("alice") {
		t.Errorf("offsets volume must not reference the persist PVC %q", PVCName("alice"))
	}

	side := mustInitContainerByName(t, spec, TranscriptTailerContainerName)
	var mount *corev1.VolumeMount
	for i := range side.VolumeMounts {
		if side.VolumeMounts[i].Name == transcriptOffsetsVolumeName {
			mount = &side.VolumeMounts[i]
			break
		}
	}
	if mount == nil {
		t.Fatalf("tailer must mount the %q offsets volume", transcriptOffsetsVolumeName)
	}
	if mount.ReadOnly {
		t.Error("offsets mount must be WRITABLE (the tailer persists checkpoints there)")
	}
	if mount.MountPath != transcriptOffsetsDir {
		t.Errorf("offsets mount path: got %q, want %q", mount.MountPath, transcriptOffsetsDir)
	}

	// The agent persist PVC must REMAIN read-only — no access-mode change (#446).
	for i := range side.VolumeMounts {
		if side.VolumeMounts[i].Name == "persist" && !side.VolumeMounts[i].ReadOnly {
			t.Error("persist PVC mount must stay ReadOnly — kyber#467 must not loosen the agent PVC")
		}
	}
}

// TestAppendTranscriptTailer_ActiveSetBounding is the kyber#584 Phase A AC: the
// live working set must track ACTIVITY, not total history. An idle file (byte
// size unchanged since its checkpoint) is skipped with a single stat and is NOT
// held in any live tail set; it is re-admitted only when it grows. The byte-size
// companion is what makes the idle-skip O(1) per file instead of a full re-read.
// (Behavioral proof — that the working set actually tracks activity — is in the
// executable TestTranscriptTailerScript_* tests below.)
func TestAppendTranscriptTailer_ActiveSetBounding(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "img:v1"})
	cmd := strings.Join(mustInitContainerByName(t, spec, TranscriptTailerContainerName).Command, " ")

	// The idle-skip fast path compares the file's current byte size against the
	// size recorded at its last checkpoint and returns early when unchanged.
	if !strings.Contains(cmd, "cur_size") || !strings.Contains(cmd, "last_size") {
		t.Errorf("script must compare current vs checkpointed byte size for the idle-skip fast path; cmd=%q", cmd)
	}
	if !strings.Contains(cmd, `if [ "$cur_size" = "$last_size" ]; then`) {
		t.Errorf("script must fast-skip a file whose byte size is unchanged (idle, no new content); cmd=%q", cmd)
	}
	// The additive size companion lives alongside the line checkpoint.
	if !strings.Contains(cmd, `sizeck="${ckpt}.size"`) {
		t.Errorf("script must persist a byte-size companion (<ckpt>.size) for re-admit-on-growth; cmd=%q", cmd)
	}
}

// TestAppendTranscriptTailer_ClampsStaleCheckpoint is the security AC (Obi-wan +
// Ackbar builder note): now that the checkpoint is DURABLE it can outlive a file
// rotation/truncation. A stored offset beyond the file's current EOF must NOT
// silently skip the now-shorter file's lines — the script clamps and re-ships
// from line 1 (a duplicate is benign; a skipped line is an audit hole).
func TestAppendTranscriptTailer_ClampsStaleCheckpoint(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "img:v1"})
	cmd := strings.Join(mustInitContainerByName(t, spec, TranscriptTailerContainerName).Command, " ")

	// The script must measure the file's current line count and guard the resume
	// point against it (clamp when start would point past EOF+1).
	if !strings.Contains(cmd, "wc -l") {
		t.Errorf("script must read the file's current line count to clamp a stale offset; cmd=%q", cmd)
	}
	if !strings.Contains(cmd, "total + 1") {
		t.Errorf("script must clamp the resume start against (line count + 1) so a stale durable checkpoint can't skip a rotated/truncated file; cmd=%q", cmd)
	}
}

// TestAppendTranscriptTailer_ScriptCheckpointsShippedLines verifies the script
// both READS a per-file checkpoint to resume and WRITES the running shipped-line
// count back to the offsets dir — the mechanism that makes a sidecar restart
// resume rather than re-ship (kyber#458 AC#1/#8).
func TestAppendTranscriptTailer_ScriptCheckpointsShippedLines(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "img:v1"})
	cmd := strings.Join(mustInitContainerByName(t, spec, TranscriptTailerContainerName).Command, " ")

	if !strings.Contains(cmd, transcriptOffsetsDir) {
		t.Errorf("script must reference the offsets dir %q; cmd=%q", transcriptOffsetsDir, cmd)
	}
	if !strings.Contains(cmd, "checkpoint_path") || !strings.Contains(cmd, "start=$((shipped + 1))") {
		t.Errorf("script must read a checkpoint and resume at last-shipped+1; cmd=%q", cmd)
	}
	if !strings.Contains(cmd, "print NR > ck") {
		t.Errorf("script must persist the running absolute shipped-line count to the checkpoint; cmd=%q", cmd)
	}
}

// TestAppendTranscriptTailer_ShipAwkFlushesWithoutInteractive is the regression
// guard for the mawk `-W interactive` strand bug. `-W interactive` was ORIGINALLY
// added for prompt shipping, but on the runtime image's mawk (Thomas Dickey's
// mawk 1.3.4) it truncates record processing once the `NR<s{next}` resume rule
// fires: the FIRST ship of a session file works (start=1, no `next`), but every
// RESUME (start>1) over a file larger than mawk's interactive read buffer ships
// only a truncated prefix, silently stranding the rest of the transcript (which
// the unconditional size checkpoint then made permanent). The explicit `fflush()`
// per line already provides the prompt-flush `-W interactive` was meant for, so
// the fix DROPS `-W interactive` and keeps fflush(). This guard fails if
// `-W interactive` is ever re-added to the ship awk.
func TestAppendTranscriptTailer_ShipAwkFlushesWithoutInteractive(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "img:v1"})
	cmd := strings.Join(mustInitContainerByName(t, spec, TranscriptTailerContainerName).Command, " ")

	// Match the actual flag USAGE ("mawk -W interactive"), not a bare "-W
	// interactive" substring — the script comment explains the removed flag and
	// legitimately names it, but never adjacent to the mawk invocation.
	if strings.Contains(cmd, "mawk -W interactive") {
		t.Errorf("the ship awk must NOT use `mawk -W interactive` — on mawk 1.3.4 it strands resumed lines "+
			"(ships too few once NR<s{next} fires over a large file); rely on fflush() instead; cmd=%q", cmd)
	}
	if !strings.Contains(cmd, "fflush()") {
		t.Errorf("the ship awk must fflush() per line so Vector sees each line promptly without `-W interactive`; cmd=%q", cmd)
	}
}

// TestTranscriptProjectsRoots pins the two documented PVC roots to the layouts
// produced by images/agent-base/entrypoint.sh: overlay mode upper-dir vs the
// bind-mount-HOME fallback. These are the AC's "named, commented constant"
// (not inline literals) so they survive overlay/runtime changes.
func TestTranscriptProjectsRoots(t *testing.T) {
	if transcriptProjectsOverlayRoot != "/agent-home/overlay/upper/home/kyber/.claude/projects" {
		t.Errorf("overlay root drifted: %q", transcriptProjectsOverlayRoot)
	}
	if transcriptProjectsBindRoot != "/agent-home/home/.claude/projects" {
		t.Errorf("bind-mount root drifted: %q", transcriptProjectsBindRoot)
	}
}

// TestBuildPodSpec_PlusTranscriptTailer is the integration-style coverage: the
// full assembly (BuildPodSpec → AppendStatusSidecar → AppendTranscriptTailer)
// keeps the agent as the sole regular container and lands the status-sidecar +
// transcript-tailer as native sidecars in InitContainers (kyber#575).
func TestBuildPodSpec_PlusTranscriptTailer(t *testing.T) {
	t.Setenv("KYBER_CONTROL_PLANE_INTERNAL_URL", "http://kyber-control-plane.kyber-system:8082")
	agent := testAgent()
	adapter := testAdapter()

	spec, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}
	AppendStatusSidecar(&spec, SidecarConfig{AgentName: agent.Name, Image: "ghcr.io/matty-v/kyber-status-sidecar:v1"})
	AppendTranscriptTailer(&spec, TranscriptTailerConfig{AgentName: agent.Name, RuntimeImage: spec.Containers[0].Image})

	if len(spec.Containers) != 1 {
		t.Fatalf("expected 1 regular container (runtime), got %d", len(spec.Containers))
	}
	if spec.Containers[0].Name != "agent" {
		t.Errorf("regular container should be 'agent' (runtime); got %q", spec.Containers[0].Name)
	}
	tailer := mustInitContainerByName(t, &spec, TranscriptTailerContainerName)
	// uid alignment: tailer reuses the runtime image already on the pod.
	if tailer.Image != spec.Containers[0].Image {
		t.Errorf("tailer image %q must equal runtime image %q", tailer.Image, spec.Containers[0].Image)
	}
}
