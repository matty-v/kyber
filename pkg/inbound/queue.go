package inbound

import (
	"context"
	"errors"
	"sync"
	"time"
)

// QueueDepth is the maximum number of in-flight jobs a single agent may hold
// before further Enqueue calls are rejected with ErrQueueFull. Hardcoded for
// V1 of inbound prompts; tune via constant when load testing motivates it.
const QueueDepth = 5

// ErrQueueFull is returned by Queue.Enqueue when the per-agent channel buffer
// is at capacity.
var ErrQueueFull = errors.New("inbound: agent queue full")

// Job is the unit of work passed from Enqueue to the per-agent worker. Fields
// are populated by the dispatcher before enqueue.
type Job struct {
	Agent      string
	Binding    string
	RequestID  string
	Envelope   string // rendered prompt body delivered to tmux
	EnqueuedAt time.Time
}

// Handler is invoked synchronously by the per-agent worker for each job. The
// context is the queue's lifetime context; handlers should respect
// cancellation but the queue does not enforce a per-job deadline.
type Handler func(ctx context.Context, job Job)

// agentChannel pairs the buffered channel with a once-guard so Stop only
// closes the channel a single time even under concurrent shutdown calls.
type agentChannel struct {
	ch        chan Job
	closeOnce sync.Once
}

// Queue is the per-agent in-flight FIFO. One goroutine per observed agent
// drains its channel and calls the handler in order.
type Queue struct {
	mu       sync.Mutex
	channels map[string]*agentChannel
	handler  Handler
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
	stopped  bool
}

// NewQueue returns a started Queue. handler is called synchronously by the
// per-agent worker — long-running handlers will hold up the queue for that
// agent (which is the desired behaviour: serialise prompts per agent).
func NewQueue(handler Handler) *Queue {
	ctx, cancel := context.WithCancel(context.Background())
	return &Queue{
		channels: make(map[string]*agentChannel),
		handler:  handler,
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Enqueue delivers job to the agent's bounded queue. If the queue is full,
// ErrQueueFull is returned without blocking. The first Enqueue per agent
// lazily spawns the worker goroutine.
func (q *Queue) Enqueue(job Job) error {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return ErrQueueFull
	}
	ac, ok := q.channels[job.Agent]
	if !ok {
		ac = &agentChannel{ch: make(chan Job, QueueDepth)}
		q.channels[job.Agent] = ac
		q.wg.Add(1)
		go q.worker(ac.ch)
	}
	q.mu.Unlock()

	select {
	case ac.ch <- job:
		return nil
	default:
		return ErrQueueFull
	}
}

// worker drains one agent's channel until it is closed.
func (q *Queue) worker(ch <-chan Job) {
	defer q.wg.Done()
	for job := range ch {
		q.handler(q.ctx, job)
	}
}

// Stop closes every per-agent channel and waits for in-flight handlers to
// drain. Once Stop has been called, further Enqueue calls return
// ErrQueueFull. Stop is idempotent.
func (q *Queue) Stop() {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return
	}
	q.stopped = true
	channels := make([]*agentChannel, 0, len(q.channels))
	for _, ac := range q.channels {
		channels = append(channels, ac)
	}
	q.mu.Unlock()

	for _, ac := range channels {
		ac.closeOnce.Do(func() { close(ac.ch) })
	}
	q.cancel()
	q.wg.Wait()
}
