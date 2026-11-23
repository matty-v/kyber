package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/activity"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This path never accepts a legacy runtime pod token or grace-mode admission.
// The dedicated credential is mounted ONLY in the platform status sidecar.
func (s *InternalServer) handleActivityObserver(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/internal/activity-observers/"), "/")
	if len(parts) != 2 || parts[0] == "" || len(parts[0]) > 253 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.k8sClient == nil || s.internalAuth == nil || s.internalAuthFailClosed {
		http.Error(w, "observer unavailable", 503)
		return
	}
	identity, err := s.internalAuth.Authenticate(r)
	if err != nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	reader := s.usageAgentReader
	if reader == nil {
		reader = s.k8sClient
	}
	var agent kyberv1.Agent
	if reader.Get(r.Context(), types.NamespacedName{Namespace: s.namespace, Name: parts[0]}, &agent) != nil {
		http.Error(w, "agent unavailable", 404)
		return
	}
	if agent.UID == "" || string(identity) != activity.ObserverIdentity(agent.Name, string(agent.UID)) {
		http.Error(w, "forbidden", 403)
		return
	}
	var report activity.Report
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if dec.Decode(&report) != nil || dec.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid observation", 400)
		return
	}
	now := time.Now().UTC()
	if report.PodUID == "" || len(report.BootID) > 64 || report.BootID == "" || report.BootedAt.IsZero() || report.BootedAt.After(now) || agent.Status.PodName == "" || agent.DeletionTimestamp != nil {
		http.Error(w, "invalid observer", 409)
		return
	}
	var pod corev1.Pod
	if reader.Get(r.Context(), types.NamespacedName{Namespace: s.namespace, Name: agent.Status.PodName}, &pod) != nil {
		http.Error(w, "pod unavailable", 503)
		return
	}
	owner := metav1.GetControllerOf(&pod)
	if string(pod.UID) != report.PodUID || pod.DeletionTimestamp != nil || owner == nil || owner.Kind != "Agent" || owner.UID != agent.UID {
		http.Error(w, "stale observer", 409)
		return
	}
	containerID := ""
	var startedAt time.Time
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.Name == "kyber-status-sidecar" && cs.State.Running != nil {
			containerID = cs.ContainerID
			startedAt = cs.State.Running.StartedAt.Time
		}
	}
	if containerID == "" || startedAt.IsZero() || report.BootedAt.Before(startedAt) {
		http.Error(w, "stale observer life", 409)
		return
	}
	before := agent.DeepCopy()
	if agent.Status.Activity == nil {
		agent.Status.Activity = &kyberv1.ActivityStatus{}
	}
	old := agent.Status.Activity.Work
	same := old != nil && old.Epoch != "" && old.AgentUID == string(agent.UID) && old.PodUID == report.PodUID && old.ContainerID == containerID
	switch parts[1] {
	case "register":
		if report.Epoch != "" || report.Sequence != 0 || !report.ObservedAt.IsZero() {
			http.Error(w, "invalid registration", 400)
			return
		}
		if same {
			// Once registered, even an in-container process restart cannot erase the
			// cursor. A kubelet restart establishes a different native ContainerID.
			if old.BootID != report.BootID {
				http.Error(w, "observer already registered", 409)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(activity.RegistrationResponse{Epoch: old.Epoch, Sequence: old.Sequence})
			return
		}
		history := metav1.MicroTime{}
		if old != nil && old.AgentUID == string(agent.UID) {
			history = old.LastWorkedAt
		}
		agent.Status.Activity.Work = &kyberv1.WorkObservation{LastWorkedAt: history, AgentUID: string(agent.UID), PodUID: report.PodUID, ContainerID: containerID, BootID: report.BootID, BootedAt: metav1.NewMicroTime(report.BootedAt), Epoch: uuid.NewString()}
		if s.k8sClient.Status().Patch(r.Context(), &agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})) != nil {
			http.Error(w, "observer update unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(activity.RegistrationResponse{Epoch: agent.Status.Activity.Work.Epoch})
		return
	case "inventory":
		if !same || old.Epoch != report.Epoch || old.BootID != report.BootID || report.Sequence <= old.Sequence || report.Sequence <= 0 || !report.Snapshot.Valid(now) || report.ObservedAt.Before(old.BootedAt.Time) || (!old.ObservedAt.IsZero() && !report.ObservedAt.Truncate(time.Microsecond).After(old.ObservedAt.Time)) {
			http.Error(w, "invalid inventory ordering or freshness", 409)
			return
		}
		if agent.Status.Phase != kyberv1.AgentPhaseRunning || (agent.Spec.DesiredPhase != "" && agent.Spec.DesiredPhase != kyberv1.AgentPhaseRunning) {
			http.Error(w, "agent not running", 409)
			return
		}
		if (report.Foreground > 0 || report.Background > 0) && report.ObservedAt.After(report.LastWorkedAt) {
			report.LastWorkedAt = report.ObservedAt
		}
		// A reporter/container restart must not erase recent work history.
		// Preserve the platform's monotonic historical timestamp, never refresh
		// it merely because the sidecar is still in contact.
		if report.LastWorkedAt.Before(old.LastWorkedAt.Time) {
			report.LastWorkedAt = old.LastWorkedAt.Time
		}
		prior := activity.Project(activity.Input{Running: true, ContactAt: old.ReceivedAt.Time, Inventory: &activity.Inventory{ObservedAt: old.ObservedAt.Time, LastWorkedAt: old.LastWorkedAt.Time, Foreground: int(old.Foreground), Background: int(old.Background), Complete: old.Complete, NeedsInput: old.NeedsInput}}, now)
		next := activity.Project(activity.Input{Running: true, ContactAt: now, Inventory: &activity.Inventory{ObservedAt: report.ObservedAt, LastWorkedAt: report.LastWorkedAt, Foreground: int(report.Foreground), Background: int(report.Background), Complete: report.Complete, NeedsInput: report.NeedsInput}}, now)
		stateChanged := prior.State != next.State || old.ObservedAt.IsZero()
		old.Sequence = report.Sequence
		old.ObservedAt = metav1.NewMicroTime(report.ObservedAt)
		old.ReceivedAt = metav1.NewMicroTime(now)
		old.Foreground = report.Foreground
		old.Background = report.Background
		old.NeedsInput = report.NeedsInput
		old.Complete = report.Complete
		old.LastWorkedAt = metav1.NewMicroTime(report.LastWorkedAt)
		if stateChanged {
			old.StateSince = old.ObservedAt
		}
		if s.k8sClient.Status().Patch(r.Context(), &agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})) != nil {
			http.Error(w, "observation update unavailable", 503)
			return
		}
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}
