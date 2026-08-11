package updates

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultCadence is how often the checker polls the release feed. Hourly is
// far more often than Kyber releases, and stays well inside GitHub's
// unauthenticated rate limit (60/hour/IP) even with several clusters behind
// one address.
const DefaultCadence = time.Hour

// Status is the full answer to "where is this cluster, and what about it?".
// It is what GET /api/v1/updates serves.
type Status struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion,omitempty"`
	LatestURL      string `json:"latestUrl,omitempty"`

	// UpdateAvailable is true only when a newer version was positively
	// identified. A failed check leaves it false — "we don't know" must never
	// render as "an update is waiting".
	UpdateAvailable bool `json:"updateAvailable"`

	Policy Policy `json:"policy"`

	// ManagedBy and CanSelfUpgrade tell the UI whether to offer an install
	// button at all, and Reason gives it something honest to display instead.
	ManagedBy      ManagedBy `json:"managedBy"`
	CanSelfUpgrade bool      `json:"canSelfUpgrade"`
	Reason         string    `json:"reason,omitempty"`

	// LastChecked is when the feed was last read successfully. Zero means
	// never — including when every attempt so far has failed.
	LastChecked time.Time `json:"lastChecked,omitempty"`
	// LastError is the most recent check failure, cleared on success. The UI
	// shows it so a cluster that has quietly stopped checking is visible
	// rather than looking permanently up to date.
	LastError string `json:"lastError,omitempty"`

	// ApplySupported is false in this build: the checker reports, it never
	// installs. Explicit in the contract so the PWA can render the right
	// affordance instead of inferring it from a missing endpoint.
	ApplySupported bool `json:"applySupported"`
}

// Checker polls the release feed and caches the answer. It never mutates the
// cluster — the whole apply path is a separate concern that does not exist
// yet.
//
// Runs as a controller-runtime Runnable: mgr.Add(manager.RunnableFunc(c.Start)).
type Checker struct {
	// Feed reads published releases. Required.
	Feed *FeedClient
	// Store loads the operator's policy. Required.
	Store *Store
	// K8sClient is used for ownership detection. Optional; nil reports
	// ManagedByUnknown.
	K8sClient client.Client
	// Namespace and ControlPlaneDeployment locate the Deployment whose
	// ownership annotations decide whether self-upgrade is possible.
	Namespace              string
	ControlPlaneDeployment string
	// CurrentVersion is this build's version, as reported by /api/v1/version.
	CurrentVersion string
	// Cadence between polls. <= 0 uses DefaultCadence.
	Cadence time.Duration
	// Logger receives soft-failure warnings. Nil uses slog.Default.
	Logger *slog.Logger

	mu     sync.RWMutex
	status Status
	seeded bool
}

func (c *Checker) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c *Checker) cadence() time.Duration {
	if c.Cadence > 0 {
		return c.Cadence
	}
	return DefaultCadence
}

// Status returns the cached answer. Safe for concurrent use.
//
// Before the first poll completes it still returns a usable value — current
// version and policy — with UpdateAvailable false. A cluster that has just
// booted should render "checking" rather than a blank card.
func (c *Checker) Status(ctx context.Context) Status {
	c.mu.RLock()
	s, seeded := c.status, c.seeded
	c.mu.RUnlock()
	if seeded {
		// Policy and ownership are cheap to re-read and can change between
		// polls (an operator edits the policy; a migration changes ownership).
		// Re-reading them here keeps the card honest without waiting an hour.
		s.Policy = c.loadPolicy(ctx, s.Policy)
		s.ManagedBy = c.detectOwner(ctx)
		s.CanSelfUpgrade = s.ManagedBy.CanSelfUpgrade()
		s.Reason = c.reasonFor(s.ManagedBy, s.Policy)
		return s
	}
	base := Status{
		CurrentVersion: c.CurrentVersion,
		Policy:         c.loadPolicy(ctx, DefaultPolicy()),
		ApplySupported: false,
	}
	base.ManagedBy = c.detectOwner(ctx)
	base.CanSelfUpgrade = base.ManagedBy.CanSelfUpgrade()
	base.Reason = c.reasonFor(base.ManagedBy, base.Policy)
	return base
}

func (c *Checker) loadPolicy(ctx context.Context, fallback Policy) Policy {
	if c.Store == nil {
		return fallback
	}
	p, err := c.Store.Load(ctx)
	if err != nil {
		c.logger().Warn("update policy read failed; reporting the last known policy", "error", err)
		return fallback
	}
	return p
}

func (c *Checker) detectOwner(ctx context.Context) ManagedBy {
	return DetectManagedBy(ctx, c.K8sClient, c.Namespace, c.ControlPlaneDeployment)
}

// reasonFor explains why nothing will be installed. A pin outranks ownership:
// a pinned cluster is not going to move regardless of who manages it, and
// saying so is more useful than explaining ArgoCD to someone who chose the pin.
func (c *Checker) reasonFor(m ManagedBy, p Policy) string {
	if p.PinnedVersion != "" {
		return "This cluster is held at " + p.PinnedVersion + ". Clear the hold to take updates."
	}
	return m.Reason()
}

// Check performs one poll and updates the cache. Exported so the API can offer
// an on-demand "check now" without waiting for the next tick.
//
// A failed check records the error and preserves the previous good answer. It
// never clears LatestVersion: losing a known-good result on a transient network
// blip would make the card flap between "update available" and "up to date".
func (c *Checker) Check(ctx context.Context) Status {
	policy := c.loadPolicy(ctx, DefaultPolicy())
	owner := c.detectOwner(ctx)

	c.mu.Lock()
	next := c.status
	c.mu.Unlock()

	next.CurrentVersion = c.CurrentVersion
	next.Policy = policy
	next.ManagedBy = owner
	next.CanSelfUpgrade = owner.CanSelfUpgrade()
	next.Reason = c.reasonFor(owner, policy)
	next.ApplySupported = false

	rel, err := c.Feed.Latest(ctx)
	switch {
	case err != nil:
		next.LastError = err.Error()
		c.logger().Warn("update check failed", "error", err)
	case rel == nil:
		// No published release. Not an error; a fresh fork looks like this.
		next.LastError = ""
		next.LastChecked = time.Now().UTC()
		next.UpdateAvailable = false
	default:
		next.LastError = ""
		next.LastChecked = time.Now().UTC()
		next.LatestVersion = rel.Version.String()
		next.LatestURL = rel.URL
		next.UpdateAvailable = c.isNewer(rel.Version, policy)
	}

	c.mu.Lock()
	c.status = next
	c.seeded = true
	c.mu.Unlock()
	return next
}

// isNewer decides whether to flag the release as an available update.
//
// A pinned cluster never reports one: the operator asked it to hold, and a
// notification it has chosen to refuse is just noise.
//
// An unparseable CURRENT version reports no update rather than assuming we are
// behind. Local and dev builds have no injected version, and telling a
// developer their laptop is out of date every hour would train them to ignore
// the card that production depends on.
func (c *Checker) isNewer(latest Version, policy Policy) bool {
	if policy.PinnedVersion != "" {
		return false
	}
	current, err := ParseVersion(c.CurrentVersion)
	if err != nil {
		c.logger().Warn("current version is not strict X.Y.Z; not reporting an update",
			"currentVersion", c.CurrentVersion, "error", err)
		return false
	}
	return latest.GreaterThan(current)
}

// Start runs the poll loop until ctx is cancelled. Conforms to
// controller-runtime's manager.RunnableFunc signature.
//
// The first poll fires immediately so the card is populated within seconds of
// startup rather than after a full cadence. The loop never exits on a check
// error — a repository that is unreachable for an hour must not leave the
// cluster permanently unable to notice a release.
func (c *Checker) Start(ctx context.Context) error {
	logger := c.logger()
	logger.Info("update checker started",
		"cadence", c.cadence().String(),
		"repo", c.Feed.repo(),
		"currentVersion", c.CurrentVersion)

	c.Check(ctx)

	ticker := time.NewTicker(c.cadence())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("update checker stopped")
			return nil
		case <-ticker.C:
			c.Check(ctx)
		}
	}
}
