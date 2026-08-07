// Transcript-pruner sidecar injection for agent pods (kyber#471). Adds the
// transcript-pruner container to the pod spec built by BuildPodSpec. The sidecar
// bounds the unbounded growth of the agent's Claude Code session JSONL
// (~/.claude/projects/<project>/*.jsonl) on the persist PVC by periodically
// removing already-archived session files past a configurable age (and optional
// per-agent size) policy.
//
// Why a sidecar (not a cluster CronJob/Job): the agent "persist" PVC is
// ReadWriteOnce (pod_builder.go) and is only reliably mountable from the pod
// that already holds it on its node. A separately-scheduled job pod has no
// guarantee of co-scheduling onto that node. On-PVC pruning must therefore run
// where the PVC already is — inside the agent pod, as its own container — the
// same topology reasoning that made the transcript-TAILER a sidecar.
//
// Why a SEPARATE container from the tailer (kyber#446 invariant): the tailer
// must keep its READ-ONLY mount of the PVC (it ships, it must not mutate). The
// pruner needs WRITE access to delete files. Keeping them as distinct containers
// lets the tailer stay read-only (unchanged) while the pruner gets its own RW
// mount. The PVC access mode is unchanged (still ReadWriteOnce) — the agent
// container already mounts it RW, so a second RW mount introduces no
// StorageClass/access-mode change.
//
// Safety: the durable archive copy (transcripts/<agent>/<date>/ in object
// storage) is the authoritative record and is structurally out of the pruner's
// reach — the default path carries NO object-store credentials and the pruner
// only ever deletes LOCAL files. The age threshold (default 7d, >> the
// minutes-scale tailer→Vector ship lag) is the primary archived-safety proxy;
// the active (most-recently-modified) session file is exempt unconditionally.

package agent

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// TranscriptPrunerContainerName is the container name in the pod spec. Kept
	// stable (same rationale as the tailer/status sidecar) so kubectl/PWA tooling
	// can target it predictably.
	TranscriptPrunerContainerName = "transcript-pruner"

	// transcriptPrunerOffsetsMountPath is where the pruner mounts the tailer's
	// pod-lifetime offsets emptyDir READ-ONLY, used only on the optional
	// archive-cross-check path to confirm a file was fully shipped before delete.
	// Distinct from the tailer's own offsets mount path so the two never collide.
	transcriptPrunerOffsetsMountPath = "/var/run/kyber-transcript-offsets-ro"
)

// TranscriptPrunerConfig carries the fields the controller threads through to
// the sidecar. Like the tailer, RuntimeImage is REQUIRED and must be the agent's
// own runtime image so the sidecar runs as the same uid that owns the JSONL
// files — required to delete the root-owned files over the shared mount. Enabled
// gates injection so the whole feature is a per-cluster Helm toggle.
type TranscriptPrunerConfig struct {
	AgentName    string
	RuntimeImage string
	Runtime      string
	// Enabled gates injection. When false the sidecar is not appended (the
	// feature is off for this cluster). Mirrors the tailer's image guard with an
	// explicit on/off so a misconfigured retention block fails closed (no prune).
	Enabled bool
	// MaxAgeDays is the age threshold: a *.jsonl whose mtime is older than this
	// many days is considered durably archived (>> ship lag) and prune-eligible.
	// Must be > 0 for any pruning to occur (a zero/negative value fails closed).
	MaxAgeDays int
	// MaxBytesPerAgent, when > 0, is an optional secondary per-agent size ceiling.
	// When set, age-eligible files are pruned oldest-first only as far as needed
	// to bring the total on-PVC transcript bytes at or under this ceiling (so some
	// archived files may be retained if already under budget). When 0/empty the
	// policy is age-only (every age-eligible file is pruned). It NEVER prunes the
	// active file or any file inside the age-safety window.
	MaxBytesPerAgent int64
	// PruneIntervalMinutes is the sidecar poll cadence. Defaults to 60 when <= 0.
	PruneIntervalMinutes int
	// ArchiveCrosscheck, when true, adds a belt-and-suspenders gate: a file is only
	// prune-eligible if the tailer's local ship checkpoint confirms it was fully
	// shipped (line count shipped >= current line count), in addition to the age
	// threshold. Off by default (the age threshold alone is archive-safe). This
	// uses the tailer's local checkpoint (proof the line reached Vector→archive),
	// not an object-store LIST, so it needs no credentials.
	ArchiveCrosscheck bool
}

// AppendTranscriptPruner appends the transcript-pruner container to pod's
// Containers slice. No-op when the feature is disabled, the runtime image is
// unset (dev installs / tests), or the age policy is non-positive — all of which
// fail closed (no destructive sidecar injected) so a half-configured retention
// block can never prune.
func AppendTranscriptPruner(spec *corev1.PodSpec, cfg TranscriptPrunerConfig) {
	if !cfg.Enabled || cfg.RuntimeImage == "" || cfg.MaxAgeDays <= 0 {
		return
	}

	interval := cfg.PruneIntervalMinutes
	if interval <= 0 {
		interval = 60
	}

	crosscheck := "false"
	if cfg.ArchiveCrosscheck {
		crosscheck = "true"
	}

	volumeMounts := []corev1.VolumeMount{
		{
			// The pruner's OWN read-WRITE mount of the agent persist PVC. This is
			// the only difference from the tailer's mount (which stays ReadOnly:
			// true, untouched — kyber#446). The PVC access mode is unchanged
			// (ReadWriteOnce); the agent container already mounts it RW.
			Name:      "persist",
			MountPath: transcriptMountPath,
			// ReadOnly omitted (defaults false) — RW, scoped by the prune script
			// to *.jsonl under the two projects roots only.
		},
	}
	// On the optional cross-check path, mount the tailer's offsets emptyDir
	// READ-ONLY so the pruner can confirm a file was fully shipped before delete.
	// Skipped entirely on the default path so the common case adds no coupling.
	if cfg.ArchiveCrosscheck {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      transcriptOffsetsVolumeName,
			MountPath: transcriptPrunerOffsetsMountPath,
			ReadOnly:  true,
		})
	}

	offsetDir := transcriptPrunerOffsetsMountPath

	container := corev1.Container{
		Name:    TranscriptPrunerContainerName,
		Image:   cfg.RuntimeImage,
		Command: []string{"/bin/bash", "-c", transcriptPruneLoopScript},
		Env: []corev1.EnvVar{
			{Name: "AGENT_NAME", Value: cfg.AgentName},
			{Name: "PRUNE_OVERLAY_ROOT", Value: transcriptRoots(cfg.Runtime, transcriptProjectsOverlayRoot, transcriptCodexOverlayRoot)},
			{Name: "PRUNE_BIND_ROOT", Value: transcriptRoots(cfg.Runtime, transcriptProjectsBindRoot, transcriptCodexBindRoot)},
			{Name: "PRUNE_MAX_AGE_DAYS", Value: fmt.Sprintf("%d", cfg.MaxAgeDays)},
			{Name: "PRUNE_MAX_BYTES", Value: fmt.Sprintf("%d", cfg.MaxBytesPerAgent)},
			{Name: "PRUNE_INTERVAL_SECONDS", Value: fmt.Sprintf("%d", interval*60)},
			{Name: "PRUNE_OFFSET_DIR", Value: offsetDir},
			{Name: "PRUNE_ARCHIVE_CROSSCHECK", Value: crosscheck},
		},
		Resources: corev1.ResourceRequirements{
			// Cheaper than the tailer: no long-lived tail -F fan-out or multi-MB
			// line buffering — it sleeps and wakes for one find+stat+rm pass.
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
		VolumeMounts: volumeMounts,
		// Identical locked-down posture to the tailer (transcript_tailer.go): runs
		// as root (uid 0) because the session JSONL is root-owned over the shared
		// mount, but NOT privileged, no added capabilities, no privilege
		// escalation, read-only root filesystem. Strictly more locked down than
		// the agent container beside it; its only write authority is bounded by
		// the prune script to *.jsonl under the two projects roots.
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptrTo(int64(0)),
			ReadOnlyRootFilesystem:   ptrTo(true),
			AllowPrivilegeEscalation: ptrTo(false),
		},
		// No liveness probe — a prune loop exposes no health endpoint, same as the
		// tailer.
		//
		// Native sidecar (kyber#575): RestartPolicy:Always so the kubelet restarts
		// the pruner on any exit independently of the pod-level RestartPolicy:Never,
		// rather than leaving it permanently dead. The prune loop is a sleep/wake
		// find+stat+rm pass that no-ops when the projects roots are absent, so
		// starting before transcript files exist is safe.
		RestartPolicy: ptrTo(corev1.ContainerRestartPolicyAlways),
	}
	spec.InitContainers = append(spec.InitContainers, container)
}

// transcriptPruneOnceScript performs ONE prune pass and exits. It is fully
// parameterized by environment variables (no baked-in paths) so it is directly
// executable in unit tests against a temp directory:
//
//	PRUNE_OVERLAY_ROOT / PRUNE_BIND_ROOT  the two projects roots to scan
//	PRUNE_MAX_AGE_DAYS                    age threshold in days (archived proxy)
//	PRUNE_MAX_BYTES                       optional size ceiling (0 = age-only)
//	PRUNE_OFFSET_DIR                      tailer offsets dir (cross-check only)
//	PRUNE_ARCHIVE_CROSSCHECK              "true" to require fully-shipped checkpoint
//
// Decision per *.jsonl file under the roots:
//   - the single newest file across all roots (the active session) is NEVER pruned
//   - a file is age-eligible iff (now - mtime) >= MAX_AGE_DAYS days
//   - on the cross-check path, a file is additionally required to be fully shipped
//     (checkpoint line count >= current line count) — an old-but-unshipped file is
//     retained (the "un-archived → retained" case)
//   - with no size ceiling: every eligible file is deleted
//   - with a size ceiling: eligible files are deleted oldest-first only until the
//     total on-PVC transcript bytes are at or under the ceiling
const transcriptPruneOnceScript = `set -u
ROOTS=("${PRUNE_OVERLAY_ROOT:-}" "${PRUNE_BIND_ROOT:-}")
MAX_AGE_DAYS="${PRUNE_MAX_AGE_DAYS:-0}"
MAX_BYTES="${PRUNE_MAX_BYTES:-0}"
OFFSET_DIR="${PRUNE_OFFSET_DIR:-}"
CROSSCHECK="${PRUNE_ARCHIVE_CROSSCHECK:-false}"

# Fail closed: a non-positive age policy prunes nothing.
case "$MAX_AGE_DAYS" in (''|*[!0-9]*) exit 0 ;; esac
[ "$MAX_AGE_DAYS" -gt 0 ] || exit 0

now="$(date +%s)"
age_cutoff=$(( now - MAX_AGE_DAYS * 86400 ))

# Gather every *.jsonl under the roots as "<mtime> <size> <path>" lines.
all="$(
  for root in "${ROOTS[@]}"; do
    [ -n "$root" ] && [ -d "$root" ] || continue
    find "$root" -type f -name '*.jsonl' -printf '%T@ %s %p\n' 2>/dev/null
  done
)"
[ -n "$all" ] || exit 0

# The active session = the single newest file across all roots; never pruned.
active="$(printf '%s\n' "$all" | sort -rn | head -n1 | sed 's/^[^ ]* [^ ]* //')"

# total_bytes = sum of ALL transcript bytes (the figure a size ceiling bounds).
total_bytes="$(printf '%s\n' "$all" | awk '{s += $2} END {print s+0}')"

fully_shipped() {
  # Cross-check: a file is confirmed archived iff the tailer's checkpoint records
  # at least as many shipped lines as the file currently has. Missing/short
  # checkpoint => not confirmed => retain.
  local f="$1" ckpt shipped have
  [ -n "$OFFSET_DIR" ] || return 1
  ckpt="$OFFSET_DIR/$(printf '%s' "$f" | md5sum | cut -d' ' -f1)"
  [ -f "$ckpt" ] || return 1
  shipped="$(cat "$ckpt" 2>/dev/null || echo 0)"
  case "$shipped" in (''|*[!0-9]*) return 1 ;; esac
  have="$(wc -l < "$f" 2>/dev/null || echo 0)"
  [ "$shipped" -ge "$have" ]
}

# Build the eligible set (oldest-first), excluding the active file, applying the
# age threshold and the optional cross-check.
eligible="$(printf '%s\n' "$all" | sort -n | while IFS= read -r line; do
  [ -n "$line" ] || continue
  mtime="${line%% *}"; mtime="${mtime%.*}"
  rest="${line#* }"; size="${rest%% *}"; path="${rest#* }"
  [ "$path" = "$active" ] && continue
  [ "$mtime" -le "$age_cutoff" ] || continue          # too recent => retain
  if [ "$CROSSCHECK" = "true" ]; then
    fully_shipped "$path" || continue                 # not confirmed shipped => retain
  fi
  printf '%s\t%s\n' "$size" "$path"
done)"
[ -n "$eligible" ] || exit 0

if [ "$MAX_BYTES" -gt 0 ] 2>/dev/null; then
  # Size ceiling: delete oldest-first only until total is at/under the ceiling.
  printf '%s\n' "$eligible" | while IFS=$'\t' read -r size path; do
    [ -n "$path" ] || continue
    [ "$total_bytes" -le "$MAX_BYTES" ] && break
    rm -f -- "$path" 2>/dev/null && total_bytes=$(( total_bytes - size ))
  done
else
  # Age-only: delete every eligible file.
  printf '%s\n' "$eligible" | while IFS=$'\t' read -r size path; do
    [ -n "$path" ] || continue
    rm -f -- "$path" 2>/dev/null || true
  done
fi
exit 0
`

// transcriptPruneLoopScript wraps the single-pass script in the sidecar poll
// loop: prune once immediately, then every PRUNE_INTERVAL_SECONDS. Mirrors the
// tailer's in-sidecar loop (no CronJob — the RWO PVC topology rules that out).
// The once-pass runs in a SUBSHELL so its terminal `exit 0` (and any `set`
// state) is scoped to that pass and never tears down the loop. A prune failure
// is non-fatal — the next tick retries.
var transcriptPruneLoopScript = fmt.Sprintf(`set -u
INTERVAL="${PRUNE_INTERVAL_SECONDS:-3600}"
while true; do
(
%s
) || true
  sleep "$INTERVAL"
done
`, transcriptPruneOnceScript)
