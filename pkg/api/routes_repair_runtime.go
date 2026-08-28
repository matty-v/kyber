package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

const (
	runtimeRepairTimeout   = 5 * time.Minute
	runtimeRepairPoll      = 2 * time.Second
	runtimeRepairMaxOutput = 512
)

var ErrRuntimeRepairInProgress = errors.New("runtime repair already in progress")

// RuntimeRepairPlan is the API-side snapshot of a runtime adapter's repair
// contract. Values are fixed by registered runtime code, never request input.
type RuntimeRepairPlan struct {
	Image          string
	PackageName    string
	BinaryName     string
	Version        string
	PackagePath    string
	ExecutablePath string
}

// RuntimeRepairRunner performs the bounded maintenance operation. The
// production implementation creates a short-lived same-node pod; this seam
// keeps HTTP phase/auth/conflict tests deterministic.
type RuntimeRepairRunner interface {
	Run(context.Context, *kyberv1.Agent, RuntimeRepairPlan) (string, error)
}

func (s *Server) handleRepairRuntime(w http.ResponseWriter, r *http.Request, name string) {
	if !s.authorizeAction(w, r, name, "repair-runtime", ScopeLifecycleWrite) {
		return
	}

	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+name+"' not found")
			return
		}
		slog.Error("failed to get agent for runtime repair", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}
	if agent.Status.Phase != kyberv1.AgentPhaseBrokenRuntime {
		writeJSONError(w, http.StatusConflict, "invalid_phase",
			fmt.Sprintf("runtime repair requires BrokenRuntime (current phase: %s)", agent.Status.Phase))
		return
	}

	plan, ok := s.RuntimeRepairPlans[agent.Spec.Runtime]
	if !ok || plan.Image == "" {
		writeJSONError(w, http.StatusNotImplemented, "repair_not_supported",
			fmt.Sprintf("runtime repair is not configured for runtime '%s'", agent.Spec.Runtime))
		return
	}
	if agent.Spec.RuntimeVersion != "" {
		plan.Version = agent.Spec.RuntimeVersion
	}

	runner := s.RuntimeRepairRunner
	if runner == nil {
		runner = &kubernetesRuntimeRepairRunner{server: s}
	}
	output, err := runner.Run(r.Context(), agent, plan)
	if err != nil {
		if errors.Is(err, ErrRuntimeRepairInProgress) {
			writeJSONError(w, http.StatusConflict, "repair_in_progress", "runtime repair is already in progress")
			return
		}
		slog.Error("runtime repair failed", "agent", name, "runtime", agent.Spec.Runtime, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "repair_failed",
			"runtime repair failed; the agent remains in BrokenRuntime: "+boundedRepairOutput(err.Error()))
		return
	}

	before := agent.DeepCopy()
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
	if err := s.K8sClient.Patch(r.Context(), agent, client.MergeFrom(before)); err != nil {
		if k8serrors.IsConflict(err) {
			writeJSONError(w, http.StatusConflict, "agent_changed", "agent changed during repair; retry the repair action")
			return
		}
		slog.Error("failed to request repaired agent restart", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "runtime repaired but restart could not be requested")
		return
	}

	if s.Recorder != nil {
		s.Recorder.Eventf(agent, corev1.EventTypeNormal, "RuntimeRepaired",
			"runtime=%s repaired through maintenance pod; restart requested", agent.Spec.Runtime)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"agent":   name,
		"runtime": agent.Spec.Runtime,
		"message": "runtime repaired; agent restart requested",
		"output":  boundedRepairOutput(output),
	})
}

type kubernetesRuntimeRepairRunner struct{ server *Server }

func (r *kubernetesRuntimeRepairRunner) Run(ctx context.Context, agent *kyberv1.Agent, plan RuntimeRepairPlan) (string, error) {
	pod, err := r.buildPod(ctx, agent, plan)
	if err != nil {
		return "", err
	}
	if err := r.server.K8sClient.Create(ctx, pod); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			return "", ErrRuntimeRepairInProgress
		}
		return "", fmt.Errorf("creating maintenance pod: %w", err)
	}

	defer func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancelCleanup()
		if err := r.server.K8sClient.Delete(cleanupCtx, pod); err != nil && !k8serrors.IsNotFound(err) {
			slog.Warn("failed to delete runtime repair pod", "pod", pod.Name, "error", err)
		}
	}()

	waitCtx, cancel := context.WithTimeout(ctx, runtimeRepairTimeout)
	defer cancel()
	ticker := time.NewTicker(runtimeRepairPoll)
	defer ticker.Stop()
	for {
		stored := &corev1.Pod{}
		if err := r.server.K8sClient.Get(waitCtx, client.ObjectKeyFromObject(pod), stored); err != nil {
			// The controller-runtime client is cache-backed. A successful Create can
			// briefly be followed by NotFound until the informer observes the pod;
			// keep polling within the same bounded deadline instead of deleting a
			// repair that has only just started.
			if k8serrors.IsNotFound(err) {
				select {
				case <-waitCtx.Done():
					return "", fmt.Errorf("waiting for maintenance pod: %w", waitCtx.Err())
				case <-ticker.C:
					continue
				}
			}
			return "", fmt.Errorf("reading maintenance pod: %w", err)
		}
		switch stored.Status.Phase {
		case corev1.PodSucceeded:
			return "repair completed and executable verified", nil
		case corev1.PodFailed:
			return "", fmt.Errorf("maintenance pod failed: %s", repairTerminationMessage(stored))
		}
		select {
		case <-waitCtx.Done():
			return "", fmt.Errorf("waiting for maintenance pod: %w", waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func (r *kubernetesRuntimeRepairRunner) buildPod(ctx context.Context, agent *kyberv1.Agent, plan RuntimeRepairPlan) (*corev1.Pod, error) {
	nodeName := ""
	activePod := &corev1.Pod{}
	if err := r.server.K8sClient.Get(ctx, types.NamespacedName{Name: "agent-" + agent.Name, Namespace: r.server.Namespace}, activePod); err == nil {
		nodeName = activePod.Spec.NodeName
	} else if !k8serrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading agent pod: %w", err)
	}
	if nodeName == "" {
		machine := &kyberv1.Machine{}
		if err := r.server.K8sClient.Get(ctx, types.NamespacedName{Name: agent.Spec.Machine, Namespace: r.server.Namespace}, machine); err != nil {
			return nil, fmt.Errorf("resolving repair node: %w", err)
		}
		nodeName = machine.Status.NodeName
	}
	if nodeName == "" {
		return nil, errors.New("resolving repair node: assigned machine has no node")
	}

	activeDeadline := int64(runtimeRepairTimeout.Seconds())
	automount := false
	runAsRoot := int64(0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runtimeRepairPodName(agent.Name),
			Namespace: r.server.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/part-of":   "kyber",
				"app.kubernetes.io/component": "runtime-repair",
				"kyber.io/agent":              agent.Name,
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(agent, kyberv1.GroupVersion.WithKind("Agent"))},
		},
		Spec: corev1.PodSpec{
			NodeName:                     nodeName,
			RestartPolicy:                corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:        &activeDeadline,
			AutomountServiceAccountToken: &automount,
			Volumes: []corev1.Volume{{
				Name: "persist",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: "agent-" + agent.Name + "-pv",
				}},
			}},
			Containers: []corev1.Container{{
				Name:    "repair",
				Image:   plan.Image,
				Command: []string{"/usr/local/bin/kyber-runtime-repair"},
				Args: []string{
					"/persist/agentroot", plan.PackageName, plan.BinaryName, plan.Version,
					plan.PackagePath, plan.ExecutablePath,
				},
				SecurityContext: &corev1.SecurityContext{RunAsUser: &runAsRoot},
				VolumeMounts:    []corev1.VolumeMount{{Name: "persist", MountPath: "/persist"}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
				},
			}},
		},
	}
	return pod, nil
}

func runtimeRepairPodName(agentName string) string {
	sum := sha256.Sum256([]byte(agentName))
	suffix := fmt.Sprintf("-repair-%x", sum[:4])
	maxName := 63 - len("agent-") - len(suffix)
	trimmed := strings.Trim(agentName, "-")
	if len(trimmed) > maxName {
		trimmed = strings.TrimRight(trimmed[:maxName], "-")
	}
	return "agent-" + trimmed + suffix
}

func repairTerminationMessage(pod *corev1.Pod) string {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "repair" && status.State.Terminated != nil {
			term := status.State.Terminated
			// Do not include the termination message: it can contain arbitrary
			// installer output. Exit code and Kubernetes' bounded reason are enough
			// for the operator-facing error while keeping audit logs non-sensitive.
			return fmt.Sprintf("exit %d (%s)", term.ExitCode, term.Reason)
		}
	}
	return "maintenance pod terminated without a diagnostic"
}

func boundedRepairOutput(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if len(s) > runtimeRepairMaxOutput {
		return s[:runtimeRepairMaxOutput] + "…"
	}
	return s
}
