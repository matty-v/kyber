// Transcript-tailer sidecar injection for agent pods (kyber#446). Adds the
// transcript-tailer container to the pod spec built by BuildPodSpec. The
// sidecar tails the agent's Claude Code session JSONL
// (~/.claude/projects/<project>/*.jsonl) off the agent PVC and writes each line
// to its own stdout, where the existing Vector log-shipper picks it up and
// routes it (by container_name) to the transcripts/<agent>/<date>/ object
// prefix — a clean, isolated lane separate from the [kyber] boot stdout that
// the durable archive (kyber#437) ships.
//
// Kept separate from pod_builder.go for the same reasons as status_sidecar.go:
// the injection is platform-level (not adapter-level) and unit-testable without
// faking the full pod-spec assembly.
//
// Why a sidecar (not a stdout tail in the start script): the [kyber] wrapper
// owns the agent container's stdout and already writes boot noise; interleaving
// JSONL there would force the reader to heuristically separate JSON from plain
// lines and risk corrupting multi-line flushes. A separate container hands
// Vector a clean stream it routes by container_name with zero guesswork.

package agent

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// TranscriptTailerContainerName is the container name in the pod spec.
	// Kept stable (same rationale as StatusSidecarContainerName) so Vector's
	// container_name routing rule and kubectl/PWA tooling can target it
	// predictably.
	TranscriptTailerContainerName = "transcript-tailer"

	// transcriptMountPath is where the agent "persist" PVC is mounted
	// READ-ONLY inside the sidecar. We mount the raw PVC (not the agent's
	// merged overlay view), so the JSONL files live under one of the two
	// mode-dependent roots below.
	transcriptMountPath = "/agent-home"

	// transcriptProjectsOverlayRoot and transcriptProjectsBindRoot are the two
	// physical locations of the agent's ~/.claude/projects/ on the PVC,
	// depending on which persistence mode images/agent-base/entrypoint.sh
	// selected at boot. They are named, commented constants (not inline
	// literals) per the kyber#446 AC, so they survive overlay/runtime changes:
	//
	//   - Kernel/fuse overlay (UPPER_DIR="$PERSIST_DIR/overlay/upper"):
	//     $HOME (/home/kyber) writes land in the overlay upper-dir, so
	//     ~/.claude/projects resolves to .../overlay/upper/home/kyber/.claude/projects.
	//   - Bind-mount-HOME fallback (HOME_PERSIST="$PERSIST_DIR/home", bind-mounted
	//     over $HOME, incl. the selective-symlink sub-fallback for ~/.claude):
	//     ~/.claude/projects resolves to .../home/.claude/projects.
	//
	// The discovery loop iterates whichever exists at runtime. (Confirmed
	// against a live pod: the overlay layout is the one in use in production.)
	transcriptProjectsOverlayRoot = transcriptMountPath + "/overlay/upper/home/kyber/.claude/projects"
	transcriptProjectsBindRoot    = transcriptMountPath + "/home/.claude/projects"
	transcriptCodexOverlayRoot    = transcriptMountPath + "/overlay/upper/home/kyber/.codex/sessions"
	transcriptCodexBindRoot       = transcriptMountPath + "/home/.codex/sessions"

	// transcriptOffsetsVolumeName / transcriptOffsetsDir are the DURABLE per-agent
	// checkpoint volume that persists each session file's last-shipped line count
	// across BOTH sidecar (container) restarts AND pod recreation (kyber#467).
	//
	// History: kyber#458 first added a checkpoint on a pod-lifetime emptyDir to
	// stop a sidecar restart from re-shipping. But an emptyDir is wiped on POD
	// recreation, and the agent's old session *.jsonl survive on the persist PVC
	// (read-only, #446) — so after a pod recreation the tailer found every
	// historical file with no checkpoint and re-shipped each from line 1, fanning
	// out up to MAX_TAILS concurrent tail|mawk and OOMing the sidecar (#466). The
	// #466 512Mi bump tolerated that burst; this is the durable fix that removes
	// it: the checkpoint now lives on a dedicated, small, WRITABLE RWO PVC
	// (OffsetsPVCName) so it survives pod recreation and every old file resumes at
	// ~EOF (≈zero re-ship). The agent "persist" PVC STAYS read-only — only this
	// separate offsets PVC is writable (exactly the posture the emptyDir already
	// established, now made durable), so #446 is intact and no persist
	// access-mode/StorageClass change is needed.
	transcriptOffsetsVolumeName = "transcript-offsets"
	transcriptOffsetsDir        = "/var/run/kyber-transcript-offsets"
)

// TranscriptTailerConfig carries the fields the controller threads through to
// the sidecar. RuntimeImage is REQUIRED and gates injection: it must be the
// agent's own runtime image (podSpec.Containers[0].Image) so the sidecar runs
// as the same uid that owns the JSONL files — that uid alignment is what lets
// the read-only PVC mount be readable without loosening file permissions (it's
// a security feature, not just a convenience). AgentName is informational
// (surfaced as AGENT_NAME for parity with the other sidecars / debugging).
type TranscriptTailerConfig struct {
	AgentName    string
	RuntimeImage string
	Runtime      string
}

// AppendTranscriptTailer appends the transcript-tailer container to pod's
// Containers slice. No-op when cfg.RuntimeImage is empty (dev installs / tests),
// mirroring AppendStatusSidecar's image guard so older deployments and unit
// tests keep their existing pod-spec shape.
func AppendTranscriptTailer(spec *corev1.PodSpec, cfg TranscriptTailerConfig) {
	if cfg.RuntimeImage == "" {
		return
	}

	container := corev1.Container{
		Name:    TranscriptTailerContainerName,
		Image:   cfg.RuntimeImage,
		Command: []string{"/bin/bash", "-c", transcriptTailScript},
		Env: []corev1.EnvVar{
			{Name: "AGENT_NAME", Value: cfg.AgentName},
			{Name: "TRANSCRIPT_OVERLAY_ROOT", Value: transcriptRoots(cfg.Runtime, transcriptProjectsOverlayRoot, transcriptCodexOverlayRoot)},
			{Name: "TRANSCRIPT_BIND_ROOT", Value: transcriptRoots(cfg.Runtime, transcriptProjectsBindRoot, transcriptCodexBindRoot)},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("250m"),
				// 512Mi headroom from #466. Originally added because a POD
				// recreation wiped the emptyDir checkpoint and the tailer
				// re-shipped every session from line 1, fanning out up to
				// MAX_TAILS=64 concurrent tail|mawk over multi-MB JSON lines and
				// OOMing the old 64Mi limit. kyber#467 removes that burst at the
				// source — the checkpoint is now durable (offsets PVC) so cold
				// from-line-1 ships are rare and bounded by MAX_COLD_TAILS, making
				// peak memory independent of backlog size. The limit is kept here
				// for safety margin; reverting it back down is tracked with #466
				// (deliberately out of scope for this change).
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
		// Read-only PVC mount: the sidecar reads session JSONL and writes
		// nothing back, so it cannot mutate agent state or transcript files
		// (kyber#446 security AC). The offsets mount (kyber#467) is a separate
		// WRITABLE per-agent PVC for the tail checkpoints — it does NOT touch the
		// agent PVC, which stays read-only.
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "persist",
				MountPath: transcriptMountPath,
				ReadOnly:  true,
			},
			{
				Name:      transcriptOffsetsVolumeName,
				MountPath: transcriptOffsetsDir,
				// Writable (no ReadOnly): the tailer persists per-file shipped
				// line counts here so a sidecar restart OR pod recreation resumes
				// instead of re-shipping (kyber#467 — durable offsets PVC).
			},
		},
		// Run as root (uid 0), like the agent + session-brief containers
		// (pod_builder.go). kyber#451: the reused runtime image defaults to a
		// root USER and the session JSONL is root-owned (the agent runs
		// privileged as root), so the tailer must be root to read those files
		// over the read-only mount — root bypasses DAC, so the read is
		// guaranteed regardless of the file owner. Asserting RunAsNonRoot here
		// (as the original #447 code did, with NO RunAsUser to back it) is what
		// bricked new-agent admission: kubelet rejects a root-effective-uid
		// container that claims runAsNonRoot ("image will run as root").
		//
		// Posture is still tight despite root: NOT privileged, no added
		// capabilities, no privilege escalation, and a read-only root
		// filesystem — strictly more locked down than the privileged agent
		// container it reads alongside. A fully non-root tailer would require
		// making the whole agent runtime non-root (image USER + chown), tracked
		// as the larger follow-up in kyber#451; this is the minimal unbrick.
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptrTo(int64(0)),
			ReadOnlyRootFilesystem:   ptrTo(true),
			AllowPrivilegeEscalation: ptrTo(false),
		},
		// No liveness probe: a tail loop exposes no health endpoint. Restarts
		// are pod-restart/OOM only; on a pod recreation the durable offsets PVC
		// (kyber#467) means each old session file resumes near EOF (≈zero
		// re-ship) and a duplicate audit line is benign where a missing one is an
		// audit hole.
		//
		// Native sidecar (kyber#575): RestartPolicy:Always makes the kubelet
		// restart the tailer on ANY exit (the OOMKill case that left lando/yoda's
		// tailer permanently dead under pod-level Never), so it self-heals instead
		// of leaving the pod NotReady. The tail script (set -u, no set -e) already
		// tolerates starting before the agent's transcript files exist: each poll
		// does `[ -d "$root" ] || continue` then `find ... 2>/dev/null`, so a
		// missing projects root is skipped and retried every 5s — no crash-loop.
		// Native-sidecar teardown also runs AFTER the agent exits, giving the
		// tailer a window to flush final transcript lines.
		RestartPolicy: ptrTo(corev1.ContainerRestartPolicyAlways),
	}
	spec.InitContainers = append(spec.InitContainers, container)

	// Durable per-agent PVC for the tail offset checkpoints (kyber#467). A
	// dedicated, small RWO PVC (OffsetsPVCName) — NOT the pod-lifetime emptyDir of
	// kyber#458, which was wiped on pod recreation and triggered a full-backlog
	// re-ship (#466). It's a sibling volume to the persist PVC, mounted writable
	// (the RO rootfs only covers image layers, not mounted volumes), so the agent
	// persist PVC stays read-only (#446 intact) and no persist access-mode change
	// is needed. The reconciler ensures/owner-refs this PVC before the pod is
	// created. Idempotent: don't double-add if a re-injection already appended it.
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == transcriptOffsetsVolumeName {
			return
		}
	}
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: transcriptOffsetsVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: OffsetsPVCName(cfg.AgentName),
			},
		},
	})
}

func transcriptRoots(runtime, claudeRoot, codexRoot string) string {
	if runtime == "codex" {
		return codexRoot
	}
	return claudeRoot
}

// transcriptTailScript is the sidecar's discover-and-ship loop. kyber#584
// rebuilt it as a SINGLE-PROCESS, poll-based, fixed-buffer incremental reader so
// the tailer's peak memory is bounded by the ACTIVE session set, NOT by the
// total number of historical session files. The previous model fanned out one
// backgrounded `tail -F | mawk` follower PROCESS per session file (bounded
// MAX_TAILS=64 concurrent, plus the churn of starting-then-evicting hundreds
// more in a single discovery pass). On an aged agent (r2-d2: 834 files / ~20 MB)
// that per-file process model — process control blocks + fd tables + per-process
// mawk/glibc arenas — pushed the sidecar past its 512Mi limit and OOM-looped
// (exitCode 137), even though the byte backlog was tiny. Memory scaled with file
// COUNT. This loop removes that axis entirely: there are NO persistent per-file
// processes and NO in-memory per-file maps — peak RSS is O(one line of one
// active file), constant in file count.
//
// Each poll, for every *.jsonl under the two documented PVC roots:
//
//   - Phase A (active-set bounding, kyber#584 Path 2): an idle file — one whose
//     byte size is unchanged since its last checkpoint — is skipped with a
//     single `stat`, never re-read. JSONL is append-only, so an unchanged size
//     means no new content. A file is re-admitted automatically the poll its
//     size grows again. On r2-d2 this collapses ~834 watched files to ~1 (the
//     live session). The byte-size companion ("<md5>.size") is ADDITIVE — the
//     old script never read it, so an upgrade/rollback is safe.
//
//   - Phase B (single-process incremental read, kyber#584 Path 1): an active
//     file is read ONCE through `mawk`, which streams a line at a time (bounded
//     buffer — never the whole file), shipping only the un-shipped COMPLETE
//     lines from the durable line checkpoint (kyber#467) forward and advancing
//     that checkpoint per line. Files are processed one at a time, sequentially,
//     so peak memory is independent of how many files are active at once.
//
// The durable per-file OFFSET CHECKPOINT (kyber#467) is preserved UNCHANGED: a
// flat md5-named file holding the last-shipped absolute line count. First sight
// of a file with no checkpoint ships from line 1 (completeness — no head lines
// lost); on a sidecar restart OR a pod recreation the in-memory loop state is
// gone but the checkpoint survives on the PVC, so each file resumes at the line
// after the last one shipped instead of re-shipping the whole session. The
// format is byte-for-byte the same as before, so existing offsets stay valid
// across BOTH an upgrade to this script AND a rollback to the old one.
//
// Two guards keep the durable checkpoint safe and bounded:
//   - Stale-checkpoint clamp: a durable offset can outlive a file
//     rotation/truncation. If the resume point is past the file's current EOF,
//     we re-ship from line 1 rather than silently skip the now-shorter file's
//     lines (a duplicate is benign; a skipped line is an audit hole).
//   - Partial-line safety: `total` is `wc -l` (newline-terminated lines only)
//     and shipping stops at NR>total, so a half-written trailing line (no
//     newline yet) is never shipped or checkpointed — it ships next poll once
//     its newline lands.
//
// Deliberately NOT `tail -n 0`: -n 0 starts at end-of-file, dropping the head of
// any file already non-empty at first poll — that loses lines on EVERY session,
// not just on restart, which is a correctness failure for an audit surface.
//
// The roots, offset dir, and poll cadence default to the documented constants
// but are env-overridable (TRANSCRIPT_OVERLAY_ROOT / TRANSCRIPT_BIND_ROOT /
// TRANSCRIPT_OFFSET_DIR / TRANSCRIPT_POLL_SECONDS) so the loop can be exercised
// against a fixture in tests; TRANSCRIPT_POLL_LIMIT (0 = run forever, the
// production default) bounds the poll count for the same reason. None of these
// are set on the production container, so its behavior is unchanged.
var transcriptTailScript = fmt.Sprintf(`set -u
OVERLAY_ROOT="${TRANSCRIPT_OVERLAY_ROOT:-%q}"
BIND_ROOT="${TRANSCRIPT_BIND_ROOT:-%q}"
OFFSET_DIR="${TRANSCRIPT_OFFSET_DIR:-%q}"
POLL_SECONDS="${TRANSCRIPT_POLL_SECONDS:-5}"
POLL_LIMIT="${TRANSCRIPT_POLL_LIMIT:-0}"   # 0 = run forever (production); >0 = exit after N polls (tests only)

mkdir -p "$OFFSET_DIR" 2>/dev/null || true

# checkpoint_path maps a session file path to its flat, fixed-length offset
# checkpoint file (the path has slashes; md5 keeps the name flat + collision-free).
# The primary checkpoint holds the last-shipped LINE count (kyber#467 format,
# UNCHANGED); a sibling "<md5>.size" companion holds the byte size at that
# checkpoint (kyber#584 Phase A idle-skip fast path; ADDITIVE — the old script
# ignores it).
checkpoint_path() {
  printf '%%s/%%s' "$OFFSET_DIR" "$(printf '%%s' "$1" | md5sum | cut -d' ' -f1)"
}

# read_int echoes the non-negative integer stored in file $1, or 0 when the file
# is absent, empty, or corrupt. A corrupt/non-numeric checkpoint falls back to 0
# (re-ship from line 1) — duplicates are benign noise; a skipped line is an audit
# hole.
read_int() {
  local v
  if [ -f "$1" ]; then
    v="$(cat "$1" 2>/dev/null || echo 0)"
    case "$v" in (''|*[!0-9]*) v=0 ;; esac
    printf '%%s' "$v"
  else
    printf '0'
  fi
}

# ship_file ships the un-shipped COMPLETE lines of ONE session file, in order,
# exactly once, then advances its durable checkpoint. mawk streams a line at a
# time (bounded buffer; no whole-file slurp, no per-file follower process) so
# peak RSS is O(one line) — independent of how many session files exist, which is
# the kyber#584 invariant. Phase A: a file whose byte size is unchanged since its
# checkpoint is idle (append-only JSONL) and returns immediately after a single
# stat — it is NOT held in any live tail set, and is re-admitted automatically
# the poll its size grows again.
ship_file() {
  local f="$1" ckpt sizeck cur_size last_size shipped total start
  cur_size="$(stat -c %%s "$f" 2>/dev/null || echo 0)"
  case "$cur_size" in (''|*[!0-9]*) cur_size=0 ;; esac
  ckpt="$(checkpoint_path "$f")"
  sizeck="${ckpt}.size"
  last_size="$(read_int "$sizeck")"

  # Phase A fast-skip: unchanged byte size since the last checkpoint => no new
  # content => nothing to ship. One stat, no re-read of the file body — this is
  # what makes a poll over thousands of idle files O(file-count cheap stats)
  # with O(1) memory, rather than re-tailing every historical file.
  if [ "$cur_size" = "$last_size" ]; then
    return 0
  fi

  shipped="$(read_int "$ckpt")"
  total="$(wc -l < "$f" 2>/dev/null || echo 0)"
  case "$total" in (''|*[!0-9]*) total=0 ;; esac

  # Resume after the last-shipped line; default to line 1 when no checkpoint
  # exists (fresh pod / first sight of the file) so a new session ships in full
  # exactly once.
  start=$((shipped + 1))
  # Stale-checkpoint clamp (kyber#467): a DURABLE offset can outlive a file
  # rotation/truncation. If the resume point is past EOF+1, the stored offset
  # claims more lines than the file now has — re-ship from line 1 rather than
  # resuming past EOF and silently skipping the file's new, shorter content.
  if [ "$start" -gt "$((total + 1))" ]; then
    start=1
  fi

  if [ "$start" -le "$total" ]; then
    # Ship complete lines [start, total] and persist the running ABSOLUTE
    # shipped-line count per line (so a crash mid-ship resumes at the last line
    # actually shipped — exactly-once at line granularity, kyber#467). mawk
    # reads the file from line 1; NR is the absolute line number. NR>total exits
    # BEFORE any half-written trailing line (no newline yet) is printed, so a
    # partial line is never shipped or checkpointed. fflush() flushes each line
    # so Vector sees it promptly. Stderr is irrelevant here; the read-only mount
    # cannot be mutated.
    #
    # NO -W interactive: on the runtime image's mawk (Thomas Dickey's mawk
    # 1.3.4) -W interactive truncates record processing once the NR<s{next}
    # resume rule fires — the FIRST ship of a file (start=1, no next rule) works,
    # but every RESUME (start>1) over a file larger than mawk's interactive read
    # buffer ships only a truncated prefix, silently stranding the rest. fflush()
    # alone gives the prompt-flush -W interactive was added for, without the
    # buggy input-buffering behavior.
    mawk -v s="$start" -v e="$total" -v ck="$ckpt" '
      NR < s { next }
      NR > e { exit }
      { print; fflush(); print NR > ck; close(ck) }
    ' "$f" 2>/dev/null
  fi

  # Record the byte size now accounted for so the next poll fast-skips this file
  # until it grows again (Phase A re-admit-on-growth). Written AFTER shipping.
  #
  # CRUCIAL: advance the size checkpoint ONLY once the line checkpoint has caught
  # up to every complete line (mawk shipped through total). If the ship
  # under-ran for ANY reason — a swallowed mawk failure, a transient read error —
  # leave the size checkpoint stale so the NEXT poll re-evaluates and re-attempts
  # the un-shipped lines, rather than fast-skipping (cur_size == last_size) and
  # PERMANENTLY stranding them. Advancing size unconditionally is what turned a
  # single failed resume into a permanent audit hole.
  shipped="$(read_int "$ckpt")"
  if [ "$shipped" -ge "$total" ]; then
    printf '%%s' "$cur_size" > "$sizeck" 2>/dev/null || true
  fi
}

# Boot-tolerance gate (kyber#575): as a native sidecar (restartPolicy:Always) the
# tailer starts AHEAD of the agent container, so on a fresh agent NEITHER projects
# root exists yet — the agent's overlay/bind HOME setup creates them only once it
# boots. Block here, polling, until at least one root appears, BEFORE entering the
# ship loop. This gate must NEVER exit: a startup exit is restarted by the kubelet
# and shows as a climbing restartCount (the boot crash-loop AC7 caught in production)
# until the agent finally writes a transcript. The poll is bounded (POLL_SECONDS)
# and tolerant of the roots never appearing (a genuinely idle agent just waits).
# An already-running/recreated agent (PVC reused, projects dir already present)
# passes the gate immediately with no wait. Logged once so the wait is observable.
boot_wait_logged=0
until [ -d "$OVERLAY_ROOT" ] || [ -d "$BIND_ROOT" ]; do
  if [ "$boot_wait_logged" -eq 0 ]; then
    echo "[transcript-tailer] waiting for a transcript projects dir to appear (agent not booted yet) — normal at startup, not an error" >&2
    boot_wait_logged=1
  fi
  sleep "$POLL_SECONDS"
done

polls=0
while true; do
  for root in "$OVERLAY_ROOT" "$BIND_ROOT"; do
    [ -d "$root" ] || continue
    # Stream the file list one path at a time (NO sort buffer) so memory stays
    # O(1) in file count; ordering across files is irrelevant (every active file
    # is shipped each poll; in-order is a per-file property the checkpoint keeps).
    while IFS= read -r f; do
      [ -n "$f" ] || continue
      ship_file "$f"
    done < <(find "$root" -type f -name '*.jsonl' 2>/dev/null)
  done
  polls=$((polls + 1))
  if [ "$POLL_LIMIT" -gt 0 ] && [ "$polls" -ge "$POLL_LIMIT" ]; then
    break   # test-only: bounded poll count (production POLL_LIMIT=0 never breaks)
  fi
  sleep "$POLL_SECONDS"
done
`, transcriptProjectsOverlayRoot, transcriptProjectsBindRoot, transcriptOffsetsDir)
