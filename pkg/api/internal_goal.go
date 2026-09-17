package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

const (
	maxAgentGoalRunes       = 120
	platformGoalFallback    = "Working on a new request"
	agentGoalSourcePlatform = "platform"
	agentGoalSourceAgent    = "agent"
)

type agentGoalRequest struct {
	Summary    string `json:"summary,omitempty"`
	AcceptedAt string `json:"acceptedAt"`
}

type agentGoalResponse struct {
	Summary    string `json:"summary"`
	Source     string `json:"source"`
	AcceptedAt string `json:"acceptedAt"`
	UpdatedAt  string `json:"updatedAt"`
}

func (s *InternalServer) handleAgentGoal(w http.ResponseWriter, r *http.Request, agentName string, start bool) {
	if s.k8sClient == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	if !start && r.Method == http.MethodGet {
		goal, err := s.readAgentGoal(r.Context(), agentName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeAgentGoal(w, goal)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req agentGoalRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	acceptedAt, err := time.Parse(time.RFC3339Nano, req.AcceptedAt)
	if err != nil {
		http.Error(w, "acceptedAt must be RFC3339", http.StatusBadRequest)
		return
	}
	var goal *kyberv1.AgentGoalStatus
	if start {
		goal, err = s.startAgentGoal(r.Context(), agentName, acceptedAt)
	} else {
		summary, normalizeErr := normalizeAgentGoal(req.Summary)
		if normalizeErr != nil {
			http.Error(w, normalizeErr.Error(), http.StatusBadRequest)
			return
		}
		goal, err = s.refineAgentGoal(r.Context(), agentName, acceptedAt, summary)
	}
	if err != nil {
		if errors.Is(err, errStaleAgentGoal) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeAgentGoal(w, goal)
}

var errStaleAgentGoal = errors.New("agent goal: revision is stale")

func (s *InternalServer) readAgentGoal(ctx context.Context, agentName string) (*kyberv1.AgentGoalStatus, error) {
	var agent kyberv1.Agent
	if err := s.k8sClient.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: agentName}, &agent); err != nil {
		return nil, fmt.Errorf("get agent: %w", err)
	}
	if agent.Status.Goal == nil {
		return nil, fmt.Errorf("agent goal not found")
	}
	return agent.Status.Goal.DeepCopy(), nil
}

func (s *InternalServer) startAgentGoal(ctx context.Context, agentName string, acceptedAt time.Time) (*kyberv1.AgentGoalStatus, error) {
	var agent kyberv1.Agent
	if err := s.k8sClient.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: agentName}, &agent); err != nil {
		return nil, fmt.Errorf("get agent: %w", err)
	}
	if current := agent.Status.Goal; current != nil {
		if current.AcceptedAt.Time.After(acceptedAt) {
			return nil, errStaleAgentGoal
		}
		if current.AcceptedAt.Time.Equal(acceptedAt) {
			return current.DeepCopy(), nil
		}
	}
	before := agent.DeepCopy()
	// MicroTime is the Kubernetes timestamp type that survives status
	// serialization with sub-second precision. Canonicalize before returning so
	// the hook receives the exact revision subsequent reads will compare.
	now := metav1.NowMicro()
	accepted := metav1.NewMicroTime(acceptedAt.UTC().Truncate(time.Microsecond))
	agent.Status.Goal = &kyberv1.AgentGoalStatus{Summary: platformGoalFallback, Source: agentGoalSourcePlatform, AcceptedAt: accepted, UpdatedAt: now}
	if err := s.k8sClient.Status().Patch(ctx, &agent, client.MergeFrom(before)); err != nil {
		return nil, fmt.Errorf("patch agent goal: %w", err)
	}
	return agent.Status.Goal.DeepCopy(), nil
}

func (s *InternalServer) refineAgentGoal(ctx context.Context, agentName string, acceptedAt time.Time, summary string) (*kyberv1.AgentGoalStatus, error) {
	var agent kyberv1.Agent
	if err := s.k8sClient.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: agentName}, &agent); err != nil {
		return nil, fmt.Errorf("get agent: %w", err)
	}
	if agent.Status.Goal == nil || !agent.Status.Goal.AcceptedAt.Time.Equal(acceptedAt) {
		return nil, errStaleAgentGoal
	}
	before := agent.DeepCopy()
	agent.Status.Goal.Summary = summary
	agent.Status.Goal.Source = agentGoalSourceAgent
	agent.Status.Goal.UpdatedAt = metav1.NowMicro()
	if err := s.k8sClient.Status().Patch(ctx, &agent, client.MergeFrom(before)); err != nil {
		return nil, fmt.Errorf("patch agent goal: %w", err)
	}
	return agent.Status.Goal.DeepCopy(), nil
}

func normalizeAgentGoal(summary string) (string, error) {
	if !utf8.ValidString(summary) {
		return "", fmt.Errorf("summary must be valid UTF-8")
	}
	summary = strings.Join(strings.Fields(summary), " ")
	if summary == "" {
		return "", fmt.Errorf("summary is required")
	}
	if utf8.RuneCountInString(summary) > maxAgentGoalRunes {
		return "", fmt.Errorf("summary exceeds %d characters", maxAgentGoalRunes)
	}
	return summary, nil
}

func writeAgentGoal(w http.ResponseWriter, goal *kyberv1.AgentGoalStatus) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(agentGoalResponse{
		Summary: goal.Summary, Source: goal.Source,
		AcceptedAt: goal.AcceptedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:  goal.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
}
