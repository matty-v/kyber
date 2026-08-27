// Package requeststore stores bounded, ephemeral agent request/reply state.
package requeststore

import (
	"context"
	"errors"
	"time"
)

// Status is the lifecycle state of an agent request.
type Status string

const (
	StatusQueued     Status = "queued"
	StatusDispatched Status = "dispatched"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusExpired    Status = "expired"
)

// FailureCode is a stable caller-visible failure category.
type FailureCode string

const (
	FailureAgentUnavailable FailureCode = "agent_unavailable"
	FailureDelivery         FailureCode = "delivery_failed"
)

const (
	DefaultLifetime         = 60 * time.Second
	DefaultMaxPromptBytes   = 2 * 1024
	DefaultMaxResponseBytes = 8 * 1024
	DefaultMaxOutstanding   = 2
	DefaultMaxTerminal      = 20

	// Hard caps bound future operator configuration. They are intentionally
	// compiled into the control plane rather than trusted to chart values.
	HardMaxLifetime      = 5 * time.Minute
	HardMaxPromptBytes   = 8 * 1024
	HardMaxResponseBytes = 32 * 1024
	HardMaxOutstanding   = 8
	HardMaxTerminal      = 100
)

var (
	ErrNotFound         = errors.New("requeststore: request not found")
	ErrInvalidLimits    = errors.New("requeststore: invalid limits")
	ErrInvalidRequest   = errors.New("requeststore: invalid request")
	ErrPromptTooLarge   = errors.New("requeststore: prompt too large")
	ErrResponseTooLarge = errors.New("requeststore: response too large")
	ErrOutstandingLimit = errors.New("requeststore: outstanding request limit reached")
	ErrConflict         = errors.New("requeststore: transition conflict")
)

// Limits controls request lifetime, payload sizes, concurrency, and retained
// terminal records. Values are validated against the compiled hard caps.
type Limits struct {
	Lifetime         time.Duration
	MaxPromptBytes   int
	MaxResponseBytes int
	MaxOutstanding   int
	MaxTerminal      int
}

// DefaultLimits returns the production defaults from the request/reply design.
func DefaultLimits() Limits {
	return Limits{
		Lifetime:         DefaultLifetime,
		MaxPromptBytes:   DefaultMaxPromptBytes,
		MaxResponseBytes: DefaultMaxResponseBytes,
		MaxOutstanding:   DefaultMaxOutstanding,
		MaxTerminal:      DefaultMaxTerminal,
	}
}

// Validate rejects non-positive limits and values above defensive hard caps.
func (l Limits) Validate() error {
	if l.Lifetime <= 0 || l.Lifetime > HardMaxLifetime ||
		l.MaxPromptBytes <= 0 || l.MaxPromptBytes > HardMaxPromptBytes ||
		l.MaxResponseBytes <= 0 || l.MaxResponseBytes > HardMaxResponseBytes ||
		l.MaxOutstanding <= 0 || l.MaxOutstanding > HardMaxOutstanding ||
		l.MaxTerminal <= 0 || l.MaxTerminal > HardMaxTerminal {
		return ErrInvalidLimits
	}
	return nil
}

// Request is the complete state stored for one bounded agent request.
type Request struct {
	ID          string
	Agent       string
	Prompt      string
	Correlation string
	Status      Status
	Response    string
	FailureCode FailureCode
	CreatedAt   time.Time
	ExpiresAt   time.Time
	UpdatedAt   time.Time
}

// Store atomically creates and transitions ephemeral agent requests.
// Implementations must be safe for concurrent use.
type Store interface {
	Create(ctx context.Context, agent, id, prompt, correlation string) (*Request, error)
	Get(ctx context.Context, agent, id string) (*Request, error)
	MarkDispatched(ctx context.Context, agent, id string) error
	Fail(ctx context.Context, agent, id string, code FailureCode) error
	Complete(ctx context.Context, agent, id, response string) error
}

func validateCreate(agent, id, prompt string, limits Limits) error {
	if agent == "" || id == "" || prompt == "" {
		return ErrInvalidRequest
	}
	if len([]byte(prompt)) > limits.MaxPromptBytes {
		return ErrPromptTooLarge
	}
	return nil
}
