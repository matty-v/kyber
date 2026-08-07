package api

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// defaultInboundFlushInterval is how often the aggregator emits a batched
// Event for each (agent, type, reason) tuple that saw activity. 60s
// balances "operator wants to see something happened" against "don't
// flood the Events API on every burst."
const defaultInboundFlushInterval = 60 * time.Second

// inboundAggKey identifies one bucket in the aggregator. Three-tuple
// because the same agent can hit two distinct event types in a window
// (rate-limited and queue-full) and they must be reported separately.
type inboundAggKey struct {
	AgentName string
	EventType string
	Reason    string
}

// InboundEventAggregator batches high-volume Events on the inbound path
// so a 100-rps flood produces 1 Event per minute, not 100 Events per
// minute.
//
// Per-occurrence Events (sig-mismatch, unmatched, filter-rejected,
// dispatched, config-error) bypass this and call recorder.Event directly
// — they're low-volume by nature.
//
// Lifecycle: NewInboundEventAggregator returns a struct with a background
// goroutine that flushes every flushInterval (default 60s). Stop() cleanly
// drains pending counts and stops the goroutine. Wire into main.go
// alongside the queue-stop Runnable.
//
// Concurrency: Increment/Stop are safe to call from any goroutine. The
// aggregator owns the flush goroutine internally; callers don't need to
// run it.
type InboundEventAggregator struct {
	recorder      record.EventRecorder
	k8sClient     client.Client
	namespace     string
	flushInterval time.Duration

	mu      sync.Mutex
	counts  map[inboundAggKey]int
	stopped bool

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewInboundEventAggregator constructs an aggregator and starts its
// background flush goroutine. recorder may be nil — Increment is a
// no-op in that case (matches the rest of the inbound path's nil-safe
// recorder convention).
func NewInboundEventAggregator(recorder record.EventRecorder, k8sClient client.Client, namespace string) *InboundEventAggregator {
	a := &InboundEventAggregator{
		recorder:      recorder,
		k8sClient:     k8sClient,
		namespace:     namespace,
		flushInterval: defaultInboundFlushInterval,
		counts:        map[inboundAggKey]int{},
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
	go a.run()
	return a
}

// Increment bumps the counter for the given (agentName, eventType, reason)
// tuple. The next flush will emit a single Event carrying the count.
//
// Cheap, mutex-guarded; safe to call from any goroutine. No-op if the
// aggregator's recorder is nil — preserves the "missing dep doesn't
// black-hole traffic" invariant of the rest of the inbound path.
func (a *InboundEventAggregator) Increment(agentName, eventType, reason string) {
	if a == nil || a.recorder == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		// Stop() has already drained counts and the goroutine is gone.
		// Accepting more bumps would silently leak — they'd never flush.
		// Drop them; the caller has no recourse during shutdown anyway.
		return
	}
	a.counts[inboundAggKey{AgentName: agentName, EventType: eventType, Reason: reason}]++
}

// Stop signals the flush goroutine to drain remaining counts and exit.
// Blocks until the goroutine has finished its final flush. Idempotent —
// safe to call multiple times.
func (a *InboundEventAggregator) Stop() {
	if a == nil {
		return
	}
	a.stopOnce.Do(func() {
		close(a.stopCh)
	})
	<-a.doneCh
}

// run is the background flush loop. Exits on Stop, doing one final flush
// so a clean shutdown still surfaces accumulated counts to the Events API.
func (a *InboundEventAggregator) run() {
	defer close(a.doneCh)
	t := time.NewTicker(a.flushInterval)
	defer t.Stop()
	for {
		select {
		case <-a.stopCh:
			a.flushNow()
			return
		case <-t.C:
			a.flushNow()
		}
	}
}

// flushNow emits one Event per non-zero counter and resets the map.
// Exposed (lowercase but callable from same package via test files) so
// tests can drive the flush deterministically without waiting for the
// 60-second tick. The exported alias for tests lives in
// inbound_event_aggregator_export_test.go.
//
// Looks up the Agent CR fresh each flush so the involvedObject reference
// reflects the current resourceVersion. Skips silently when the Agent has
// been deleted — Events on a non-existent object would just be discarded
// by the API server.
func (a *InboundEventAggregator) flushNow() {
	a.mu.Lock()
	// On the Stop() path we set `stopped` here so subsequent Increment
	// calls drop instead of accumulating into a map nobody will flush.
	// Setting under the same mu the run goroutine and Increment use
	// preserves the happens-before with the close(stopCh) signal.
	select {
	case <-a.stopCh:
		a.stopped = true
	default:
	}
	if len(a.counts) == 0 {
		a.mu.Unlock()
		return
	}
	pending := a.counts
	a.counts = map[inboundAggKey]int{}
	a.mu.Unlock()

	for k, count := range pending {
		if count <= 0 {
			continue
		}
		// Look up the Agent so the Event has a real involvedObject.
		// Without this, the recorder would either no-op (controller
		// recorder) or attach to a synthetic ObjectReference that
		// doesn't show up in `kubectl describe agent`.
		agent := &kyberv1.Agent{}
		if a.k8sClient != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := a.k8sClient.Get(ctx, types.NamespacedName{
				Name: k.AgentName, Namespace: a.namespace,
			}, agent)
			cancel()
			if err != nil {
				if apierrors.IsNotFound(err) {
					// Agent gone — drop these counts on the floor.
					continue
				}
				// Transient lookup failure — drop too. Putting them
				// back in the map would risk unbounded growth if the
				// failure persists.
				continue
			}
		}
		a.recorder.Eventf(agent,
			corev1.EventTypeWarning,
			k.EventType,
			"%s — %d in last %s",
			k.Reason, count, a.flushInterval)
	}
}
