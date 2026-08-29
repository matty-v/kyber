// Package diskreserve holds the single decision that drives an agent's
// DiskExhausted maintenance lifecycle.
//
// It exists because that decision is made in two places — the in-pod status
// sidecar, which writes the marker that pauses the runtime session, and the
// control plane, which owns the Agent phase. When those two disagree the agent
// is either paused while reporting Running or running while reporting paused,
// so they must run identical arithmetic against identical thresholds.
package diskreserve

// TripRatio is the fraction of an agent's disk allocation at which the
// DiskExhausted lifecycle engages.
const TripRatio = 0.90

// ClearRatio is the fraction usage must fall back to before it releases.
//
// The gap between the two is deliberate hysteresis, and it is what makes
// recovery possible at all. Directory accounting on an agent with rootfs
// persistence permanently reports `partial`: the sidecar runs unprivileged and
// cannot enumerate the root-owned directories seeded into /persist/agentroot,
// so a complete `ready` scan never happens on a real agent.
//
// A partial figure is a lower bound. That makes it sound evidence FOR
// exhaustion — true usage is at least what was counted — and unsound evidence
// against it, which is why clearing was originally refused outright. But the
// two errors are not comparable. Releasing early costs one cycle: the next
// sample re-trips, five minutes later, and the agent pauses again. Never
// releasing costs the agent permanently, with no operator recourse — deleting
// files, restarting, and growing the volume were all verified not to recover a
// stuck agent on kyber-canary.
//
// The band also reflects what the unreadable remainder actually is: system
// directories seeded once and never written by the agent. Their size is
// effectively constant over the volume's life, so while it makes the absolute
// figure an undercount, it cancels out of any comparison between two samples of
// the same volume — and recovery detection is exactly such a comparison.
const ClearRatio = 0.80

// Decide reports whether an agent's disk reserve should be considered reached.
//
// previous is the last decision, which is what the hysteresis band holds and
// what a sample with no measurement authority carries forward unchanged.
// state is the sidecar's DiskUsageState: only "ready" and "partial" carry a
// usable figure. "pending" and "error" mean no walk has completed, and a sample
// that measured nothing must not be able to move the lifecycle in either
// direction.
func Decide(previous bool, usedBytes, totalBytes int64, state string) bool {
	if totalBytes <= 0 {
		return false
	}
	if state != "ready" && state != "partial" {
		return previous
	}
	switch ratio := float64(usedBytes) / float64(totalBytes); {
	case ratio >= TripRatio:
		return true
	case ratio <= ClearRatio:
		return false
	default:
		return previous
	}
}
