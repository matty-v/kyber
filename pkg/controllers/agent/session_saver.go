// Session-saver sidecar injection for agent pods (session-continuity).
//
// Companion to the transcript-tailer: where the tailer ships the RAW session
// JSONL to the durable object-store audit lane (read-only, kyber#446), the
// session-saver maintains a small, continuously-updated RECALL snapshot —
// "what did the previous session last do, and the last few turns" — at
// /persist/session-state.json (= ClaudeCodeAdapter.SessionStatePath()).
//
// Why this exists: a recreated pod (reboot, crash, OOM, operator restart) starts
// a fresh session with no memory of the prior one. The controller CANNOT read the
// RWO /persist PVC off-node, so it cannot build the recall content cross-node
// (BuildBrief carries only restart metadata). But /persist is DURABLE across pod
// recreation, so the recall content is delivered entirely in-pod: this sidecar
// writes session-state.json every poll, it survives the recreate on the PVC, and
// the next pod's start-claude.sh reads it on boot BEFORE claude launches.
//
// Why a separate sidecar (not folded into the transcript-tailer): the tailer
// mounts /persist READ-ONLY (kyber#446) so it cannot write the snapshot, and its
// single-process O(1)-memory ship loop (kyber#584) is deliberately not allowed to
// buffer "the last N exchanges." A separate container mounts /persist READ-WRITE
// (like the transcript-pruner already does) and leaves the tailer's invariants
// untouched. session-state.json is the HOT recall snapshot (agent-scoped, fine to
// be agent-adjacent); it is NOT the immutable audit record — that stays the
// tailer -> Vector -> object-store lane.
//
// Harness-neutral: the jq program below normalizes both Claude Code and Codex
// transcript records into the same recall shape. The sidecar injection,
// durable-/persist delivery, and boot-read contract are shared by both runtimes.

package agent

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// SessionSaverContainerName is the container name in the pod spec. Kept
	// stable (same rationale as the other sidecars) so Vector routing and
	// kubectl/PWA tooling can target it predictably.
	SessionSaverContainerName = "session-saver"

	// saverMountPath is where the agent "persist" PVC is mounted READ-WRITE
	// inside the sidecar. Unlike the transcript-tailer (which mounts the PVC
	// read-only at /agent-home), the saver must WRITE session-state.json to the
	// durable PVC, so it mounts at /persist — the same path the agent container
	// uses — and writes the snapshot at saverStateFile below. The transcript
	// JSONL is read from the same two mode-dependent roots as the tailer, here
	// re-rooted under /persist.
	saverMountPath = "/persist"

	// saverProjectsOverlayRoot / saverProjectsBindRoot mirror the tailer's two
	// physical locations of ~/.claude/projects on the PVC (overlay-upper vs
	// bind-HOME), re-rooted from the tailer's /agent-home to the saver's
	// /persist. See transcript_tailer.go for the full explanation of the two
	// persistence modes.
	saverProjectsOverlayRoot = saverMountPath + "/overlay/upper/home/kyber/.claude/projects"
	saverProjectsBindRoot    = saverMountPath + "/home/.claude/projects"

	// saverStateFile is the durable recall snapshot the saver writes and
	// start-claude.sh reads on boot. It matches ClaudeCodeAdapter.SessionStatePath()
	// (/persist/session-state.json) so the boot read and this writer agree on one
	// path — the contract between them.
	saverStateFile = saverMountPath + "/session-state.json"

	// saverDefaultTurns is how many of the most recent user/assistant text
	// exchanges the snapshot keeps. Enough for boot recall without bloating the
	// file; tunable via SAVER_TURNS for tests.
	saverDefaultTurns = 12
)

// SessionSaverConfig carries the fields the controller threads through to the
// sidecar. RuntimeImage is REQUIRED and gates injection (same guard as the
// transcript-tailer): it must be the agent's own runtime image so the saver runs
// as the same uid that owns the root-owned JSONL and can read it over the mount.
// AgentName is surfaced as AGENT_NAME (stamped into the snapshot and for
// debugging parity with the other sidecars).
type SessionSaverConfig struct {
	AgentName    string
	RuntimeImage string
	Runtime      string
}

// AppendSessionSaver appends the session-saver container to the pod's
// InitContainers slice as a native sidecar. No-op when cfg.RuntimeImage is empty
// (dev installs / tests), mirroring AppendTranscriptTailer's image guard so older
// deployments and unit tests keep their existing pod-spec shape.
//
// It adds NO new volume: it reuses the existing "persist" PVC volume (added by
// pod_builder) with a READ-WRITE mount, so no ensure*PVC call is needed — the
// snapshot lands on the same durable PVC that survives pod recreation.
func AppendSessionSaver(spec *corev1.PodSpec, cfg SessionSaverConfig) {
	if cfg.RuntimeImage == "" {
		return
	}

	container := corev1.Container{
		Name:    SessionSaverContainerName,
		Image:   cfg.RuntimeImage,
		Command: []string{"/bin/bash", "-c", sessionSaverScript},
		Env: []corev1.EnvVar{
			{Name: "AGENT_NAME", Value: cfg.AgentName},
			{Name: "SAVER_OVERLAY_ROOT", Value: transcriptRoots(cfg.Runtime, saverProjectsOverlayRoot, "/persist/overlay/upper/home/kyber/.codex/sessions")},
			{Name: "SAVER_BIND_ROOT", Value: transcriptRoots(cfg.Runtime, saverProjectsBindRoot, "/persist/home/.codex/sessions")},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("250m"),
				// Only the single newest session file is parsed per poll (jq over
				// one bounded, pruner-capped day of JSONL), so peak memory is small
				// and independent of the agent's historical file count. 128Mi is
				// ample headroom.
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
		// READ-WRITE persist mount: unlike the tailer's read-only mount, the saver
		// writes session-state.json to the durable PVC. It writes ONLY that single
		// file (atomic tmp+rename) and never touches the agent's transcript or
		// state, so the kyber#446 posture is preserved in spirit — the writable
		// surface is one controller-owned recall file, not the agent tree. (The
		// transcript-pruner already mounts persist read-write, so a writable-persist
		// sidecar is established precedent.)
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "persist",
				MountPath: saverMountPath,
			},
		},
		// Run as root (uid 0), same rationale as the transcript-tailer: the reused
		// runtime image defaults to a root USER and the session JSONL is root-owned,
		// so the saver must be root to read it. Posture stays tight: NOT privileged,
		// no added capabilities, no privilege escalation, read-only root filesystem
		// (the mounted /persist volume stays writable — RO rootfs only covers image
		// layers). Asserting RunAsNonRoot without RunAsUser bricks admission, so we
		// pin RunAsUser:0 explicitly (kyber#451 lesson from the tailer).
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptrTo(int64(0)),
			ReadOnlyRootFilesystem:   ptrTo(true),
			AllowPrivilegeEscalation: ptrTo(false),
		},
		// Native sidecar (kyber#575): RestartPolicy:Always so the kubelet restarts
		// it on any exit and teardown runs AFTER the agent container exits — giving
		// a final poll to capture the last turns of a planned shutdown. The script
		// (set -u, no set -e) tolerates starting before the agent's transcript files
		// exist: each poll finds no file and simply retries, so a missing projects
		// root is a wait, not a crash-loop.
		RestartPolicy: ptrTo(corev1.ContainerRestartPolicyAlways),
	}
	spec.InitContainers = append(spec.InitContainers, container)
}

// sessionSaverScript is the sidecar's poll loop. Every poll it finds the single
// NEWEST session JSONL across the two PVC roots, extracts the last N user/assistant
// text exchanges plus a "last activity" line via one jq program, and writes the
// recall snapshot to session-state.json atomically (tmp+rename), skipping the
// write when the content is unchanged so it doesn't churn the PVC.
//
// Only the newest file is parsed (recall is about the current/most-recent
// session), and only its last SAVER_MAX_BYTES are read (via tail -c) — so peak
// memory/CPU is bounded by that tail, NOT by the active session's total size,
// which can grow to tens of MB on a long-lived agent. jq's per-line tolerant
// decode (fromjson? // empty) skips both the half-written trailing line and the
// partial leading line the byte cut may produce, so a snapshot is never taken
// from a torn record.
//
// The roots, output path, turn count, and poll cadence default to the documented
// constants but are env-overridable (SAVER_OVERLAY_ROOT / SAVER_BIND_ROOT /
// SAVER_OUT / SAVER_TURNS / SAVER_POLL_SECONDS) so the loop can be exercised
// against a fixture in tests; SAVER_POLL_LIMIT (0 = run forever, the production
// default) bounds the poll count for the same reason. None are set on the
// production container, so its behavior is unchanged.
var sessionSaverScript = fmt.Sprintf(`set -u
OVERLAY_ROOT="${SAVER_OVERLAY_ROOT:-%q}"
BIND_ROOT="${SAVER_BIND_ROOT:-%q}"
OUT="${SAVER_OUT:-%q}"
TURNS="${SAVER_TURNS:-%d}"
MAX_BYTES="${SAVER_MAX_BYTES:-2000000}"   # only parse the last N bytes of the newest file (bounds memory/CPU vs. an unbounded active session)
POLL_SECONDS="${SAVER_POLL_SECONDS:-5}"
POLL_LIMIT="${SAVER_POLL_LIMIT:-0}"   # 0 = run forever (production); >0 = exit after N polls (tests only)

# newest_transcript prints the path of the most-recently-modified *.jsonl across
# both roots, or nothing when neither root has one yet (fresh pod: the agent
# hasn't written a transcript). Uses find -printf mtime so it is a single stream,
# no per-file processes.
newest_transcript() {
  { for root in "$OVERLAY_ROOT" "$BIND_ROOT"; do
      [ -d "$root" ] || continue
      find "$root" -type f -name '*.jsonl' -printf '%%T@ %%p\n' 2>/dev/null
    done; } | sort -n | tail -1 | cut -d' ' -f2-
}

# The jq program: main-thread only (drop isSidechain), user/assistant only, text
# blocks only (a string content, or the joined .text of an array content — tool_use
# / tool_result / thinking blocks have no .text and are dropped), non-empty after
# trim. Emit briefstore-compatible field names (recent_exchanges / last_activity /
# role / content / timestamp) so the snapshot can also feed BuildBrief later.
JQ_PROG='
  def claude_exchange:
    select((.isSidechain // false) | not)
    | select(.type == "user" or .type == "assistant")
    | { role: .type,
        timestamp: (.timestamp // ""),
        content: ( .message.content as $c
                   | if   ($c | type) == "string" then $c
                     elif ($c | type) == "array"  then ([ $c[] | select(.type == "text") | .text ] | join("\n"))
                     else "" end ) };
  def codex_exchange:
    select(.type == "event_msg")
    | select(.payload.type == "user_message" or .payload.type == "agent_message")
    | { role: (if .payload.type == "user_message" then "user" else "assistant" end),
        timestamp: (.timestamp // ""),
        content: ((.payload.message // "") | tostring) };
  [ inputs
    | (fromjson? // empty)
    | (claude_exchange // codex_exchange)
    | select( (.content | gsub("^\\s+|\\s+$"; "")) != "" )
  ] as $all
  | ( if ($all | length) > $n then $all[-$n:] else $all end ) as $recent
  | ( $all | map(select(.role == "assistant")) | last ) as $la
  | { version: 1,
      agent_name: $agent,
      updated_at: ( $recent | last | (.timestamp // "") ),
      last_activity: ( ($la.content // "") | split("\n")[0] // "" ),
      recent_exchanges: $recent }
'

write_state() {
  # Fixed temp name (not mktemp): a kill between write and rename leaves at most
  # ONE stale "${OUT}.tmp", overwritten next poll — no unbounded orphan accrual on
  # the durable PVC. Only the last MAX_BYTES of the newest file are parsed so peak
  # memory/CPU is bounded regardless of the active session's size.
  local f="$1" tmp="${OUT}.tmp"
  if tail -c "$MAX_BYTES" "$f" 2>/dev/null | jq -nRc --arg agent "${AGENT_NAME:-}" --argjson n "$TURNS" "$JQ_PROG" > "$tmp" 2>/dev/null; then
    # Write only on change so the PVC and any boot read don't churn. Atomic
    # rename means a concurrent boot read never sees a torn file.
    if [ ! -f "$OUT" ] || ! cmp -s "$tmp" "$OUT"; then
      mv -f "$tmp" "$OUT" 2>/dev/null || rm -f "$tmp"
    else
      rm -f "$tmp"
    fi
  else
    rm -f "$tmp"
  fi
}

polls=0
while true; do
  f="$(newest_transcript)"
  if [ -n "$f" ]; then
    write_state "$f"
  fi
  polls=$((polls + 1))
  if [ "$POLL_LIMIT" -gt 0 ] && [ "$polls" -ge "$POLL_LIMIT" ]; then
    break   # test-only: bounded poll count (production POLL_LIMIT=0 never breaks)
  fi
  sleep "$POLL_SECONDS"
done
`, saverProjectsOverlayRoot, saverProjectsBindRoot, saverStateFile, saverDefaultTurns)
