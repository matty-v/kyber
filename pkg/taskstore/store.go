// Package taskstore owns Kyber's durable, protocol-neutral agent task model.
package taskstore

import (
	"context"
	"errors"
	"time"
)

const (
	HardMaxPromptBytes      = 32 * 1024
	HardMaxCorrelationBytes = 1024
	HardMaxResponseBytes    = 128 * 1024
	HardMaxIdempotencyBytes = 128
	HardMaxOutstanding      = 100
	HardMaxListPage         = 100
)

var (
	ErrNotFound            = errors.New("taskstore: task not found")
	ErrInvalid             = errors.New("taskstore: invalid task")
	ErrPromptTooLarge      = errors.New("taskstore: prompt too large")
	ErrCorrelationTooLarge = errors.New("taskstore: correlation too large")
	ErrResponseTooLarge    = errors.New("taskstore: response too large")
	ErrIdempotencyTooLarge = errors.New("taskstore: idempotency key too large")
	ErrIdempotencyConflict = errors.New("taskstore: idempotency conflict")
	ErrOutstandingLimit    = errors.New("taskstore: outstanding task limit reached")
	ErrCapacity            = errors.New("taskstore: retained task capacity exhausted")
	ErrConflict            = errors.New("taskstore: transition conflict")
	ErrInvalidCursor       = errors.New("taskstore: invalid cursor")
	ErrNoDispatch          = errors.New("taskstore: no dispatch available")
	ErrReceiptConflict     = errors.New("taskstore: receipt conflict")
)

type State string

const (
	StateQueued     State = "queued"
	StateDispatched State = "dispatched"
	StateCompleted  State = "completed"
	StateFailed     State = "failed"
)

type FailureCode string

const (
	FailureAgentUnavailable FailureCode = "agent_unavailable"
	FailureDelivery         FailureCode = "delivery_failed"
	FailureDeliveryUnknown  FailureCode = "delivery_unknown"
	FailureDeadline         FailureCode = "deadline_exceeded"
	FailureInternal         FailureCode = "internal_error"
)

type Limits struct {
	MaxPromptBytes      int
	MaxCorrelationBytes int
	MaxResponseBytes    int
	MaxOutstanding      int
	MaxRetained         int
	DefaultDeadline     time.Duration
	MaxDeadline         time.Duration
	Retention           time.Duration
	DefaultListPage     int
	MaxListPage         int
	MaxDispatchAttempts int
}

func DefaultLimits() Limits {
	return Limits{
		MaxPromptBytes: 8 * 1024, MaxCorrelationBytes: 256,
		MaxResponseBytes: 32 * 1024, MaxOutstanding: 8, MaxRetained: 100000,
		DefaultDeadline: 24 * time.Hour, MaxDeadline: 7 * 24 * time.Hour,
		Retention: 7 * 24 * time.Hour, DefaultListPage: 20, MaxListPage: 100,
		MaxDispatchAttempts: 3,
	}
}

func (l Limits) Validate() error {
	if l.MaxPromptBytes <= 0 || l.MaxPromptBytes > HardMaxPromptBytes ||
		l.MaxCorrelationBytes < 0 || l.MaxCorrelationBytes > HardMaxCorrelationBytes ||
		l.MaxResponseBytes <= 0 || l.MaxResponseBytes > HardMaxResponseBytes ||
		l.MaxOutstanding <= 0 || l.MaxOutstanding > HardMaxOutstanding ||
		l.MaxRetained <= 0 || l.DefaultDeadline < 5*time.Minute ||
		l.MaxDeadline < l.DefaultDeadline || l.MaxDeadline > 7*24*time.Hour ||
		l.Retention <= 0 || l.Retention > 30*24*time.Hour ||
		l.DefaultListPage <= 0 || l.DefaultListPage > l.MaxListPage ||
		l.MaxListPage <= 0 || l.MaxListPage > HardMaxListPage ||
		l.MaxDispatchAttempts <= 0 || l.MaxDispatchAttempts > 10 {
		return ErrInvalid
	}
	return nil
}

type AgentRef struct{ Namespace, Name string }

type Task struct {
	ID, AgentNamespace, AgentName, CreatedBy, Prompt, Correlation string
	State                                                         State
	FailureCode                                                   FailureCode
	Response                                                      string
	Version                                                       int64
	CreatedAt, UpdatedAt, DeadlineAt, RetainUntil                 time.Time
	CompletedAt                                                   *time.Time
}

type CreateParams struct {
	ID                                                          string
	Agent                                                       AgentRef
	CreatedBy, Prompt, Correlation, IdempotencyKey, RequestHash string
	DeadlineAt                                                  time.Time
}

type CreateResult struct {
	Task   *Task
	Replay bool
}

type ListParams struct {
	Agent  AgentRef
	State  State
	Limit  int
	Cursor string
}
type Page struct {
	Tasks      []*Task
	NextCursor string
}

// Store implementations must make every mutation atomic. Create also persists
// dispatch intent in the same transaction in durable implementations.
type Store interface {
	Create(context.Context, CreateParams) (*CreateResult, error)
	Get(context.Context, AgentRef, string) (*Task, error)
	List(context.Context, ListParams) (*Page, error)
	MarkDispatched(context.Context, AgentRef, string, int64) error
	Fail(context.Context, AgentRef, string, int64, FailureCode) error
	Complete(context.Context, AgentRef, string, int64, string) error
}

type DispatchClaim struct {
	Task       *Task
	LeaseOwner string
	LeaseUntil time.Time
	Attempts   int
}

type Receipt struct {
	TaskID    string `json:"taskId"`
	AttemptID string `json:"attemptId"`
	Runtime   string `json:"runtime"`
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId,omitempty"`
}

type ReconcileResult struct{ RequeuedLeases, UnknownAttempts, ExpiredTasks, DeletedTasks int64 }

// DispatchStore is the production-only extension used by the durable worker
// and receipt endpoint. The memory store intentionally need not emulate
// cross-replica leases.
type DispatchStore interface {
	Store
	ClaimPending(context.Context, string, time.Duration) (*DispatchClaim, error)
	BeginAttempt(context.Context, AgentRef, string, string, string) error
	MarkReceiptPending(context.Context, AgentRef, string, string, string) error
	ReleaseLease(context.Context, AgentRef, string, string, time.Duration) error
	RenewLease(context.Context, AgentRef, string, string, time.Duration) error
	FailDelivery(context.Context, AgentRef, string, string, int64, FailureCode) error
	AcceptReceipt(context.Context, AgentRef, Receipt) (*Task, bool, error)
	GetReceipt(context.Context, AgentRef, string) (*Receipt, error)
	Reconcile(context.Context, int) (*ReconcileResult, error)
}

func cloneTask(t *Task) *Task {
	if t == nil {
		return nil
	}
	c := *t
	if t.CompletedAt != nil {
		v := *t.CompletedAt
		c.CompletedAt = &v
	}
	return &c
}
