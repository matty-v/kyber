// kyber#285: detect kernel OOM kills inside the pod-level cgroup and
// POST a memory_oom event to the control plane when the oom_kill counter
// increments. Closes the gap left by #272 V1: kubelet only tags the
// agent container's State.Terminated.Reason as "OOMKilled" when the
// kernel's OOM-killer victim is the container's PID 1. When PID 1 is a
// shepherd like dbus-run-session and the killer picks a child
// (claude-code), kubelet records reason="Error" instead and the OOM is
// invisible at the K8s API level. The cgroup counter is kernel-
// authoritative and does not depend on which process was the victim.
//
// Path resolution (verified empirically on k3s + WSL2 + GCP, 2026-05-09):
//
// On clusters WITHOUT cgroup namespacing (the kubelet/containerd default
// in the environments Kyber currently targets), each container sees the
// full host cgroup hierarchy at /sys/fs/cgroup/. /proc/self/cgroup
// reports the container's absolute cgroup path:
//
//	/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<UID>.slice/cri-containerd-<container>.scope
//
// Strip the trailing cri-containerd scope to get the **pod-level** cgroup,
// which has its own memory.* files (the kubelet creates this layer for
// per-pod resource accounting). Read memory.events at that path — NOT
// memory.events.local. The .local variant counts events in THIS cgroup
// only; an OOM kill inside a per-container child cgroup bumps the child's
// .local but only the parent's recursive memory.events. We want kills
// attributed to ANY container in the pod, so memory.events is the right
// file. See cgroup-v2 docs §"Memory Events".
//
// The ConfigMap-style override (KYBER_CGROUP_MEMORY_EVENTS_PATH) bypasses
// the auto-derivation entirely and is intended for tests + clusters where
// path discovery doesn't apply (cgroup ns enabled, exotic CRI, etc.).
//
// File format (subset; see cgroup-v2 docs for the full schema):
//
//	low 0
//	high 0
//	max 0
//	oom 0
//	oom_kill 1
//	oom_group_kill 0
//
// On startup the sidecar snapshots the counter as a baseline so existing
// kills (from a previous container life within the same pod) don't get
// re-attributed to this sidecar's session. Subsequent ticks compare to
// the last-seen value; on increment, we POST one memory_oom event with
// the current counter value and update the last-seen.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// readOOMKillCount parses a cgroup memory.events / memory.events.local
// file at path and returns the oom_kill counter. The file format is one
// "<key> <value>" per line; we look for the "oom_kill" key. Returns an
// error if the file can't be read or the key is absent (a missing key
// means this kernel/cgroup version doesn't surface oom_kill — caller
// should treat that as "OOM detection unavailable" not as zero kills).
func readOOMKillCount(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if key != "oom_kill" {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse oom_kill %q: %w", val, err)
		}
		return n, nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	return 0, fmt.Errorf("oom_kill key not found in %s", path)
}

// parsePodCgroupPath takes the body of /proc/self/cgroup (cgroup-v2
// format: "0::<absolute-path>") and returns the POD-level cgroup path
// by stripping the trailing kubelet container-scope suffix. The pod
// path is what the kubelet creates per-pod for resource accounting and
// is where memory.events aggregates kills across all containers.
//
// Examples (containerd, default kubelet):
//
//	0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<UID>.slice/cri-containerd-<id>.scope
//	→ /kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<UID>.slice
//
//	0::/kubepods/pod<UID>/<container-id>      (cgroupfs driver)
//	→ /kubepods/pod<UID>
//
// With cgroup namespaces, "/" is already the pod-level cgroup exposed at
// cgroupRoot, so it is returned directly. Other inputs that do not look like
// kubelet-managed cgroup-v2 paths return an error.
func parsePodCgroupPath(procCgroupBody string) (string, error) {
	// /proc/self/cgroup may have multiple lines for cgroup-v1; the v2
	// line is the one starting with "0::". Single-line v2 systems also
	// land here.
	for _, line := range strings.Split(strings.TrimSpace(procCgroupBody), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		full := strings.TrimPrefix(line, "0::")
		if full == "/" {
			// Kubernetes commonly enables cgroup namespaces for pods. In that
			// shape /sys/fs/cgroup is already rooted at the pod cgroup and its
			// memory.events/cpu.stat files aggregate the container children.
			return full, nil
		}
		if full == "" {
			return "", fmt.Errorf("self cgroup path is empty")
		}
		// The kubelet's per-pod cgroup is the parent of the container scope.
		// Strip exactly one path segment.
		pod := filepath.Dir(full)
		if pod == "" || pod == "/" {
			return "", fmt.Errorf("self cgroup %q has no parent — pod path cannot be derived", full)
		}
		return pod, nil
	}
	return "", fmt.Errorf("no cgroup-v2 (\"0::\") line found in /proc/self/cgroup")
}

// resolvePodCgroupEventsPath builds the absolute filesystem path to the
// pod-level memory.events file. cgroupRoot is typically /sys/fs/cgroup;
// procCgroupPath is typically /proc/self/cgroup. Returns the joined path
// when derivation succeeds; otherwise an error the caller should log
// once and treat as "OOM detection unavailable".
//
// memory.events (NOT memory.events.local) is the right file because the
// kubelet creates a child cgroup PER container under the pod cgroup, and
// kills inside a container cgroup bump that child's events.local — they
// only show up in the parent's recursive memory.events. See file-level
// doc above for the empirical confirmation.
func resolvePodCgroupEventsPath(procCgroupPath, cgroupRoot string) (string, error) {
	body, err := os.ReadFile(procCgroupPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", procCgroupPath, err)
	}
	pod, err := parsePodCgroupPath(string(body))
	if err != nil {
		return "", err
	}
	return filepath.Join(cgroupRoot, pod, "memory.events"), nil
}

// oomDetector tracks the pod-level cgroup oom_kill counter across
// heartbeat ticks and emits a memory_oom event when it increments.
// Designed to be cheap: one file read per tick (heartbeatInterval). All
// state is in-process — the control plane is the durable record of OOM
// observations.
//
// An empty path means "OOM detection disabled" — tick is a no-op. This
// is the graceful-degradation mode for clusters / setups where the path
// can't be derived (cgroup ns enabled, cgroup-v1, etc.).
type oomDetector struct {
	path          string
	lastCount     uint64
	baselineSet   bool
	readErrLogged atomic.Bool // log a missing/unreadable cgroup file once, not every tick
	postErrLogged atomic.Bool // same for transient post failures
}

func newOOMDetector(path string) *oomDetector {
	return &oomDetector{path: path}
}

// tick reads the current oom_kill counter. On the first tick it
// establishes a baseline and emits nothing. On subsequent ticks, if the
// counter is higher than last seen, it POSTs a memory_oom event and
// returns true. Returns false otherwise. Read/post errors are logged
// at-most-once and swallowed — never crash the sidecar on this path.
//
// An empty d.path (detection disabled) returns false immediately.
func (d *oomDetector) tick(
	ctx context.Context,
	client *http.Client,
	cfg config,
	now time.Time,
	logf func(string, ...any),
	warnf func(string, ...any),
) bool {
	if d.path == "" {
		return false
	}
	current, err := readOOMKillCount(d.path)
	if err != nil {
		if d.readErrLogged.CompareAndSwap(false, true) {
			warnf("OOM cgroup read failed; OOM detection disabled until restart: err=%v path=%s", err, d.path)
		}
		return false
	}
	// First successful read: snapshot as baseline, do NOT emit. A pod
	// that already OOM'd before the sidecar started would otherwise post
	// a spurious event attributing an old kill to a fresh sidecar.
	if !d.baselineSet {
		d.lastCount = current
		d.baselineSet = true
		logf("OOM detector baseline established: oom_kill=%d path=%s", current, d.path)
		return false
	}
	if current <= d.lastCount {
		return false
	}
	// Counter increased — kernel OOM kill happened in the pod's cgroup
	// since the last tick. Post the event and bump the last-seen so the
	// next tick doesn't re-emit.
	if err := postMemoryOOM(ctx, client, cfg, now, current); err != nil {
		if d.postErrLogged.CompareAndSwap(false, true) {
			warnf("memory_oom post failed; will retry on next increment: err=%v", err)
		}
		// Don't bump lastCount on post failure — we want the next tick
		// to retry. (Re-using readErrLogged-style at-most-once for the
		// noise is the right call but allow re-tries on the underlying
		// state.)
		return false
	}
	logf("memory_oom posted: oom_kill went %d → %d", d.lastCount, current)
	d.lastCount = current
	d.postErrLogged.Store(false) // reset so a future failure is logged again
	return true
}

// postMemoryOOM sends one memory_oom event to the control plane via the
// shared postToCP helper. Body shape mirrors heartbeat / activity
// events; the control plane's status-event handler stamps
// Status.LastKernelOOMKillAt from the `at` timestamp.
func postMemoryOOM(ctx context.Context, client *http.Client, cfg config, now time.Time, oomKillCount uint64) error {
	body, err := json.Marshal(map[string]any{
		"type":         "memory_oom",
		"at":           now.UTC().Format(time.RFC3339),
		"oomKillCount": oomKillCount,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if _, err := postToCP(ctx, client, cfg, "status-event", body); err != nil {
		return err
	}
	return nil
}
