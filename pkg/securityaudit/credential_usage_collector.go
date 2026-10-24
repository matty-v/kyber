package securityaudit

import (
	"context"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// CredentialUsageObservation has no request-content or authentication-material
// fields. Unidentified observations deliberately carry no guessed key identity.
type CredentialUsageObservation struct {
	CredentialUsageIdentity
	Transport  string
	Outcome    string // accepted, denied, unrecognized, unavailable, unattributed
	Action     string
	At         time.Time
	Generation uint64
}

func (o CredentialUsageObservation) valid() bool {
	switch o.Outcome {
	case "unrecognized", "unavailable", "unattributed":
		return o.CredentialID == "" && o.PrincipalID == "" && o.TenantID == "" && o.Authority == "" && o.Transport == "" && o.Action == "" && o.At.IsZero() && o.Generation == 0
	case "accepted", "denied":
		if !o.CredentialUsageIdentity.valid() {
			return false
		}
		switch o.Transport {
		case "bearer", "browser-session", "archive-capability":
		default:
			return false
		}
		if o.Outcome == "denied" {
			return o.Action == "" && o.At.IsZero() && o.Generation == 0
		}
		return validUsageTime(o.At) && o.Generation > 0 && credentialUsageActions[o.Action]
	default:
		return false
	}
}

type CredentialUsageObserver interface {
	ObserveCredentialUsage(CredentialUsageObservation)
	CredentialUsageObserverID() string
	CredentialUsageNamespace() string
}

// CredentialUsageCollector keeps database work outside the request path. A
// stable pending batch survives acknowledgement loss; queue overflow is reported
// as lost observations in the next acknowledged batch, never as a false zero.
type CredentialUsageCollector struct {
	writer                        CredentialUsageWriter
	namespace, observerID, podUID string
	startedAt                     time.Time
	queue                         chan CredentialUsageObservation
	inputMu                       sync.RWMutex
	closed                        bool
	lost                          atomic.Uint64
	flushMu                       sync.Mutex
	pending                       *CredentialUsageBatch
	sequence                      int64
	stopped                       bool
	running                       atomic.Bool
	pendingCount                  atomic.Int64
	pendingLoss                   atomic.Uint64
	pendingActive                 atomic.Bool
}

func NewCredentialUsageCollector(writer CredentialUsageWriter, namespace, podUID string) (*CredentialUsageCollector, error) {
	return newCredentialUsageCollector(writer, namespace, podUID, 4096)
}
func newCredentialUsageCollector(writer CredentialUsageWriter, namespace, podUID string, capacity int) (*CredentialUsageCollector, error) {
	if capacity < 1 || capacity > 4096 {
		return nil, ErrInvalid
	}
	c := &CredentialUsageCollector{writer: writer, namespace: namespace, podUID: podUID, observerID: uuid.NewString(), startedAt: time.Now().UTC(), queue: make(chan CredentialUsageObservation, max(1, capacity))}
	if writer == nil || capacity < 1 || capacity > 4096 || (CredentialUsageBatch{Namespace: namespace, ObserverID: c.observerID, PodUID: podUID, StartedAt: c.startedAt, Sequence: 1}).Validate() != nil {
		return nil, ErrInvalid
	}
	return c, nil
}
func (c *CredentialUsageCollector) CredentialUsageObserverID() string {
	if c == nil {
		return ""
	}
	return c.observerID
}
func (c *CredentialUsageCollector) recordLoss() {
	for {
		n := c.lost.Load()
		if n == math.MaxUint64 || c.lost.CompareAndSwap(n, n+1) {
			return
		}
	}
}
func (c *CredentialUsageCollector) ObserveCredentialUsage(observation CredentialUsageObservation) {
	if c == nil {
		return
	}
	c.inputMu.RLock()
	defer c.inputMu.RUnlock()
	if c.closed {
		return
	} // Outside this reporter's stated observation window.
	if !observation.valid() {
		c.recordLoss()
		return
	}
	select {
	case c.queue <- observation:
	default:
		c.recordLoss()
	}
}
func (c *CredentialUsageCollector) buildBatch() CredentialUsageBatch {
	batch := CredentialUsageBatch{Namespace: c.namespace, ObserverID: c.observerID, PodUID: c.podUID, StartedAt: c.startedAt, Sequence: c.sequence + 1, Items: []CredentialUsageDelta{}}
	items := map[string]*CredentialUsageDelta{}
drain:
	for n := 0; n < MaxCredentialUsageItems; n++ {
		var observation CredentialUsageObservation
		select {
		case observation = <-c.queue:
		default:
			break drain
		}
		switch observation.Outcome {
		case "unrecognized":
			batch.Unrecognized++
			continue
		case "unavailable":
			batch.Unavailable++
			continue
		case "unattributed":
			batch.Unattributed++
			continue
		}
		identity := CredentialUsageDelta{CredentialUsageIdentity: observation.CredentialUsageIdentity, Transport: observation.Transport}
		key := identity.key()
		item := items[key]
		if item == nil {
			identity.Actions = map[string]uint64{}
			item = &identity
			items[key] = item
		}
		if observation.Outcome == "denied" {
			item.Denied++
			continue
		}
		item.Authenticated++
		item.Actions[observation.Action]++
		if item.LastAcceptedAt == nil || observation.At.After(*item.LastAcceptedAt) {
			at := observation.At
			item.LastAcceptedAt = &at
			item.Generation = observation.Generation
		} else if observation.At.Equal(*item.LastAcceptedAt) && observation.Generation > item.Generation {
			item.Generation = observation.Generation
		}
	}
	for _, item := range items {
		batch.Items = append(batch.Items, *item)
	}
	c.inputMu.RLock()
	batch.Dropped = c.lost.Swap(0)
	batch.Stopped = c.closed && len(c.queue) == 0
	c.inputMu.RUnlock()
	return batch.normalized()
}

type CredentialUsageLocalStatus struct {
	PendingObservations             int64  `json:"pendingObservations"`
	LossReportsAwaitingConfirmation string `json:"lossReportsAwaitingConfirmation"`
	Closed                          bool   `json:"closed"`
}

func (c *CredentialUsageCollector) CredentialUsageLocalStatus() CredentialUsageLocalStatus {
	if c == nil {
		return CredentialUsageLocalStatus{Closed: true, LossReportsAwaitingConfirmation: "0"}
	}
	c.inputMu.RLock()
	closed := c.closed
	c.inputMu.RUnlock()
	lost := c.lost.Load()
	pendingLoss := c.pendingLoss.Load()
	if math.MaxUint64-lost < pendingLoss {
		lost = math.MaxUint64
	} else {
		lost += pendingLoss
	}
	return CredentialUsageLocalStatus{PendingObservations: int64(len(c.queue)) + c.pendingCount.Load(), LossReportsAwaitingConfirmation: strconv.FormatUint(lost, 10), Closed: closed}
}

// Flush persists at most one immutable batch. Retrying after a lost response
// sends exactly that batch, rather than draining/reassigning its observations.
func (c *CredentialUsageCollector) Flush(ctx context.Context) error {
	if c == nil {
		return ErrInvalid
	}
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	if c.stopped {
		return nil
	}
	if c.pending == nil {
		if c.sequence == math.MaxInt64 {
			return ErrConflict
		}
		batch := c.buildBatch()
		c.pending = &batch
		c.pendingActive.Store(true)
		var count uint64 = batch.Unrecognized + batch.Unavailable + batch.Unattributed
		for _, item := range batch.Items {
			count += item.Authenticated + item.Denied
		}
		c.pendingCount.Store(int64(count))
		c.pendingLoss.Store(batch.Dropped)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := c.writer.WriteCredentialUsage(writeCtx, *c.pending); err != nil {
		return err
	}
	c.sequence = c.pending.Sequence
	c.stopped = c.pending.Stopped
	c.pending = nil
	c.pendingActive.Store(false)
	c.pendingCount.Store(0)
	c.pendingLoss.Store(0)
	return nil
}

// Close ends this reporter's input window before draining. New observations
// remain outside that closed window. A terminal heartbeat is written only when
// every queued observation and loss report has been acknowledged.
func (c *CredentialUsageCollector) Close(ctx context.Context) error {
	if c == nil {
		return ErrInvalid
	}
	c.inputMu.Lock()
	c.closed = true
	c.inputMu.Unlock()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.Flush(ctx); err != nil {
			return err
		}
		c.flushMu.Lock()
		stopped := c.stopped
		c.flushMu.Unlock()
		if stopped {
			return nil
		}
	}
}

// Run flushes active traffic once per second and heartbeats idle collection
// every five seconds. Database failures retain the pending batch and keep the
// authentication path available. Coverage exposes stalled progress and losses.
func (c *CredentialUsageCollector) Run(ctx context.Context) {
	if c == nil || !c.running.CompareAndSwap(false, true) {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastAttempt := time.Time{}
	for {
		if lastAttempt.IsZero() || len(c.queue) > 0 || c.lost.Load() > 0 || c.pendingActive.Load() || time.Since(lastAttempt) >= 5*time.Second {
			_ = c.Flush(ctx)
			lastAttempt = time.Now()
		}
		select {
		case <-ctx.Done():
			finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 6*time.Second)
			_ = c.Close(finalCtx)
			cancel()
			return
		case <-ticker.C:
		}
	}
}

var _ CredentialUsageObserver = (*CredentialUsageCollector)(nil)

func (c *CredentialUsageCollector) CredentialUsageNamespace() string {
	if c == nil {
		return ""
	}
	return c.namespace
}
