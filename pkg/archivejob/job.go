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
	// KindExport archives an existing agent's volume (MAT-87).
	KindExport Kind = "export"
	// KindUpload stages an operator-supplied archive so it can be imported,
	// which is how an archive moves between installations (MAT-88).
	KindUpload Kind = "upload"
	// KindImport creates a new agent from a completed export or upload.
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
	// Source is the manifest's source description, captured when the
	// export starts so a recreated export pod writes the same one.
	Source *diskarchive.Source `json:"source,omitempty"`
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
	// UploadClaimed is set, with the version check, before an upload reads
	// its body, so a job accepts exactly one upload attempt.
	UploadClaimed bool `json:"uploadClaimed,omitempty"`
	// ExportPodCreated is set once the export pod exists, so a restart
	// before that point recreates it instead of failing the export.
	ExportPodCreated bool `json:"exportPodCreated,omitempty"`
	Uploaded         bool `json:"uploaded,omitempty"`
	// An upload received in parts: the declared plaintext size, the part
	// size, which parts are stored, and when the last one arrived. Zero
	// PartSize means the archive arrived as one request body.
	UploadSize    int64      `json:"uploadSize,omitempty"`
	PartSize      int64      `json:"partSize,omitempty"`
	PartsReceived []int      `json:"partsReceived,omitempty"`
	LastPartAt    *time.Time `json:"lastPartAt,omitempty"`
	// VerifyStarted and Verified track the verification pod; Verified is set
	// when it has posted a summary of a fully checked archive.
	VerifyStarted bool `json:"verifyStarted,omitempty"`
	Verified      bool `json:"verified,omitempty"`

	// Finishing is the terminal state a job is being cleaned up towards.
	// finish saves it before releasing anything, so a concurrent writer or a
	// restart can never leave cleanup half done and unrecorded.
	Finishing    State  `json:"finishing,omitempty"`
	FinishReason string `json:"finishReason,omitempty"`

	// Summary is the verified manifest without its per-entry list, which can
	// run to millions of records and stays inside the archive.
	Summary *Summary `json:"summary,omitempty"`

	// Import-only fields. Agent is the new agent's name.
	SourceJobID string `json:"sourceJobID,omitempty"`
	Machine     string `json:"machine,omitempty"`
	// Skip lists archived files the restore leaves out (by default the
	// source harness's own credential files).
	Skip []string `json:"skip,omitempty"`
	// Cutover lists what the operator must do by hand while the source and
	// the new agent coexist.
	Cutover []string `json:"cutover,omitempty"`
	// Restored is set once the volume is complete and verified. From then on
	// the new agent is kept even if the job fails, because it is whole.
	Restored bool `json:"restored,omitempty"`
	// RestoreStarted is set once the restore pod exists.
	RestoreStarted bool `json:"restoreStarted,omitempty"`
	// CreatedSecrets are the Secrets the create path made for the new agent,
	// removed with it if the import is abandoned before it is whole.
	CreatedSecrets []string `json:"createdSecrets,omitempty"`
	// KeepCredentialFiles and KeepCrontabs restore those files instead of
	// leaving them out (the default).
	KeepCredentialFiles bool `json:"keepCredentialFiles,omitempty"`
	KeepCrontabs        bool `json:"keepCrontabs,omitempty"`
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
	// Cron lists crontabs the agent installed itself; a restored copy runs
	// them as well.
	Cron []string `json:"cron,omitempty"`
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
		Cron:          diskarchive.CronPaths(m),
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
	// Create inserts a job, refusing a second active job of the same kind
	// for the same named agent (jobs with no agent, such as uploads, are not
	// limited per agent), or more than maxActive active jobs overall (zero
	// means unbounded).
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
