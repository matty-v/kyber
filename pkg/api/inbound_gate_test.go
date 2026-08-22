package api

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func gateServer(t *testing.T, phase kyberv1.AgentPhase) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "wedge", Namespace: fdNS},
		Status:     kyberv1.AgentStatus{Phase: phase},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent).WithObjects(agent).Build()
	return &Server{K8sClient: c, Namespace: fdNS}
}

func TestWaitAgentRunning_RunningReturnsImmediately(t *testing.T) {
	s := gateServer(t, kyberv1.AgentPhaseRunning)
	if err := s.WaitAgentRunning(context.Background(), "wedge", time.Minute); err != nil {
		t.Fatalf("WaitAgentRunning: %v", err)
	}
}

func TestWaitAgentRunning_TerminalPhasesFailFast(t *testing.T) {
	// A phase that never flips on its own must not burn the timeout —
	// the per-agent FIFO worker would be pinned for the full wait per
	// message, turning a burst to a stopped agent into ~15 min of
	// queue-full.
	for _, phase := range []kyberv1.AgentPhase{
		kyberv1.AgentPhaseStopped, kyberv1.AgentPhaseSuspended, kyberv1.AgentPhaseFailed,
		kyberv1.AgentPhaseNeedsAuth, kyberv1.AgentPhaseMemoryExhausted,
	} {
		t.Run(string(phase), func(t *testing.T) {
			s := gateServer(t, phase)
			start := time.Now()
			err := s.WaitAgentRunning(context.Background(), "wedge", time.Minute)
			if err == nil {
				t.Fatalf("phase %s: expected error, got nil", phase)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("phase %s: took %s — must fail fast, not wait out the timeout", phase, elapsed)
			}
		})
	}
}

func TestWaitAgentRunning_TransitionalPhaseWaitsThenTimesOut(t *testing.T) {
	s := gateServer(t, kyberv1.AgentPhaseRestarting)
	start := time.Now()
	err := s.WaitAgentRunning(context.Background(), "wedge", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error for an agent stuck Restarting")
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("returned before the timeout for a transitional phase")
	}
}

func TestWaitAgentRunning_MissingAgentErrors(t *testing.T) {
	s := gateServer(t, kyberv1.AgentPhaseRunning)
	if err := s.WaitAgentRunning(context.Background(), "nobody", time.Minute); err == nil {
		t.Fatal("expected lookup error for a missing agent")
	}
}
