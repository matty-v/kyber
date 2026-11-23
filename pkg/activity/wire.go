package activity

import "time"

// ObserverIdentity is purpose separated from the runtime's ordinary pod token.
func ObserverIdentity(name, agentUID string) string {
	return "activity-observer:" + name + ":" + agentUID
}

// Registration contains metadata supplied by the platform sidecar, never by a
// runtime event. The API issues an epoch only after checking current ownership
// and the native sidecar container lifetime through a direct Kubernetes read.
type Registration struct {
	PodUID   string    `json:"podUID"`
	BootID   string    `json:"bootID"`
	BootedAt time.Time `json:"bootedAt"`
}
type RegistrationResponse struct {
	Epoch    string `json:"epoch"`
	Sequence int64  `json:"sequence"`
}

// Snapshot contains counts only: no commands, handles, output, or credentials.
type Snapshot struct {
	ObservedAt   time.Time `json:"observedAt"`
	LastWorkedAt time.Time `json:"lastWorkedAt"`
	Foreground   int32     `json:"foreground"`
	Background   int32     `json:"background"`
	Complete     bool      `json:"complete"`
	NeedsInput   bool      `json:"needsInput"`
}
type Report struct {
	Registration
	Epoch    string `json:"epoch"`
	Sequence int64  `json:"sequence"`
	Snapshot
}

func (s Snapshot) Valid(now time.Time) bool {
	return !s.ObservedAt.IsZero() && !s.ObservedAt.After(now) && now.Sub(s.ObservedAt) < 2*ObservationInterval &&
		!s.LastWorkedAt.After(s.ObservedAt) && s.Foreground >= 0 && s.Foreground <= MaxJobs && s.Background >= 0 && s.Background <= MaxJobs
}
