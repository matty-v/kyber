package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
)

type switchRuntimeRequest struct {
	Runtime string `json:"runtime"`
}

// handleSwitchRuntime prepares the target on the same PVC before committing
// the runtime change. Until the one spec patch succeeds, the source harness
// and its pod remain authoritative. The controller then tears that pod down
// through the existing force-NeedsAuth transition.
func (s *Server) handleSwitchRuntime(w http.ResponseWriter, r *http.Request, name string) {
	if !s.authorizeAction(w, r, name, "switch-runtime", ScopeLifecycleWrite) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req switchRuntimeRequest
	if err := dec.Decode(&req); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "expected one JSON object with a runtime")
		return
	}
	if req.Runtime == "" || len(s.ValidRuntimes) > 0 && !s.ValidRuntimes[req.Runtime] {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "runtime is not registered", "runtime")
		return
	}
	descriptor, ok := runtimes.Describe(req.Runtime)
	if !ok {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "runtime is not registered", "runtime")
		return
	}
	if s.RuntimeImages != nil && s.RuntimeImages[req.Runtime] == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			fmt.Sprintf("runtime %q has no image configured; pin image.%s.tag in the install's Helm values", req.Runtime, runtimes.HelmImageKey(req.Runtime)), "runtime")
		return
	}

	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	agent := &kyberv1.Agent{}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to read agent")
		return
	}
	if req.Runtime == agent.Spec.Runtime {
		writeJSONError(w, http.StatusConflict, "same_runtime", "agent already uses this runtime")
		return
	}
	switch agent.Status.Phase {
	case kyberv1.AgentPhaseRunning, kyberv1.AgentPhaseStopped, kyberv1.AgentPhaseFailed, kyberv1.AgentPhaseNeedsAuth:
	default:
		writeJSONError(w, http.StatusConflict, "invalid_phase", fmt.Sprintf("cannot switch runtime while agent is %s", agent.Status.Phase))
		return
	}
	if _, ok := descriptor.Auth(agent.Spec.Secrets.AuthType); !ok {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			fmt.Sprintf("%s does not offer %s authentication", descriptor.Name, agent.Spec.Secrets.AuthType), "authType")
		return
	}
	channels := map[string]bool{
		"telegram": agent.Spec.Secrets.TelegramEnabled,
		"discord":  agent.Spec.Secrets.DiscordEnabled || agent.Spec.Channels != nil && agent.Spec.Channels.Discord != nil,
		"slack":    agent.Spec.Secrets.SlackEnabled,
	}
	for _, channel := range []string{"telegram", "discord", "slack"} {
		if channels[channel] {
			if err := validateChannelAuth(req.Runtime, agent.Spec.Secrets.AuthType, channel); err != nil {
				writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), "runtime")
				return
			}
		}
	}
	if agent.Spec.Inference != nil && !descriptor.Supports(runtimes.CustomInferenceEndpoint) {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			fmt.Sprintf("%s does not support this agent's custom inference endpoint", descriptor.Name), "runtime")
		return
	}

	// The target image owns the package to install; the request cannot choose
	// an image, package, path, or version. A runtime without a repair contract
	// is installed by its normal image bootstrap instead.
	plan, hasRepairPlan := s.RuntimeRepairPlans[req.Runtime]
	if descriptor.Supports(runtimes.RuntimeRepairFeature) && !hasRepairPlan {
		writeJSONError(w, http.StatusNotImplemented, "switch_not_supported", "target runtime maintenance plan is not configured")
		return
	}
	if hasRepairPlan {
		if plan.Image == "" {
			writeJSONError(w, http.StatusNotImplemented, "switch_not_supported", "target runtime maintenance image is not configured")
			return
		}
		runner := s.RuntimeRepairRunner
		if runner == nil {
			runner = &kubernetesRuntimeRepairRunner{server: s}
		}
		if _, err := runner.Run(r.Context(), agent, plan); err != nil {
			if errors.Is(err, ErrRuntimeRepairInProgress) {
				writeJSONError(w, http.StatusConflict, "repair_in_progress", "runtime maintenance is already in progress")
				return
			}
			slog.Error("target runtime preparation failed", "agent", name, "runtime", req.Runtime, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "switch_failed", "target runtime preparation failed; agent remains on the old runtime: "+boundedRepairOutput(err.Error()))
			return
		}
	}

	// Maintenance can take minutes. Re-read and require the same UID/spec and
	// stable phase so an operator change cannot be overwritten by a stale
	// request. The optimistic-lock patch closes the final race.
	current := &kyberv1.Agent{}
	if err := s.K8sClient.Get(r.Context(), key, current); err != nil {
		writeJSONError(w, http.StatusConflict, "agent_changed", "agent changed during runtime preparation")
		return
	}
	if current.UID != agent.UID || current.Generation != agent.Generation || current.Status.Phase != agent.Status.Phase || current.Spec.Runtime != agent.Spec.Runtime {
		writeJSONError(w, http.StatusConflict, "agent_changed", "agent changed during runtime preparation")
		return
	}
	before := current.DeepCopy()
	current.Spec.Runtime = req.Runtime
	current.Spec.Model = ""
	current.Spec.RuntimeVersion = ""
	current.Spec.DesiredPhase = kyberv1.AgentPhaseNeedsAuth
	if err := s.K8sClient.Patch(r.Context(), current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		if k8serrors.IsConflict(err) {
			writeJSONError(w, http.StatusConflict, "agent_changed", "agent changed during runtime preparation; retry")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "runtime switch could not be committed")
		return
	}
	if s.Recorder != nil {
		s.Recorder.Eventf(current, corev1.EventTypeNormal, "RuntimeSwitchRequested", "runtime %s → %s; reauthorization requested", before.Spec.Runtime, req.Runtime)
	}
	jobWarning := ""
	if !descriptor.Supports(runtimes.JobTurnHooks) {
		for _, job := range current.Spec.Jobs {
			if job.Exclusive || job.ClearContextAfter {
				jobWarning = "scheduled jobs remain configured, but exclusive and clearContextAfter are inert on this runtime"
				break
			}
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"agent": name, "runtime": req.Runtime,
		"message":               "target prepared; agent is moving to NeedsAuth on the same volume",
		"jobTurnHooksSupported": descriptor.Supports(runtimes.JobTurnHooks),
		"jobWarning":            jobWarning,
	})
}
