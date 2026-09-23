// Package archivejob persists the asynchronous agent-disk export (MAT-87) and
// import (MAT-88) jobs. A job row is the only record of what a job has done:
// the worker that drives it re-reads the row at every step and writes each
// transition with an optimistic version check, so a control-plane restart or
// a second replica can never apply a stale step.
package archivejob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/matty-v/kyber/pkg/diskarchive"
)

// Kind distinguishes exports from imports.
type Kind string

const (
	KindExport Kind = "export"
	KindImport Kind = "import"
)

// State is a job's lifecycle position. Terminal states never change again
// except Completed → Expired when retention lapses.
type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateCanceled  State = "canceled"
	StateExpired   State = "expired"
)

// Terminal reports whether s is final.
func (s State) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCanceled, StateExpired:
		return true
	}
	return false
}

// ActiveStates are the states that count against concurrency limits.
var ActiveStates = []State{StateQueued, StateRunning}

// Step is the worker's position inside StateRunning. It is persisted so a
// restarted worker resumes (or safely fails) from where the last one stopped.
type Step string

const (
	// Export steps.
	StepPausing   Step = "pausing"
	StepArchiving Step = "archiving"
	StepVerifying Step = "verifying"
	// Import steps.
	StepReceiving Step = "receiving"
	StepReady     Step = "ready"
	StepRestoring Step = "restoring"
	StepStarting  Step = "starting"
)

var (
	ErrNotFound    = errors.New("archivejob: job not found")
	ErrConflict    = errors.New("archivejob: job changed concurrently")
	ErrActiveJob   = errors.New("archivejob: agent already has an active job of this kind")
	ErrConcurrency = errors.New("archivejob: installation-wide job limit reached")
)

// Job is one export or import.
type Job struct {
	ID      string `json:"id"`
	Kind    Kind   `json:"kind"`
	Agent   string `json:"agent"`
	State   State  `json:"state"`
	Step    Step   `json:"step,omitempty"`
	Version int64  `json:"version"`

	Message    string `json:"message,omitempty"`
	Error      string `json:"error,omitempty"`
	BytesDone  int64  `json:"bytesDone,omitempty"`
	BytesTotal int64  `json:"bytesTotal,omitempty"`

	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	// Deadline bounds the whole running phase; past it the worker fails the
	// job and releases the agent.
	Deadline *time.Time `json:"deadline,omitempty"`

	CancelRequested bool   `json:"cancelRequested,omitempty"`
	RequestedBy     string `json:"requestedBy,omitempty"`

	// Hold and pause bookkeeping (export) — the agent's desiredPhase before
	// the job stopped it, and whether the job placed its hold annotation.
	AgentUID          string `json:"agentUID,omitempty"`
	PriorDesiredPhase string `json:"priorDesiredPhase,omitempty"`
	PausedByJob       bool   `json:"pausedByJob,omitempty"`
	HoldApplied       bool   `json:"holdApplied,omitempty"`
	NodeName          string `json:"nodeName,omitempty"`
	// Mounts is the source pod's filesystem inventory, captured before the
	// pause so it reflects the pod the agent actually ran.
	Mounts []diskarchive.Mount `json:"mounts,omitempty"`

	// Stored bytes. WrappedKey is the per-archive data key encrypted under
	// the installation's key-encryption key; it is never returned by the API.
	ObjectKey  string `json:"objectKey,omitempty"`
	SealedSize int64  `json:"sealedSize,omitempty"`
	PlainSize  int64  `json:"plainSize,omitempty"`
	WrappedKey []byte `json:"wrappedKey,omitempty"`
	// TokenHash is the SHA-256 of the per-job pod upload/download token.
	TokenHash string `json:"tokenHash,omitempty"`
	Uploaded  bool   `json:"uploaded,omitempty"`

	// Summary is the verified manifest without its per-entry list, which can
	// run to millions of records and stays inside the archive.
	Summary *Summary `json:"summary,omitempty"`

	// Import-only fields.
	SourceJobID string         `json:"sourceJobID,omitempty"`
	Create      *CreateRequest `json:"create,omitempty"`
}

// Summary is the operator-visible part of a manifest.
type Summary struct {
	FormatVersion string                  `json:"formatVersion"`
	ExportedAt    time.Time               `json:"exportedAt"`
	Source        diskarchive.Source      `json:"source"`
	Mounts        []diskarchive.Mount     `json:"mounts"`
	Excluded      []diskarchive.Exclusion `json:"excluded,omitempty"`
	Totals        diskarchive.Totals      `json:"totals"`
	Sensitive     []string                `json:"sensitive,omitempty"`
}

// CreateRequest is the destination an import restores into.
type CreateRequest struct {
	Name    string `json:"name"`
	Machine string `json:"machine"`
	Runtime string `json:"runtime"`
	Model   string `json:"model,omitempty"`
	Disk    string `json:"disk"`
	CPU     string `json:"cpu,omitempty"`
	Memory  string `json:"memory,omitempty"`
}

// SummaryOf strips a manifest down to its summary.
func SummaryOf(m *diskarchive.Manifest) *Summary {
	return &Summary{
		FormatVersion: m.FormatVersion,
		ExportedAt:    m.ExportedAt,
		Source:        m.Source,
		Mounts:        m.Mounts,
		Excluded:      m.Excluded,
		Totals:        m.Totals,
		Sensitive:     diskarchive.SensitivePaths(m),
	}
}

// NewID returns a random job ID that is safe in object keys and URLs.
func NewID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Store persists jobs.
type Store interface {
	// Create inserts a queued job, refusing a second active job of the same
	// kind for the same agent, or more than maxActive active jobs overall
	// (zero means unbounded).
	Create(ctx context.Context, j *Job, maxActive int) error
	Get(ctx context.Context, id string) (*Job, error)
	// List returns an agent's jobs of a kind, newest first; agent "" lists all.
	List(ctx context.Context, kind Kind, agent string, limit int) ([]*Job, error)
	// ListActive returns every non-terminal job plus completed jobs whose
	// retention has lapsed, for the worker to drive.
	ListActive(ctx context.Context, now time.Time) ([]*Job, error)
	// Update writes j if its Version still matches the stored row, then
	// increments j.Version.
	Update(ctx context.Context, j *Job) error
}
