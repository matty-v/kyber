package taskdispatch

import (
	"context"

	"github.com/matty-v/kyber/pkg/taskstore"
)

// TaskCancellationMode is a capability claim, not a best-effort guess.
// Current Claude Code and Codex TUI integrations are NotifyOnly.
type TaskCancellationMode string

const (
	NotifyOnly     TaskCancellationMode = "notify_only"
	ExactInterrupt TaskCancellationMode = "exact_interrupt"
)

type CancelReceipt struct {
	TaskID, AttemptID, AcknowledgmentID string
}

// TaskCancellationAdapter may claim ExactInterrupt only when it targets the
// opaque receipt's exact native turn and returns structured terminal evidence
// for that same turn. Generic terminal keys and process signals cannot satisfy
// this interface.
type TaskCancellationAdapter interface {
	CancellationMode() TaskCancellationMode
	RequestCancellation(context.Context, taskstore.Receipt) (CancelReceipt, error)
}
