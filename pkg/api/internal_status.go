// Status-event endpoint for the kyber-status-sidecar (kyber#248). Each
// heartbeat / activity event is a self-contained POST. The sidecar
// opens a new request per event; HTTP keepalive on the underlying
// transport amortises the TCP handshake.
//
// The earlier design of this endpoint was a long-lived streaming POST,
// but client.Do blocks waiting for response headers, and the server
// can't flush headers AND keep reading the body without hitting net/http
// duplex constraints under httptest. Per-event POST sidesteps the whole
// problem and has acceptable wire overhead at the 5s heartbeat cadence.
// Phase B's sub-second activity events still ride on the same shape;
// HTTP/1.1 keepalive keeps it cheap.
//
// Direction: sidecar → control plane. The /internal/ subtree's network
// isolation (cluster-internal :8082 only) covers identity for now;
// optional Bearer header on the request will become mandatory when
// internal auth lands.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/diskreserve"
)

// statusEvent is one event the sidecar pushes. Type is the event kind
// ("heartbeat" today; "activity" lands with kyber#249). Unknown types
// are accepted and silently dropped so older control planes can
// tolerate newer sidecars.
type statusEvent struct {
	Type string `json:"type"`
	// At is the sidecar's wall-clock at the time the event was generated.
	// RFC3339. Heartbeat events use this to update LastHeartbeatAt; future
	// activity events will use it for LastActivityAt.
	At string `json:"at"`
	// State applies to "activity" events only (kyber#249).
	State     string                      `json:"state,omitempty"`
	Resources *kyberv1.AgentResourceUsage `json:"resources,omitempty"`
}

// handleStatusEvent handles POST /internal/agents/{name}/status-event.
// Body is one JSON statusEvent. Each heartbeat / activity tick is its
// own request. Returns 204 on a successful patch (no body), 400 on
// malformed JSON, 404 when the agent is not found, 503 when the server
// has no Kubernetes client wired in.
func (s *InternalServer) handleStatusEvent(w http.ResponseWriter, r *http.Request, agentName string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.k8sClient == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}

	var ev statusEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	if err := s.applyStatusEvent(r.Context(), agentName, ev); err != nil {
		// applyStatusEvent returns a "not found" error if the agent's
		// gone — surface as 404 so the sidecar can back off.
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyStatusEvent patches the agent's status.activity based on a single
// event. Heartbeat events update LastHeartbeatAt only; activity events
// (kyber#249) will additionally update State + LastActivityAt.
func (s *InternalServer) applyStatusEvent(ctx context.Context, agentName string, ev statusEvent) error {
	var agent kyberv1.Agent
	if err := s.k8sClient.Get(ctx, types.NamespacedName{
		Namespace: s.namespace,
		Name:      agentName,
	}, &agent); err != nil {
		return fmt.Errorf("get agent: %w", err)
	}
	before := agent.DeepCopy()

	if agent.Status.Activity == nil {
		agent.Status.Activity = &kyberv1.ActivityStatus{}
	}

	at, err := parseStatusEventTime(ev.At)
	if err != nil {
		// Bad timestamp → use server-side now. Better to record SOMETHING
		// than to drop the event.
		at = time.Now().UTC()
	}
	atMeta := metav1.NewTime(at)

	switch ev.Type {
	case "heartbeat":
		agent.Status.Activity.LastHeartbeatAt = &atMeta
	case "activity":
		// Phase B (kyber#249) lights this branch up. Pre-#249 sidecars
		// won't send these; this case is a forward-compat stub.
		agent.Status.Activity.LastHeartbeatAt = &atMeta
		agent.Status.Activity.LastActivityAt = &atMeta
		if ev.State != "" {
			agent.Status.Activity.State = ev.State
		}
	case "memory_oom":
		// kyber#285: sidecar observed an oom_kill counter increment in
		// the pod-level cgroup memory.events (recursive). Stamp the
		// timestamp on Status.LastKernelOOMKillAt; the agent controller
		// compares it against the agent container's StartedAt to
		// attribute the kill to the current life and routes to
		// MemoryExhausted.
		agent.Status.LastKernelOOMKillAt = &atMeta
	case "resource_usage":
		if ev.Resources == nil {
			return nil
		}
		ev.Resources.SampledAt = atMeta
		cpuLimit := agent.Spec.Resources.CPU.MilliValue()
		memoryLimit := agent.Spec.Resources.Memory.Value()
		ev.Resources.CPULimitMillicores = &cpuLimit
		ev.Resources.MemoryLimitBytes = &memoryLimit
		// The allocation the SIDECAR measured against, captured before the
		// spec's value overwrites it. The two differ after an online PVC grow:
		// KYBER_AGENT_DISK_BYTES is a pod env var read once at sidecar start
		// (status_sidecar.go:85, main.go:377), so a resize does not reach a
		// running pod. Deciding on the spec's larger number while the sidecar
		// still holds the old one would clear the phase while the sidecar keeps
		// the pause marker in place — the agent would report Running and answer
		// nothing. Decide on what the pod actually measured; the operator's
		// Start recreates the pod, and the new sidecar picks up the grow.
		sampledTotal := ev.Resources.DiskTotalBytes
		diskTotal := agent.Spec.Resources.Disk.Value()
		if diskTotal > 0 {
			ev.Resources.DiskTotalBytes = diskTotal
		}
		// Only the new topology-aware sidecar may drive the reserve signal.
		// Older sidecars report node-level statfs on local-path volumes, so
		// accepting their boolean during a rolling upgrade could exhaust every
		// agent at once. Recompute from the allocation instead, through the
		// same decision the sidecar itself runs — if the two ever disagree the
		// agent is paused while reporting Running, or the reverse.
		//
		// "partial" is treated exactly like "ready" here. It used to be allowed
		// to assert exhaustion but never to clear it, which made DiskExhausted
		// permanent on every agent with rootfs persistence, because their walk
		// is always partial. diskreserve.ClearRatio explains why hysteresis is
		// the better trade than refusing to clear.
		var previousReached bool
		if previous := agent.Status.Activity.Resources; previous != nil {
			previousReached = previous.DiskReserveReached
		}
		//
		// An old sidecar sends no DiskUsageState, so Decide returns the previous
		// decision for it whatever total is passed — its node-level statfs
		// figure cannot exhaust a fleet through this path.
		decideTotal := sampledTotal
		if decideTotal <= 0 {
			decideTotal = ev.Resources.DiskTotalBytes
		}
		ev.Resources.DiskReserveReached = diskreserve.Decide(
			previousReached,
			ev.Resources.DiskUsedBytes,
			decideTotal,
			ev.Resources.DiskUsageState,
		)
		// A cgroup-namespaced sidecar sees its own 100m/64Mi cgroup, not the
		// agent container or pod. Replace those misleading values with the
		// metrics API's agent-container usage and the Agent's requested limits.
		// Disk used bytes remain the sidecar's topology-aware sample.
		if s.agentMetrics != nil {
			metrics, metricsErr := s.agentMetrics.AgentContainer(ctx, s.namespace, "agent-"+agentName)
			if metricsErr != nil {
				slog.Warn("agent container metrics unavailable", "agent", agentName, "error", metricsErr)
				// metrics-server removes a terminated agent container before the
				// native sidecar stops reporting disk. Preserve the last valid
				// agent usage across MemoryExhausted/terminal states instead of
				// falling back to the sidecar's own cgroup values.
				if previous := agent.Status.Activity.Resources; previous != nil {
					ev.Resources.CPUUsageMillicores = previous.CPUUsageMillicores
					ev.Resources.MemoryUsedBytes = previous.MemoryUsedBytes
				} else {
					ev.Resources.CPUUsageMillicores = 0
					ev.Resources.MemoryUsedBytes = 0
				}
			} else {
				ev.Resources.CPUUsageMillicores = metrics.CPUUsageMillicores
				ev.Resources.MemoryUsedBytes = metrics.MemoryUsedBytes
			}
		}
		agent.Status.Activity.LastHeartbeatAt = &atMeta
		agent.Status.Activity.Resources = ev.Resources
	default:
		// Unknown event types are ignored. Forward-compat: a newer
		// sidecar pushing a new event kind shouldn't break an older CP.
		return nil
	}

	return s.k8sClient.Status().Patch(ctx, &agent, client.MergeFrom(before))
}

// parseStatusEventTime accepts the sidecar's RFC3339 timestamp; falls
// through with an error so the caller can substitute server-now.
func parseStatusEventTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	return time.Parse(time.RFC3339, s)
}
