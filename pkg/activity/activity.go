// Package activity defines the shared, runtime-independent work inventory
// contract. Contact with a sidecar is not evidence of current work.
package activity

import "time"

const (
	ObservationInterval = 15 * time.Second
	WorkLease           = 60 * time.Second
	ContactLease        = 90 * time.Second
	RecentWindow        = 24 * time.Hour
	MaxJobs             = 1024
)

type State string

const (
	Working     State = "working"
	Background  State = "background"
	Waiting     State = "waiting"
	Unavailable State = "unavailable"
	Excluded    State = "excluded"
)

// Inventory is a native reporter's complete current snapshot, not an event
// replay or a guess from process CPU, file mtime or an old turn-start marker.
type Inventory struct {
	ObservedAt   time.Time
	LastWorkedAt time.Time
	Foreground   int
	Background   int
	Complete     bool
	NeedsInput   bool
}

type Input struct {
	Running     bool
	StopPending bool
	ContactAt   time.Time
	Inventory   *Inventory
}

type View struct {
	State        State
	Reason       string
	ExpiresAt    time.Time
	LastWorkedAt time.Time
	Foreground   int
	Background   int
	NeedsInput   bool
}

// Project applies lifecycle and telemetry precedence before classifying work.
// It never treats a missing or invalid inventory as confirmed idle.
func Project(in Input, now time.Time) View {
	if !in.Running || in.StopPending {
		return View{State: Excluded, Reason: "lifecycle"}
	}
	if in.ContactAt.IsZero() {
		return View{State: Unavailable, Reason: "contact_missing"}
	}
	if in.ContactAt.After(now) || !now.Before(in.ContactAt.Add(ContactLease)) {
		return View{State: Unavailable, Reason: "contact_stale"}
	}
	i := in.Inventory
	if i == nil {
		return View{State: Unavailable, Reason: "legacy_reporting"}
	}
	if !i.Complete {
		return View{State: Unavailable, Reason: "inventory_incomplete"}
	}
	if i.ObservedAt.IsZero() || i.ObservedAt.After(now) || !now.Before(i.ObservedAt.Add(WorkLease)) {
		return View{State: Unavailable, Reason: "reporter_stale"}
	}
	if i.Foreground < 0 || i.Background < 0 || i.Foreground > MaxJobs || i.Background > MaxJobs || i.LastWorkedAt.After(i.ObservedAt) {
		return View{State: Unavailable, Reason: "inventory_invalid"}
	}
	v := View{LastWorkedAt: i.LastWorkedAt, Foreground: i.Foreground, Background: i.Background, NeedsInput: i.NeedsInput, ExpiresAt: i.ObservedAt.Add(WorkLease)}
	if expiry := in.ContactAt.Add(ContactLease); expiry.Before(v.ExpiresAt) {
		v.ExpiresAt = expiry
	}
	switch {
	case i.Foreground > 0:
		v.State = Working
	case i.Background > 0:
		v.State = Background
	case i.NeedsInput:
		v.State = Waiting
	case i.LastWorkedAt.IsZero() || !now.Before(i.LastWorkedAt.Add(RecentWindow)):
		v.State = Excluded
		v.Reason = "outside_history"
	default:
		v.State = Waiting
	}
	return v
}
