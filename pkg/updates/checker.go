package updates

import (
	"context"
	"fmt"
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

	// ApplySupported reports whether POST /api/v1/updates/apply will do
	// anything on this cluster. False when the control plane has no applier
	// configured (self-upgrade disabled in the chart). Explicit in the
	// contract so the PWA renders the right affordance instead of inferring it
	// from a missing endpoint.
	//
	// Note this is about the CONTROL PLANE's capability, not this cluster's
	// permission — an ArgoCD-managed cluster reports applySupported:true and
	// canSelfUpgrade:false, because the button exists but this cluster may not
	// press it. Collapsing the two would tell an operator the feature does not
	// exist when the truth is that their cluster is not eligible.
	ApplySupported bool `json:"applySupported"`

	// LastRun is the most recent upgrade attempt, present once there has been
	// one. Carried on the status payload so the PWA can poll a single endpoint
	// while an upgrade is in flight.
	LastRun *Run `json:"lastRun,omitempty"`
}

// Checker polls the release feed and caches the answer. It never mutates the
// cluster — the whole apply path is a separate concern that does not exist
// yet.
//
// Runs as a controller-runtime Runnable: mgr.Add(manager.RunnableFunc(c.Start)).
type Checker struct {
	// Feed reads published GitHub releases — the stable channel's source.
	// Required.
	Feed *FeedClient
	// ChartFeed lists published charts in the OCI registry — the main
	// channel's source. Optional; without it a cluster set to `main` reports
	// the check as failed rather than silently claiming to be up to date,
	// because "we cannot see the feed" and "there is nothing new" must never
	// render the same.
	ChartFeed *ChartFeedClient
	// Store loads the operator's policy. Required.
	Store *Store
	// K8sClient is used for ownership detection. Optional; nil reports
	// ManagedByUnknown.
	K8sClient client.Client
	// Applier reports whether this build can install an update, and what the
	// last attempt did. Optional; nil means apply is not available and the
	// status says so rather than offering a button that 503s.
	Applier *Applier
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

	// checkMu serializes Check. Check is a read-modify-write around a network
	// call: it snapshots the cached status, polls, then writes the whole
	// struct back. Two overlapping calls — the hourly ticker and an operator
	// pressing "Check now" — would race, and whichever finished last would
	// discard the other wholesale, so a successful poll's result could be
	// thrown away by a concurrent failing one. Serializing is simpler than
	// merging field-by-field and costs nothing at this cadence.
	checkMu sync.Mutex
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
		// UpdateAvailable must be recomputed against the policy we just read,
		// not left at whatever the last poll decided. Otherwise pinning a
		// cluster returns updateAvailable:true alongside "held at 1.0.1" in
		// the same payload — the package promises a pinned cluster never
		// reports an update, and a stale flag breaks that promise in the one
		// response the operator sees right after making the change.
		s.UpdateAvailable = c.availableUnder(s.LatestVersion, s.Policy)
		s.ApplySupported, s.LastRun = c.applyState(ctx)
		return s
	}
	base := Status{
		CurrentVersion: c.CurrentVersion,
		Policy:         c.loadPolicy(ctx, DefaultPolicy()),
	}
	base.ApplySupported, base.LastRun = c.applyState(ctx)
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

// latestFor asks the source that matches the channel.
//
// stable reads GitHub's /releases/latest — an explicit publishing decision,
// not our idea of version ordering. main reads the chart registry, because on
// that channel a commit is only an update once its chart exists to pull.
func (c *Checker) latestFor(ctx context.Context, channel Channel) (*Release, error) {
	if channel == ChannelMain {
		if c.ChartFeed == nil {
			return nil, fmt.Errorf("channel %q needs a chart registry to read and none is configured on this control plane", ChannelMain)
		}
		return c.ChartFeed.Latest(ctx, channel)
	}
	return c.Feed.Latest(ctx)
}

func (c *Checker) detectOwner(ctx context.Context) ManagedBy {
	return DetectManagedBy(ctx, c.K8sClient, c.Namespace, c.ControlPlaneDeployment)
}

// applyState reports whether this control plane can install updates, and the
// last attempt if there was one.
//
// A failure to read past runs is logged and swallowed: it must not take down
// the status card, which is the surface an operator uses to work out what is
// wrong in the first place.
func (c *Checker) applyState(ctx context.Context) (bool, *Run) {
	if c.Applier == nil || !c.Applier.Configured() {
		return false, nil
	}
	last, err := c.Applier.Latest(ctx)
	if err != nil {
		c.logger().Warn("could not read previous upgrade runs", "error", err)
		return true, nil
	}
	return true, last
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
	c.checkMu.Lock()
	defer c.checkMu.Unlock()

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
	next.ApplySupported, next.LastRun = c.applyState(ctx)

	rel, err := c.latestFor(ctx, policy.Channel)
	switch {
	case err != nil:
		// A cancelled context means the CALLER went away (a PWA fetch aborted
		// mid-poll), not that the cluster cannot reach the feed. Persisting it
		// would leave the card reading "we stopped checking" on a healthy
		// cluster until the next hourly tick.
		if ctx.Err() != nil {
			c.logger().Info("update check aborted by the caller; leaving the last result in place", "error", err)
			return next
		}
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

// availableUnder re-answers "is an update available?" from an already-known
// latest version and a possibly-changed policy, without re-polling. An
// unparseable or absent latest means no — we do not guess.
func (c *Checker) availableUnder(latestVersion string, policy Policy) bool {
	if latestVersion == "" {
		return false
	}
	latest, err := ParseVersion(latestVersion)
	if err != nil {
		return false
	}
	return c.isNewer(latest, policy)
}

// StatusWithPolicy renders the status against a policy the caller already has,
// skipping the ConfigMap re-read.
//
// Exists because reads go through the manager's CACHED client while writes go
// straight to the API server: re-reading immediately after a successful write
// can return the PRE-write policy, so a PUT would echo back the setting the
// operator just changed as unchanged. The caller passes what it saved.
func (c *Checker) StatusWithPolicy(ctx context.Context, p Policy) Status {
	s := c.Status(ctx)
	s.Policy = p
	s.ManagedBy = c.detectOwner(ctx)
	s.CanSelfUpgrade = s.ManagedBy.CanSelfUpgrade()
	s.Reason = c.reasonFor(s.ManagedBy, p)
	s.UpdateAvailable = c.availableUnder(s.LatestVersion, p)
	return s
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
