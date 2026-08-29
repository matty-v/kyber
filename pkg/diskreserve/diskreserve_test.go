package diskreserve_test

import (
	"testing"

	"github.com/matty-v/kyber/pkg/diskreserve"
)

func TestDecide(t *testing.T) {
	const total = 1000
	cases := []struct {
		name     string
		previous bool
		used     int64
		state    string
		want     bool
	}{
		// Trip and clear, from either starting point.
		{"at the trip ratio", false, 900, "ready", true},
		{"above the trip ratio", false, 950, "ready", true},
		{"at the clear ratio", true, 800, "ready", false},
		{"below the clear ratio", true, 750, "ready", false},

		// The hysteresis band holds whatever was decided before, so an agent
		// hovering just under the trip point neither flaps nor resumes early.
		{"inside the band, previously exhausted", true, 850, "ready", true},
		{"inside the band, previously healthy", false, 850, "ready", false},

		// partial is a lower bound. It gets the same authority as ready: see
		// ClearRatio for why refusing it the clearing half made DiskExhausted
		// permanent rather than making it safe.
		{"partial above the trip ratio", false, 950, "partial", true},
		{"partial below the clear ratio", true, 750, "partial", false},
		{"partial inside the band", true, 850, "partial", true},

		// A sample that measured nothing must not move the lifecycle.
		{"pending holds exhausted", true, 0, "pending", true},
		{"pending holds healthy", false, 0, "pending", false},
		{"error holds exhausted", true, 0, "error", true},
		{"error holds healthy", false, 950, "error", false},
		{"unknown state holds", true, 10, "something-new", true},

		// A missing allocation cannot be reasoned about as a ratio at all.
		{"zero allocation", true, 950, "ready", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capacity := int64(total)
			if tc.name == "zero allocation" {
				capacity = 0
			}
			if got := diskreserve.Decide(tc.previous, tc.used, capacity, tc.state); got != tc.want {
				t.Fatalf("Decide(previous=%v, used=%d, total=%d, state=%q) = %v, want %v",
					tc.previous, tc.used, capacity, tc.state, got, tc.want)
			}
		})
	}
}

// The gap between the two ratios is the whole mechanism. If they are ever set
// equal the band disappears and the signal can flap on every sample; if they
// invert, the decision becomes incoherent.
func TestClearRatioLeavesAHysteresisBand(t *testing.T) {
	if diskreserve.ClearRatio >= diskreserve.TripRatio {
		t.Fatalf("ClearRatio %v must sit below TripRatio %v", diskreserve.ClearRatio, diskreserve.TripRatio)
	}
}

// An agent that fills up, is cleaned out, and fills up again must pause, resume
// and pause again. Before hysteresis existed the middle step was impossible on
// a partial walk, which is every agent with rootfs persistence.
func TestExhaustRecoverExhaustCycle(t *testing.T) {
	var total int64 = 2 << 30
	reached := false

	reached = diskreserve.Decide(reached, int64(float64(total)*0.98), total, "partial")
	if !reached {
		t.Fatal("a volume at 98% did not pause the agent")
	}
	reached = diskreserve.Decide(reached, int64(float64(total)*0.30), total, "partial")
	if reached {
		t.Fatal("a volume cleaned back to 30% left the agent paused — this is the canary failure")
	}
	reached = diskreserve.Decide(reached, int64(float64(total)*0.95), total, "partial")
	if !reached {
		t.Fatal("a volume that filled again did not pause the agent a second time")
	}
}
