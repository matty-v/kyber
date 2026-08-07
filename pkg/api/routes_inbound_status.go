package api

import (
	"context"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// inboundRunMu serializes status.inboundRuns[] writes per agent name. The
// status subresource atomically replaces list fields, so two parallel
// receiver invocations against the same agent would otherwise lose an
// append (both read the same baseline, each appends locally, last writer
// wins). Mirrors the InternalServer.jobEventMu pattern in internal.go.
//
// Keyed by agent name; coarse-but-correct — even a high-rate inbound
// binding fires at single-digit rps in practice, so contention is
// negligible. The map grows for the server's lifetime; reclaiming entries
// would require an Agent-delete hook that isn't wired into this package.
var inboundRunMu sync.Map // map[string]*sync.Mutex

// inboundRunMutex returns the per-agent mutex that gates status.inboundRuns
// writes. Mutexes are created lazily and live for the server's lifetime.
func inboundRunMutex(agentName string) *sync.Mutex {
	if mu, ok := inboundRunMu.Load(agentName); ok {
		return mu.(*sync.Mutex)
	}
	mu, _ := inboundRunMu.LoadOrStore(agentName, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// appendInboundRun adds one AgentInboundRun to agent.status.inboundRuns,
// trimming to MaxInboundRunsPerBinding entries PER BINDING (oldest dropped
// first). The cap is per-binding so a noisy binding can't starve the
// others.
//
// Patches via Status().Patch with controller-runtime MergeFrom — same
// pattern as InternalServer.handleJobEvent in internal.go.
//
// Failure modes are intentionally non-fatal for the caller: this function
// returns the error so the caller can log it, but the receiver's HTTP
// response should NOT change based on whether status persistence succeeded.
// A status-write failure is an operational/observability degradation, not
// a security or correctness problem.
func (s *Server) appendInboundRun(ctx context.Context, agentName string, run kyberv1.AgentInboundRun) error {
	mu := inboundRunMutex(agentName)
	mu.Lock()
	defer mu.Unlock()

	key := types.NamespacedName{Name: agentName, Namespace: s.Namespace}
	agent := &kyberv1.Agent{}
	if err := s.K8sClient.Get(ctx, key, agent); err != nil {
		// NotFound is also "not fatal" — the receiver already 401'd if the
		// agent didn't exist, so this branch only fires under a TOCTOU
		// (agent deleted between auth and audit-write).
		if apierrors.IsNotFound(err) {
			return err
		}
		return err
	}
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.InboundRuns = appendInboundRunCapped(
		agent.Status.InboundRuns, run, MaxInboundRunsPerBinding)
	if err := s.K8sClient.Status().Patch(ctx, agent, patch); err != nil {
		return err
	}
	return nil
}

// appendInboundRunCapped appends run to the ring buffer and trims so that
// no binding name has more than cap entries. Older entries for the same
// binding are dropped first. Entries for other bindings are never evicted
// by this call — the cap is enforced PER BINDING, not in aggregate.
//
// Order is preserved (oldest-first). Callers that need per-binding latest
// should scan from the tail.
//
// Complexity: O(n) where n = total entries. n is bounded by
// (#bindings × MaxInboundRunsPerBinding) which in practice is well under
// a few hundred — cheap enough that we don't need a smarter data
// structure.
func appendInboundRunCapped(existing []kyberv1.AgentInboundRun, run kyberv1.AgentInboundRun, cap int) []kyberv1.AgentInboundRun {
	out := append([]kyberv1.AgentInboundRun(nil), existing...)
	out = append(out, run)

	// Count entries with the same binding name. We only need to drop
	// entries for the binding we just appended into — other bindings
	// can't exceed their cap as a result of this single append.
	count := 0
	for _, e := range out {
		if e.BindingName == run.BindingName {
			count++
		}
	}
	if count <= cap {
		return out
	}
	// Drop the oldest matching entries until count == cap. Callers append
	// one at a time, so in practice this loop runs at most once — but the
	// loop form is robust if the caller ever batches.
	for count > cap {
		for i, e := range out {
			if e.BindingName == run.BindingName {
				out = append(out[:i], out[i+1:]...)
				count--
				break
			}
		}
	}
	return out
}
