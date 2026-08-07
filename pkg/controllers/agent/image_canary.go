package agent

import (
	"sync"
	"time"
)

// imageCanaryTracker is the kyber#371 observed-evidence canary FSM,
// factored image-agnostic (kyber#529, Obi-wan's Option B) so both the
// status-sidecar image roll (convergeSidecarImage) and the runtime-image
// roll (shouldRollRuntimeImage) gate on identical machinery while keeping
// their image namespaces isolated. The FSM is keyed purely on an image
// reference string plus a "some pod was observed Ready on it" predicate;
// nothing about it is sidecar- or runtime-specific.
//
// FSM per (controller process, image string):
//
//	unknown ──first eligible roll──▶ CANARY IN FLIGHT
//	  │                                │            │
//	  │  any pod observed Ready        │  window    │  window elapses
//	  │  on the image                  │  Ready     │  without Ready
//	  ▼                                ▼            ▼
//	VERIFIED  ◀──── Ready observed ───┘          FAILED
//	(steady-state rolls)                       (rolls held)
//
// The window itself is NOT held here — callers own the window duration
// (canaryWindow / runtimeCanaryWindow) so the same tracker serves two
// independently-tunable knobs. All three maps are lazy-initialized; the
// zero value is a ready-to-use tracker (tests construct one directly).
// State is re-initialized on controller restart, which intentionally
// re-arms the canary against the current pin (covers
// env-was-bad-then-good across a process restart).
type imageCanaryTracker struct {
	mu sync.Mutex

	// verified maps an image reference string → true once any pod has
	// been observed Ready on that image in this controller process.
	// Ever-true within the process lifetime.
	verified map[string]bool

	// started records the wall-clock time the FIRST eligible roll was
	// committed against a given image — that pod IS the canary.
	// Subsequent rolls defer until the window elapses (canary failed)
	// or the image is verified (canary succeeded).
	started map[string]time.Time

	// failed marks images whose canary window elapsed without any Ready
	// observation. Rolls against a failed image are permanently held
	// until controller restart or an operator hot-fix (which produces a
	// new image string and re-arms the FSM).
	failed map[string]bool
}

// markVerified records that some pod has been observed Ready on image.
// Idempotent. Clears any in-flight/failed canary bookkeeping for the
// same image — once we have a Ready pod we know the image is pullable
// and prior canary state is irrelevant.
func (c *imageCanaryTracker) markVerified(image string) {
	if image == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.verified == nil {
		c.verified = make(map[string]bool)
	}
	c.verified[image] = true
	delete(c.started, image)
	delete(c.failed, image)
}

// wasVerified reports whether any pod has been observed Ready on image
// during this controller process's lifetime.
func (c *imageCanaryTracker) wasVerified(image string) bool {
	if image == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.verified[image]
}

// markCanaryStarted records the first eligible roll against image.
// Subsequent calls for the same image are no-ops (the original start
// time is the canary clock).
func (c *imageCanaryTracker) markCanaryStarted(image string) {
	if image == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started == nil {
		c.started = make(map[string]time.Time)
	}
	if _, ok := c.started[image]; !ok {
		c.started[image] = time.Now()
	}
}

// canaryInFlight returns the canary start time and whether one is
// recorded for image.
func (c *imageCanaryTracker) canaryInFlight(image string) (time.Time, bool) {
	if image == "" {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.started[image]
	return t, ok
}

// markCanaryFailed marks image as canary-failed; future rolls against
// this image are permanently held until controller restart or an env
// hot-fix produces a new image string.
func (c *imageCanaryTracker) markCanaryFailed(image string) {
	if image == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed == nil {
		c.failed = make(map[string]bool)
	}
	c.failed[image] = true
}

// failedCanary reports whether image's canary window elapsed without a
// Ready observation.
func (c *imageCanaryTracker) failedCanary(image string) bool {
	if image == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failed[image]
}
