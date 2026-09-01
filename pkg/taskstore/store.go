// Package taskstore owns Kyber's durable, protocol-neutral agent task model.
package taskstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	HardMaxPromptBytes              = 32 * 1024
	HardMaxCorrelationBytes         = 1024
	HardMaxResponseBytes            = 128 * 1024
	HardMaxIdempotencyBytes         = 128
	HardMaxOutstanding              = 100
	HardMaxListPage                 = 100
	HardMaxProgressBytes            = 4 * 1024
	HardMaxProgressUpdates          = 2000
	HardMaxResults                  = 64
	HardMaxResultParts              = 32
	HardMaxTextPartBytes            = 128 * 1024
	HardMaxJSONPartBytes            = 256 * 1024
	HardMaxFileBytes                = 100 * 1024 * 1024
	HardMaxTaskFileBytes            = 500 * 1024 * 1024
	HardMaxResultNameBytes          = 256
	HardMaxDescriptionBytes         = 4 * 1024
	HardMaxFilenameBytes            = 255
	HardMaxCancelReasonBytes        = 2 * 1024
	HardMaxInteractionQuestionBytes = 16 * 1024
	HardMaxInteractionResponseBytes = 256 * 1024
	HardMaxInteractionOptions       = 100
	HardMaxTaskInteractions         = 64
	HardMaxTaskMessages             = 256
)

var (
	ErrNotFound             = errors.New("taskstore: task not found")
	ErrInvalid              = errors.New("taskstore: invalid task")
	ErrPromptTooLarge       = errors.New("taskstore: prompt too large")
	ErrCorrelationTooLarge  = errors.New("taskstore: correlation too large")
	ErrResponseTooLarge     = errors.New("taskstore: response too large")
	ErrIdempotencyTooLarge  = errors.New("taskstore: idempotency key too large")
	ErrIdempotencyConflict  = errors.New("taskstore: idempotency conflict")
	ErrOutstandingLimit     = errors.New("taskstore: outstanding task limit reached")
	ErrCapacity             = errors.New("taskstore: retained task capacity exhausted")
	ErrConflict             = errors.New("taskstore: transition conflict")
	ErrInvalidCursor        = errors.New("taskstore: invalid cursor")
	ErrNoDispatch           = errors.New("taskstore: no dispatch available")
	ErrReceiptConflict      = errors.New("taskstore: receipt conflict")
	ErrUpdateConflict       = errors.New("taskstore: progress update conflict")
	ErrResultConflict       = errors.New("taskstore: result conflict")
	ErrProgressTooLarge     = errors.New("taskstore: progress too large")
	ErrResultTooLarge       = errors.New("taskstore: result too large")
	ErrResultLimit          = errors.New("taskstore: result limit reached")
	ErrUpdateLimit          = errors.New("taskstore: progress update limit reached")
	ErrInvalidAttempt       = errors.New("taskstore: invalid attempt")
	ErrCancelReasonTooLarge = errors.New("taskstore: cancellation reason too large")
	ErrInteractionLimit     = errors.New("taskstore: interaction limit reached")
	ErrInteractionNotReady  = errors.New("taskstore: interaction is not awaiting a response")
	ErrInteractionExpired   = errors.New("taskstore: interaction expired")
)

type State string

const (
	StateQueued        State = "queued"
	StateDispatched    State = "dispatched"
	StateCanceling     State = "canceling"
	StateInputRequired State = "input_required"
	StateAuthRequired  State = "auth_required"
	StateCanceled      State = "canceled"
	StateCompleted     State = "completed"
	StateFailed        State = "failed"
	StateRejected      State = "rejected"
)

type FailureCode string

const (
	FailureAgentUnavailable  FailureCode = "agent_unavailable"
	FailureDelivery          FailureCode = "delivery_failed"
	FailureDeliveryUnknown   FailureCode = "delivery_unknown"
	FailureDeadline          FailureCode = "deadline_exceeded"
	FailureInternal          FailureCode = "internal_error"
	FailureCancelUnconfirmed FailureCode = "cancel_unconfirmed"
	FailureInputTimeout      FailureCode = "input_timeout"
	FailureAuthTimeout       FailureCode = "auth_timeout"
	FailureContextTooLarge   FailureCode = "context_too_large"
)

type Limits struct {
	MaxPromptBytes                                                     int
	MaxCorrelationBytes                                                int
	MaxResponseBytes                                                   int
	MaxOutstanding                                                     int
	MaxRetained                                                        int
	DefaultDeadline                                                    time.Duration
	MaxDeadline                                                        time.Duration
	Retention                                                          time.Duration
	DefaultListPage                                                    int
	MaxListPage                                                        int
	MaxDispatchAttempts                                                int
	MaxProgressBytes, MaxProgressUpdates, MaxResults, MaxResultParts   int
	MaxTextPartBytes, MaxJSONPartBytes, MaxFileBytes, MaxTaskFileBytes int64
	MaxResultNameBytes, MaxDescriptionBytes, MaxFilenameBytes          int
	MaxCancelReasonBytes                                               int
	DefaultCancelDeadline, MaxCancelDeadline                           time.Duration
	MaxInteractions, MaxMessages, MaxInteractionOptions                int
	MaxInteractionQuestionBytes, MaxInteractionResponseBytes           int
	DefaultInteractionExpiry, MaxInteractionExpiry                     time.Duration
}

func DefaultLimits() Limits {
	return Limits{
		MaxPromptBytes: 8 * 1024, MaxCorrelationBytes: 256,
		MaxResponseBytes: 32 * 1024, MaxOutstanding: 8, MaxRetained: 100000,
		DefaultDeadline: 24 * time.Hour, MaxDeadline: 7 * 24 * time.Hour,
		Retention: 7 * 24 * time.Hour, DefaultListPage: 20, MaxListPage: 100,
		MaxDispatchAttempts: 3,
		MaxProgressBytes:    1024, MaxProgressUpdates: 200, MaxResults: 16, MaxResultParts: 8,
		MaxTextPartBytes: 32 * 1024, MaxJSONPartBytes: 64 * 1024,
		MaxFileBytes: 25 * 1024 * 1024, MaxTaskFileBytes: 100 * 1024 * 1024,
		MaxResultNameBytes: 128, MaxDescriptionBytes: 1024, MaxFilenameBytes: 255,
		MaxCancelReasonBytes: 512, DefaultCancelDeadline: 5 * time.Minute, MaxCancelDeadline: time.Hour,
		MaxInteractions: 16, MaxMessages: 64, MaxInteractionOptions: 20,
		MaxInteractionQuestionBytes: 4 * 1024, MaxInteractionResponseBytes: 64 * 1024,
		DefaultInteractionExpiry: 24 * time.Hour, MaxInteractionExpiry: 7 * 24 * time.Hour,
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
		l.MaxDispatchAttempts <= 0 || l.MaxDispatchAttempts > 10 ||
		l.MaxProgressBytes <= 0 || l.MaxProgressBytes > HardMaxProgressBytes ||
		l.MaxProgressUpdates <= 0 || l.MaxProgressUpdates > HardMaxProgressUpdates ||
		l.MaxResults <= 0 || l.MaxResults > HardMaxResults ||
		l.MaxResultParts <= 0 || l.MaxResultParts > HardMaxResultParts ||
		l.MaxTextPartBytes <= 0 || l.MaxTextPartBytes > HardMaxTextPartBytes ||
		l.MaxJSONPartBytes <= 0 || l.MaxJSONPartBytes > HardMaxJSONPartBytes ||
		l.MaxFileBytes <= 0 || l.MaxFileBytes > HardMaxFileBytes ||
		l.MaxTaskFileBytes <= 0 || l.MaxTaskFileBytes > HardMaxTaskFileBytes ||
		l.MaxFileBytes > l.MaxTaskFileBytes || l.MaxResultNameBytes <= 0 || l.MaxResultNameBytes > HardMaxResultNameBytes ||
		l.MaxDescriptionBytes < 0 || l.MaxDescriptionBytes > HardMaxDescriptionBytes ||
		l.MaxFilenameBytes <= 0 || l.MaxFilenameBytes > HardMaxFilenameBytes {
		return ErrInvalid
	}
	if l.MaxCancelReasonBytes <= 0 || l.MaxCancelReasonBytes > HardMaxCancelReasonBytes ||
		l.DefaultCancelDeadline <= 0 || l.MaxCancelDeadline < l.DefaultCancelDeadline || l.MaxCancelDeadline > time.Hour {
		return ErrInvalid
	}
	if l.MaxInteractions <= 0 || l.MaxInteractions > HardMaxTaskInteractions || l.MaxMessages <= 0 || l.MaxMessages > HardMaxTaskMessages ||
		l.MaxInteractionOptions <= 0 || l.MaxInteractionOptions > HardMaxInteractionOptions ||
		l.MaxInteractionQuestionBytes <= 0 || l.MaxInteractionQuestionBytes > HardMaxInteractionQuestionBytes ||
		l.MaxInteractionResponseBytes <= 0 || l.MaxInteractionResponseBytes > HardMaxInteractionResponseBytes ||
		l.DefaultInteractionExpiry <= 0 || l.MaxInteractionExpiry < l.DefaultInteractionExpiry || l.MaxInteractionExpiry > 7*24*time.Hour {
		return ErrInvalid
	}
	return nil
}

type AgentRef struct{ Namespace, Name string }

type Progress struct {
	Message   string    `json:"message"`
	Percent   *int      `json:"percent,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}
type ProgressUpdate struct {
	UpdateID string `json:"updateId"`
	Message  string `json:"message"`
	Percent  *int   `json:"percent,omitempty"`
}
type PartKind string

const (
	PartText PartKind = "text"
	PartJSON PartKind = "json"
	PartFile PartKind = "file"
)

type FileMetadata struct {
	ObjectID   string `json:"-"`
	Filename   string `json:"filename"`
	MediaType  string `json:"mediaType"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size"`
	ScanStatus string `json:"scanStatus"`
}
type ResultPart struct {
	ID   string          `json:"id"`
	Kind PartKind        `json:"kind"`
	Text string          `json:"text,omitempty"`
	JSON json.RawMessage `json:"value,omitempty"`
	File *FileMetadata   `json:"file,omitempty"`
}
type Result struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Description   string       `json:"description,omitempty"`
	ContentDigest string       `json:"-"`
	Parts         []ResultPart `json:"parts"`
	CreatedAt     time.Time    `json:"createdAt"`
}

type Task struct {
	ID, AgentNamespace, AgentName, CreatedBy, Prompt, Correlation string
	State                                                         State
	FailureCode                                                   FailureCode
	Response                                                      string
	Version                                                       int64
	CreatedAt, UpdatedAt, DeadlineAt, RetainUntil                 time.Time
	CompletedAt                                                   *time.Time
	Progress                                                      *Progress
	Results                                                       []Result
	Cancellation                                                  *Cancellation
	Interaction                                                   *Interaction
	Messages                                                      []Message
}

type InteractionType string

const (
	InteractionText          InteractionType = "text"
	InteractionChoice        InteractionType = "choice"
	InteractionConfirm       InteractionType = "confirm"
	InteractionJSON          InteractionType = "json"
	InteractionAuthorization InteractionType = "authorization"
)

type InteractionStatus string

const (
	InteractionPaused   InteractionStatus = "paused"
	InteractionAnswered InteractionStatus = "answered"
	InteractionConsumed InteractionStatus = "consumed"
	InteractionExpired  InteractionStatus = "expired"
)

type InteractionOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}
type Interaction struct {
	ID                string              `json:"id"`
	AttemptID         string              `json:"attemptId"`
	Type              InteractionType     `json:"type"`
	Status            InteractionStatus   `json:"status"`
	Question          string              `json:"question"`
	Options           []InteractionOption `json:"options,omitempty"`
	Schema            json.RawMessage     `json:"schema,omitempty"`
	AuthorizationFlow string              `json:"authorizationFlow,omitempty"`
	Response          json.RawMessage     `json:"response,omitempty"`
	CreatedAt         time.Time           `json:"createdAt"`
	ExpiresAt         time.Time           `json:"expiresAt"`
	AnsweredAt        *time.Time          `json:"answeredAt,omitempty"`
}
type Message struct {
	Sequence  int64           `json:"sequence"`
	Role      string          `json:"role"`
	Kind      string          `json:"kind"`
	Text      string          `json:"text,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
}
type RequestInteractionParams struct {
	Agent                            AgentRef
	TaskID, AttemptID, InteractionID string
	Type                             InteractionType
	Question                         string
	Options                          []InteractionOption
	Schema                           json.RawMessage
	AuthorizationFlow                string
	ExpiresIn                        time.Duration
}
type RespondInteractionParams struct {
	Agent                                                           AgentRef
	TaskID, InteractionID, RespondedBy, IdempotencyKey, RequestHash string
	Response                                                        json.RawMessage
}
type InteractionResult struct {
	Task   *Task
	Replay bool
}

type Cancellation struct {
	RequestedAt    time.Time  `json:"requestedAt"`
	RequestedBy    string     `json:"requestedBy"`
	Reason         string     `json:"reason,omitempty"`
	DeadlineAt     time.Time  `json:"deadlineAt"`
	AcknowledgedAt *time.Time `json:"acknowledgedAt,omitempty"`
	AckSource      string     `json:"ackSource,omitempty"`
	Status         string     `json:"status"`
	Scope          string     `json:"scope"`
}

type CancelParams struct {
	Agent                                                    AgentRef
	TaskID, RequestedBy, Reason, IdempotencyKey, RequestHash string
}

type CancelResult struct {
	Task            *Task
	Applied, Replay bool
}

type TaskControl struct {
	CancelRequested bool      `json:"cancel_requested"`
	Reason          string    `json:"reason,omitempty"`
	RequestedAt     time.Time `json:"requested_at,omitempty"`
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
	ReportProgress(context.Context, AgentRef, string, string, ProgressUpdate) (*Progress, bool, error)
	PublishResult(context.Context, AgentRef, string, string, Result) (*Result, bool, error)
	Cancel(context.Context, CancelParams) (*CancelResult, error)
	GetControl(context.Context, AgentRef, string, string) (*TaskControl, error)
	AcknowledgeCancel(context.Context, AgentRef, string, string, string, string) (*Task, bool, error)
	RequestInteraction(context.Context, RequestInteractionParams) (*Task, error)
	RespondInteraction(context.Context, RespondInteractionParams) (*InteractionResult, error)
	Reject(context.Context, AgentRef, string, string, string) (*Task, error)
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

type ReconcileResult struct {
	RequeuedLeases, UnknownAttempts, ExpiredTasks, ExpiredInteractions, CancelUnconfirmed, ClosedCancellations, DeletedTasks int64
}

type ObjectDeletion struct {
	ID         string
	ObjectKey  string
	LeaseOwner string
}

type PendingFile struct {
	ObjectID, ResultID, Name, Filename, MediaType string
	SizeBytes                                     int64
}

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
	PrepareFileUpload(context.Context, AgentRef, string, string, PendingFile) error
	AbandonFileUpload(context.Context, string) error
	Reconcile(context.Context, int) (*ReconcileResult, error)
	ClaimObjectDeletions(context.Context, string, time.Duration, int) ([]ObjectDeletion, error)
	DeleteObjectRecord(context.Context, string, string) error
	RetryObjectDeletion(context.Context, string, string, string) error
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
	if t.Cancellation != nil {
		v := *t.Cancellation
		if t.Cancellation.AcknowledgedAt != nil {
			a := *t.Cancellation.AcknowledgedAt
			v.AcknowledgedAt = &a
		}
		c.Cancellation = &v
	}
	if t.Progress != nil {
		v := *t.Progress
		if t.Progress.Percent != nil {
			p := *t.Progress.Percent
			v.Percent = &p
		}
		c.Progress = &v
	}
	c.Results = cloneResults(t.Results)
	if t.Interaction != nil {
		v := *t.Interaction
		v.Options = append([]InteractionOption(nil), v.Options...)
		v.Schema = append(json.RawMessage(nil), v.Schema...)
		v.Response = append(json.RawMessage(nil), v.Response...)
		c.Interaction = &v
	}
	c.Messages = append([]Message(nil), t.Messages...)
	return &c
}

func cloneResults(in []Result) []Result {
	out := make([]Result, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Parts = append([]ResultPart(nil), in[i].Parts...)
		for j := range out[i].Parts {
			out[i].Parts[j].JSON = append(json.RawMessage(nil), in[i].Parts[j].JSON...)
			if in[i].Parts[j].File != nil {
				v := *in[i].Parts[j].File
				out[i].Parts[j].File = &v
			}
		}
	}
	return out
}
