package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// Event is the JSON envelope pushed to WebSocket clients on every CRD change.
type Event struct {
	// Type encodes "<resource>.<verb>", e.g. "agent.updated" or "machine.created".
	Type      string        `json:"type"`
	Timestamp string        `json:"timestamp"`
	Resource  EventResource `json:"resource"`
}

// EventResource is the resource sub-object inside Event.
type EventResource struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Phase   string `json:"phase,omitempty"`
	Machine string `json:"machine,omitempty"`
}

// EventBus manages the set of active WebSocket event subscribers.
// A single EventBus is embedded in Server and shared across all connections.
type EventBus struct {
	mu          sync.Mutex
	subscribers map[chan Event]struct{}
}

// NewEventBus creates a new EventBus.
func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make(map[chan Event]struct{}),
	}
}

// subscribe registers a new channel and returns it.
func (b *EventBus) subscribe() chan Event {
	ch := make(chan Event, 32)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// unsubscribe removes and closes the channel.
func (b *EventBus) unsubscribe(ch chan Event) {
	b.mu.Lock()
	delete(b.subscribers, ch)
	b.mu.Unlock()
	close(ch)
}

// Publish sends an Event to every subscriber, dropping messages to full channels.
func (b *EventBus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subscribers {
		select {
		case ch <- e:
		default:
			// Slow client — drop rather than block the broadcaster.
		}
	}
}

// informerEventHandler implements the cache.ResourceEventHandler callbacks and
// converts them into Event values published on the bus.
type informerEventHandler struct {
	bus  *EventBus
	kind string // "Agent" or "Machine"
}

func (h *informerEventHandler) eventType(verb string) string {
	return h.kind + "." + verb
}

func (h *informerEventHandler) buildEvent(obj interface{}, verb string) (Event, bool) {
	ro, ok := obj.(runtime.Object)
	if !ok {
		return Event{}, false
	}

	accessor, err := meta.Accessor(ro)
	if err != nil {
		return Event{}, false
	}

	res := EventResource{
		Kind: h.kind,
		Name: accessor.GetName(),
	}

	switch v := obj.(type) {
	case *kyberv1.Agent:
		res.Phase = string(v.Status.Phase)
		res.Machine = v.Spec.Machine
	case *kyberv1.Machine:
		res.Phase = string(v.Status.Phase)
	}

	return Event{
		Type:      h.eventType(verb),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Resource:  res,
	}, true
}

func (h *informerEventHandler) OnAdd(obj interface{}, isInitialList bool) {
	if isInitialList {
		return // suppress the initial list-sync flood
	}
	if e, ok := h.buildEvent(obj, "created"); ok {
		h.bus.Publish(e)
	}
}

func (h *informerEventHandler) OnUpdate(oldObj, newObj interface{}) {
	// Suppress Agent updates whose only diff is the activity heartbeat
	// timestamp. The kyber-status-sidecar patches LastHeartbeatAt every 5s
	// per agent (kyber#249), and the PWA does not render that field — so
	// republishing each heartbeat just churns WebSocket traffic and triggers
	// list refetches that the operator sees as bouncing card order. See #263.
	if h.kind == "Agent" && isHeartbeatOnlyUpdate(oldObj, newObj) {
		return
	}
	if e, ok := h.buildEvent(newObj, "updated"); ok {
		h.bus.Publish(e)
	}
}

// isHeartbeatOnlyUpdate reports whether the only difference between two
// Agent objects is Status.Activity.LastHeartbeatAt. Returns false on type
// mismatch or any other Spec/Status diff so genuine changes still publish.
func isHeartbeatOnlyUpdate(oldObj, newObj interface{}) bool {
	oldA, ok := oldObj.(*kyberv1.Agent)
	if !ok {
		return false
	}
	newA, ok := newObj.(*kyberv1.Agent)
	if !ok {
		return false
	}
	if !reflect.DeepEqual(oldA.Spec, newA.Spec) {
		return false
	}
	// Compare a deep copy of Status with LastHeartbeatAt scrubbed on both
	// sides. Anything else (phase, activity.state, conditions, scheduling,
	// inboundRuns, …) still triggers a publish.
	oldStatus := oldA.Status.DeepCopy()
	newStatus := newA.Status.DeepCopy()
	if oldStatus.Activity != nil {
		oldStatus.Activity.LastHeartbeatAt = nil
	}
	if newStatus.Activity != nil {
		newStatus.Activity.LastHeartbeatAt = nil
	}
	return reflect.DeepEqual(oldStatus, newStatus)
}

func (h *informerEventHandler) OnDelete(obj interface{}) {
	if e, ok := h.buildEvent(obj, "deleted"); ok {
		h.bus.Publish(e)
	}
}

// startEventInformers registers informers for Agent and Machine CRDs on the
// controller-runtime cache. Call once during Server.Start before the manager
// starts its cache sync. Returns a non-nil error if informer registration fails.
func (s *Server) startEventInformers(ctx context.Context, c cache.Cache) error {
	for _, reg := range []struct {
		obj  client.Object
		kind string
	}{
		{obj: &kyberv1.Agent{}, kind: "Agent"},
		{obj: &kyberv1.Machine{}, kind: "Machine"},
	} {
		informer, err := c.GetInformer(ctx, reg.obj)
		if err != nil {
			return err
		}
		_, err = informer.AddEventHandler(&informerEventHandler{
			bus:  s.EventBus,
			kind: reg.kind,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// handleEvents upgrades the connection to WebSocket and streams CRD events
// until the client disconnects or the server shuts down.
//
// GET /api/v1/events (Upgrade: websocket)
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	if s.EventBus == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
			"event stream not initialised")
		return
	}

	conn := upgradeWebSocket(w, r)
	if conn == nil {
		return
	}

	writer := newWSSafeWriter(conn)
	defer writer.Close()

	sub := s.EventBus.subscribe()
	defer s.EventBus.unsubscribe(sub)

	// Detect client disconnect: read loop discards all inbound frames and
	// signals via the closed channel when the connection goes away.
	disconnected := make(chan struct{})
	go func() {
		defer close(disconnected)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-disconnected:
			return
		case e, ok := <-sub:
			if !ok {
				return
			}
			b, err := json.Marshal(e)
			if err != nil {
				continue
			}
			writer.WriteText(b)
		}
	}
}
