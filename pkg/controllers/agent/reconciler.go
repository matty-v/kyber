package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/fleetdefaults"
	"github.com/matty-v/kyber/pkg/metricsstore"
	"github.com/matty-v/kyber/pkg/podtoken"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
	"github.com/matty-v/kyber/pkg/skillstore"
	"github.com/matty-v/kyber/pkg/statechangestore"
	"github.com/matty-v/kyber/pkg/telemetry"
	"github.com/matty-v/kyber/pkg/tokenreport"
	"github.com/matty-v/kyber/pkg/tokenstore"
)

// AgentFinalizer is the finalizer name used to gate cleanup of agent resources.
// The spec (2026-04-10-agent-controller-design.md) uses the placeholder domain
// "platform.example.com/agent-cleanup"; we use the project's actual domain
// "kyber.io" to match the CRD group and label prefix. This is an intentional
// deviation from the spec's placeholder text.
const AgentFinalizer = "kyber.io/agent-cleanup"

const (
	// maxRestartRetries is the number of auto-restart attempts before staying in Failed.
	maxRestartRetries = int32(3)

	// startupTimeoutSeconds is how long the controller waits for Starting → Running.
	startupTimeoutSeconds = 120 * time.Second

	// requeueWaiting is how long to wait before checking on a pod that's not yet Ready.
	requeueWaiting = 30 * time.Second

	// terminatingPodGraceWindow bounds the recently-deleted wait guard: a
	// Running agent's pod with a DeletionTimestamp younger than this is a
	// deliberate graceful roll in progress (wait for the roll's own
	// transition); older means stuck Terminating (dead node) and takes
	// the dead-pod recovery path.
	terminatingPodGraceWindow = 60 * time.Second

	// requeueImmediate is used after transitions that should quickly lead to another.
	requeueImmediate = 2 * time.Second

	// requeueOrphanCleanup is the backoff between finalizer retries when the
	// external stores (Postgres/Redis) are transiently unreachable during
	// orphan cleanup (kyber#565). Longer than requeueImmediate to give a
	// blipping store room to recover without busy-looping the finalizer.
	requeueOrphanCleanup = 10 * time.Second

	// orphanCleanupMaxAttempts bounds those retries. After this many failed
	// attempts the finalizer gives up reaping the external stores, completes
	// agent deletion anyway, and emits an OrphanCleanupIncomplete Event — so a
	// durably-unreachable store can never wedge deletion forever (the #171
	// off-cluster backups are the recoverability backstop, not a stuck
	// finalizer). The attempt count is tracked on the agent via the
	// orphanCleanupAttemptsAnnotation.
	orphanCleanupMaxAttempts = 5
)

// orphanCleanupAttemptsAnnotation counts finalizer orphan-cleanup attempts that
// failed against the external stores, so the bounded-give-up policy (kyber#565)
// has state across reconciles.
const orphanCleanupAttemptsAnnotation = "kyber.io/orphan-cleanup-attempts"

// MachineGetter retrieves machine CRDs. Used by the agent reconciler to check
// machine phase when classifying pod death events.
type MachineGetter interface {
	Get(ctx context.Context, name, namespace string) (*kyberv1.Machine, error)
}

// KubernetesMachineGetter reads Machine CRDs through the controller cache.
// Production must install this on AgentReconciler; otherwise machine-loss
// recovery silently degrades into ordinary pod-crash retries.
type KubernetesMachineGetter struct {
	Client client.Reader
}

func (g *KubernetesMachineGetter) Get(ctx context.Context, name, namespace string) (*kyberv1.Machine, error) {
	machine := &kyberv1.Machine{}
	if err := g.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, machine); err != nil {
		return nil, fmt.Errorf("getting machine: %w", err)
	}
	return machine, nil
}

// AgentReconciler reconciles Agent CRDs.
//
// +kubebuilder:rbac:groups=kyber.io,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kyber.io,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kyber.io,resources=agents/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
type AgentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// AdapterRegistry maps runtime type strings to their adapters. Optional
	// at construction time (post-#250): when nil, resolveAdapter falls back
	// to the global registry in pkg/runtimes. Tests inject stubs here to
	// avoid touching the global registry; production main.go can also pre-
	// build the map from runtimes.All() if it wants explicit ownership.
	AdapterRegistry map[string]pkgruntimes.Adapter

	// BriefStore persists session briefs for each agent.
	// Written before pod creation so the init container can fetch the brief via the internal API.
	// If nil, brief writing is skipped and the init container falls back to an empty brief.
	BriefStore briefstore.BriefStore

	// The following stores are reaped by the delete finalizer so a
	// confirmed agent delete leaves zero orphaned identity state (kyber#565
	// AC-5). They are the same handles the InternalServer already constructs in
	// cmd/control-plane/main.go, threaded in here for cleanup-on-delete. Each is
	// optional: a nil store means that backend isn't configured (dev install
	// without Redis/Postgres), so the finalizer simply skips it — there is
	// nothing of its kind to orphan.
	//
	// TokenStore holds the TTL'd per-agent token-usage snapshot
	// (token-usage:<agent>).
	TokenStore tokenstore.TokenStore
	// TokenAccumulator holds the non-TTL'd all-time token counts
	// (accum:token_usage:<ns>:<agent>:<model>) — a real orphan risk.
	TokenAccumulator tokenstore.Accumulator
	// MetricsStore holds the per-agent activity / token time-series
	// (ts:activity / ts:token_usage :<ns>:<agent>:*).
	MetricsStore metricsstore.MetricsStore
	// StateChangeAccumulator holds the non-TTL'd state-transition counts
	// (accum:state_changes:<ns>:<agent>) — a real orphan risk.
	StateChangeAccumulator statechangestore.Accumulator
	// SkillStore holds the agent's last reported skill inventory. Durable, so
	// a deleted agent's row would otherwise outlive it and be served to a
	// later agent that reuses the name.
	SkillStore skillstore.Store

	// AlertSink receives alerts for notable agent events (e.g., Failed, retry limit reached).
	// If nil, alerts are silently dropped. Use telemetry.NewLogAlertSink() for a safe default.
	AlertSink telemetry.AlertSink

	// MachineGetter retrieves machine CRDs for preemption classification.
	// If nil, pod deaths are always classified as crashes (pre-preemption behavior).
	MachineGetter MachineGetter

	// PodTokenKey is the cluster signing key used to mint each agent's
	// control-plane-signed pod-token (kyber#566, Option A). Sourced from the
	// kyber-internal-signing-key Secret via KYBER_INTERNAL_SIGNING_KEY in
	// cmd/control-plane/main.go. Empty (the Go zero value) disables minting —
	// the pod-token Secret is not created, and the internal API runs without
	// enforcement (degraded mode; logged loudly at startup). This keeps unit
	// tests and pre-key-delivery installs working.
	PodTokenKey []byte

	// AgentStorageClass is the StorageClass name applied to new agent PVCs.
	// Empty string means omit StorageClassName from the PVC spec, which lets
	// the cluster's default StorageClass (e.g. local-path on k3s, standard on
	// GKE) bind the volume. Sourced from KYBER_AGENT_STORAGE_CLASS in
	// cmd/control-plane/main.go.
	AgentStorageClass string

	// TranscriptOffsetsStorageClass / TranscriptOffsetsSize configure the small
	// per-agent transcript-offsets PVC (kyber#467) that durably holds the tailer's
	// line-count checkpoints. The StorageClass deliberately defaults to empty so
	// the cluster default (local-path) binds it on ALL targets — never kyber-pd,
	// whose 1Gi PD minimum would waste space for sub-1KB checkpoints (Ackbar's
	// deploy review). Empty size falls back to defaultTranscriptOffsetsSize.
	// Sourced from KYBER_TRANSCRIPT_OFFSETS_STORAGE_CLASS / _SIZE in
	// cmd/control-plane/main.go.
	TranscriptOffsetsStorageClass string
	TranscriptOffsetsSize         string

	// GithubTokenMinter mints installation tokens for agents that have a
	// spec.identityRepo.repo configured. Nil means the Kyber GitHub App
	// Secret wasn't present at startup — agents without IdentityRepo work
	// fine, agents with IdentityRepo will have their status surface a
	// configuration error.
	GithubTokenMinter GithubTokenMinter

	// Scaffolder creates a new GitHub repo from a template repo and substitutes
	// agent-specific placeholders. Used by the auto-create path when
	// spec.identityRepo.template is set and spec.identityRepo.repo is empty.
	// Nil means auto-create is disabled — agents that set only identityRepo.repo
	// are unaffected.
	Scaffolder RepoScaffolder

	// IdentityRepoOwner is the GitHub username/org under which auto-created
	// identity repos are placed (e.g. "matty-v"). Sourced from
	// KYBER_IDENTITY_REPO_OWNER in cmd/control-plane/main.go. Required when
	// Scaffolder is set; auto-create surfaces a Failed status when empty.
	IdentityRepoOwner string

	// StatusSidecarImage is the full image reference for the
	// kyber-status-sidecar container injected into every agent pod
	// (kyber#248). Sourced from KYBER_STATUS_SIDECAR_IMAGE in
	// cmd/control-plane/main.go. Empty string disables sidecar injection
	// — used by unit tests that don't care about the sidecar and by dev
	// installs that haven't built the image yet.
	StatusSidecarImage string

	// DiscordSidecarImage is the full image reference for the
	// kyber-mcp-discord channel sidecar (kyber#646), injected only into pods
	// of agents that enable spec.channels.discord. Sourced from
	// KYBER_DISCORD_SIDECAR_IMAGE in cmd/control-plane/main.go. Empty disables
	// injection (dev installs / tests / agents without the Discord channel).
	DiscordSidecarImage string

	// TelegramSidecarImage is the full image reference for the Telegram polling
	// bridge. Runtime-neutral since kyber#684 — every agent with
	// spec.secrets.telegramEnabled gets it, and the native Claude Code plugin
	// was retired. Empty disables injection and raises TelegramUnavailable.
	TelegramSidecarImage string

	// TelegramDefaultAllowedUserIDs is a comma-separated list of Telegram user
	// IDs an install trusts by default (chart value
	// `telegram.defaultAllowedUserIds`, env KYBER_TELEGRAM_DEFAULT_ALLOWED_USER_IDS).
	//
	// It exists for one job: the kyber#684 migration. An agent configured under
	// the retired plugin kept its allowlist in access.json on its own PVC, which
	// the control plane cannot read, so there is no per-agent value to carry
	// forward — but the install operator does know who owns these agents. Only
	// ever used to seed a Secret that has NO allowlist key; it never overrides
	// one. Empty means a migrated agent starts with no allowlist and accepts
	// nobody until an operator sets one through /comms.
	TelegramDefaultAllowedUserIDs string

	// SidecarOtelEndpoint is the OTLP HTTP endpoint the sidecar pushes
	// per-agent metrics to (kyber#256, #247 Phase C1). Sourced from
	// KYBER_SIDECAR_OTEL_ENDPOINT in cmd/control-plane/main.go (chart
	// value `controlPlane.otelEndpoint`). Empty disables sidecar metrics
	// — the rest of the sidecar (heartbeat, event forwarder) keeps
	// working.
	SidecarOtelEndpoint string

	// SidecarLogLevel is the slog level to set on every injected sidecar
	// pod (kyber#360 diagnostic safety net). Sourced from
	// KYBER_SIDECAR_LOG_LEVEL in cmd/control-plane/main.go. Empty (the
	// default) leaves the env var unset on sidecar pods → sidecar defaults
	// to LevelInfo. Set to "debug" to enable the forwarder + snapshot
	// debug log lines that would otherwise hide the next regression.
	SidecarLogLevel string
	// DiscordLogLevel and TelegramLogLevel are component-specific effective
	// levels passed to the channel sidecars as KYBER_LOG_LEVEL.
	DiscordLogLevel  string
	TelegramLogLevel string

	// SidecarAutoRollEnabled toggles the kyber#299 Option B auto-roll
	// behavior: when an agent's SidecarOutOfDate condition has been True
	// long enough, the controller deletes the pod so it comes back on
	// the new sidecar digest. Off by default — operators opt in via the
	// chart's KYBER_SIDECAR_AUTO_ROLL env var. The PWA's "dirty" surface
	// (Option A) is the always-on fallback.
	SidecarAutoRollEnabled bool

	// SidecarAutoRollMinStable is the minimum duration the
	// SidecarOutOfDate condition must have been True before auto-roll
	// fires. Guards against flapping during a control-plane bounce
	// (controller env-var briefly empty mid-roll). Sourced from
	// KYBER_SIDECAR_AUTO_ROLL_MIN_STABLE; zero means use the package
	// default (sidecarAutoRollDefaultMinStable).
	SidecarAutoRollMinStable time.Duration

	// Transcript-pruner config (kyber#471) — bounds on-PVC transcript backlog
	// growth via a per-agent pruner sidecar. All sourced from
	// KYBER_TRANSCRIPT_RETENTION_* in cmd/control-plane/main.go (chart
	// `transcripts.retention`). When TranscriptRetentionEnabled is false (the Go
	// zero value, so `helm upgrade --reuse-values` without the block is safe) the
	// sidecar is not injected.
	TranscriptRetentionEnabled bool
	// TranscriptRetentionMaxAgeDays is the age threshold: *.jsonl older than this
	// many days is treated as durably archived (>> ship lag) and prune-eligible.
	// Must be > 0 for pruning to occur.
	TranscriptRetentionMaxAgeDays int
	// TranscriptRetentionMaxBytesPerAgent is an optional secondary per-agent size
	// ceiling in bytes; 0 means age-only.
	TranscriptRetentionMaxBytesPerAgent int64
	// TranscriptPruneIntervalMinutes is the pruner poll cadence; <= 0 → 60.
	TranscriptPruneIntervalMinutes int
	// TranscriptRetentionArchiveCrosscheck, when true, additionally requires the
	// tailer's local ship checkpoint to confirm a file was fully shipped before
	// the pruner deletes it. Off by default (the age threshold alone is archive-
	// safe and needs no credentials).
	TranscriptRetentionArchiveCrosscheck bool

	// FleetDefaults resolves cluster-wide fallback values (defaultModel,
	// defaultRuntimeVersion) used when an Agent CR omits the
	// corresponding spec field (kyber#376 / PR-B of #374). Nil disables
	// the fallback layer. An empty resolved model is valid and delegates model
	// selection to the runtime harness.
	// Wired by cmd/control-plane/main.go from the kyber-fleet-defaults
	// ConfigMap; tests typically leave this nil and set spec.Model
	// directly.
	FleetDefaults *fleetdefaults.Resolver

	// SidecarImageCanaryWindow is the maximum time convergeSidecarImage
	// waits for the canary pod's replacement to reach Ready on a freshly-
	// pinned StatusSidecarImage before marking the image as failed and
	// freezing further sidecar-convergence deletes (kyber#371 Defect A).
	// Zero means use the package default
	// (sidecarImageCanaryDefaultWindow).
	SidecarImageCanaryWindow time.Duration

	// RuntimeImageCanaryWindow is the maximum time shouldRollRuntimeImage
	// waits for the canary agent's replacement pod to reach Ready on a
	// freshly-bumped KYBER_AGENT_RUNTIME_IMAGE before marking the image
	// failed and freezing further runtime-image rolls (kyber#529). Zero
	// means use the package default (runtimeImageCanaryDefaultWindow).
	// ADDITIVE: unset preserves current single-agent behavior; not
	// chart-wired (package-default-only, mirroring SidecarImageCanaryWindow).
	RuntimeImageCanaryWindow time.Duration

	// TelegramSidecarCanaryWindow is the same knob for
	// convergeTelegramSidecar (kyber#688). Zero means the package default
	// (sidecarImageCanaryDefaultWindow). Not chart-wired, mirroring
	// RuntimeImageCanaryWindow — it exists so tests can arm a short window.
	TelegramSidecarCanaryWindow time.Duration

	// sidecarCanary is the kyber#371 observed-evidence canary FSM for the
	// status-sidecar image roll (convergeSidecarImage). runtimeCanary is
	// the same FSM for the runtime-image roll (shouldRollRuntimeImage,
	// kyber#529); telegramCanary for the Telegram sidecar roll
	// (convergeTelegramSidecar, kyber#688). Keeping them as separate
	// trackers isolates the image namespaces — a status-sidecar Ready can
	// never falsely verify a runtime image that happens to share a
	// reference string, and vice versa. That isolation matters most for
	// Telegram: a bad telegramSidecar pin has to stay contained to one
	// agent, because a broken bridge is also the agent's ability to report
	// that it is broken. All three are zero-value-ready (lazy map init) and
	// re-armed on controller restart. See imageCanaryTracker
	// (image_canary.go).
	sidecarCanary  imageCanaryTracker
	runtimeCanary  imageCanaryTracker
	telegramCanary imageCanaryTracker

	// sidecarOOMAlerts dedups the kyber#584 Phase C sidecar OOM/flap alert so a
	// flapping native sidecar (transcript-tailer / kyber-status-sidecar) fires an
	// alert once per escalation rather than every reconcile. Zero-value-ready
	// (lazy map init); re-armed on controller restart like the image canaries.
	sidecarOOMAlerts sidecarAlertTracker
}

// Reconcile is the main reconciliation function called by controller-runtime.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the Agent CRD.
	agent := &kyberv1.Agent{}
	if err := r.Get(ctx, req.NamespacedName, agent); err != nil {
		if errors.IsNotFound(err) {
			// Agent deleted before we could process it — nothing to do.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching agent: %w", err)
	}

	// Record reconcile latency. The phase label is captured now (before any transition)
	// so we know which phase triggered the reconcile.
	reconcileStart := time.Now()
	phaseAtStart := string(agent.Status.Phase)
	defer func() {
		if telemetry.AgentReconcileLatency != nil {
			telemetry.AgentReconcileLatency.Record(ctx,
				time.Since(reconcileStart).Seconds(),
				metric.WithAttributes(attribute.String("phase", phaseAtStart)))
		}
	}()

	// 2. Handle deletion: run finalizer if the CRD is being deleted.
	if !agent.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, agent)
	}

	// 3. Ensure finalizer is registered.
	if err := r.ensureFinalizer(ctx, agent); err != nil {
		return ctrl.Result{}, err
	}

	// 3a. Ensure the user-secrets shell Secrets exist (#75). These are mounted
	// unconditionally by pod_builder.go, so they must exist before any pod is
	// created — see docs/design/2026-04-18-user-secrets-design.md.
	if err := r.ensureUserSecrets(ctx, agent); err != nil {
		return ctrl.Result{}, err
	}

	// 3a.0. Heal agents wired for Discord before the bot token moved into the
	// sidecar. Their binding Action still tells the agent to call Discord's REST
	// API with DISCORD_BOT_TOKEN, which the runtime no longer receives — so
	// outbound replies fail with no condition and no log. The comms API rewrites
	// this on PUT, but nothing re-PUTs an already-wired agent, so without this
	// the break is silent and permanent.
	if err := r.migrateLegacyDiscordAction(ctx, agent); err != nil {
		return ctrl.Result{}, err
	}

	// 3a.0.05. Heal agents that had Telegram configured under the retired
	// in-process plugin (kyber#684). Their Secret holds only a bot token and
	// they have no inbound binding at all, because the plugin polled and replied
	// in-process and never touched the platform's inbound rail. Left alone, such
	// an agent comes back from its next restart with a sidecar that cannot sign,
	// cannot allowlist and has nowhere to deliver — silently deaf behind a green
	// pod. Runs BEFORE the state machine so the wiring is in place by the time a
	// pod is built.
	telegramWiring := r.migrateLegacyTelegramSecret(ctx, agent)

	// 3a.0.1. Surface an install that enables Telegram but never pinned
	// image.telegramSidecar.tag (kyber#684). Since the convergence there is no
	// plugin left to fall back to and the runtime holds no bot token, so the
	// agent simply has no Telegram — which reads to an operator as "the agent
	// stopped answering me", with nothing anywhere pointing at the install.
	// Runs every reconcile so it also CLEARS once the image is pinned; the
	// agent's pod is never rebuilt for this, so nothing else would clear it.
	r.reconcileTelegramCondition(ctx, agent, telegramWiring)

	// 3a.1. Mint the agent's control-plane-signed pod-token Secret (kyber#566)
	// before any pod is created, so pod_builder.go's mount resolves and the
	// agent's clients can authenticate to the internal API as themselves. No-op
	// until the signing key is delivered (PodTokenKey empty).
	if err := r.ensurePodTokenSecret(ctx, agent); err != nil {
		return ctrl.Result{}, err
	}

	// 3b. Ensure the per-agent GitHub identity-repo Secret is present and
	// fresh when spec.identityRepo.repo is set. This runs before the state
	// machine so the pod has a valid token mounted the moment it starts.
	// Failures are logged but don't block the rest of the reconcile — the
	// agent's lifecycle shouldn't be held hostage by a GitHub outage. The
	// returned duration tells us when to come back to refresh the token.
	identityRequeue, err := r.reconcileIdentityRepo(ctx, agent)
	if err != nil {
		logger.Error(err, "reconciling identity repo Secret (continuing)", "agent", agent.Name)
	}

	// 3b.2. Render spec.jobs into the per-agent <name>-jobs ConfigMap. The
	// pod's volumeMount at /kyber/jobs-src sources from this — the dispatcher
	// reads prompt files out of it and entrypoint.sh copies the rendered
	// crontab into /persist/cron/cron.d/kyber-jobs on every boot. A ConfigMap
	// rotation while the pod is up propagates via kubelet (~30-60s); the
	// in-pod node-agent re-syncs the persist path so cron's mtime scan picks
	// up changes without a restart. Failures here don't block the state
	// machine — jobs are a secondary feature and an empty ConfigMap simply
	// means no scheduled prompts fire.
	if err := r.reconcileJobsConfigMap(ctx, agent); err != nil {
		logger.Error(err, "reconciling jobs ConfigMap (continuing)", "agent", agent.Name)
	}

	// 3c. Gate pod creation when auto-create is still pending. Template set +
	// Repo empty means the scaffolder hasn't successfully patched Repo yet —
	// running the state machine now would build a pod without the
	// identity-repo env vars and mount, and spec changes on an already-running
	// pod don't rebuild it. Only gate before the state machine has produced a
	// pod (Phase=="" or Creating); never thrash an already-running agent.
	if agent.Spec.IdentityRepo.Template != "" && agent.Spec.IdentityRepo.Repo == "" {
		if agent.Status.Phase == "" || agent.Status.Phase == kyberv1.AgentPhaseCreating {
			logger.Info("waiting for identity repo scaffold before creating pod",
				"agent", agent.Name, "template", agent.Spec.IdentityRepo.Template)
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}

	// 4. Check if the retry counter should be reset (agent stable in Running for 5 min).
	if agent.Status.StartTime != nil {
		if ShouldResetRetryCount(agent.Status.Phase, agent.Status.StartTime.Time) &&
			agent.Status.RestartCount > 0 {
			patch := client.MergeFrom(agent.DeepCopy())
			agent.Status.RestartCount = 0
			if err := r.Status().Patch(ctx, agent, patch); err != nil {
				return ctrl.Result{}, fmt.Errorf("resetting restart count: %w", err)
			}
			logger.Info("reset restart count after 5 min stable", "agent", agent.Name)
		}
	}

	// 5. Read the current pod state.
	pod, err := r.getAgentPod(ctx, agent)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 5a. Verification trigger for the kyber#371 observed-evidence canary.
	// When the current pod is running the controller's current
	// StatusSidecarImage AND the sidecar container is Ready (kubelet
	// pulled successfully and readiness gates pass), record the image as
	// verified. This is the positive signal that gates the convergence
	// pullability check in 5d — without it, an unverified image stays in
	// canary mode and only one pod is at risk per (controller process,
	// bad-image) pair.
	if pod != nil && r.StatusSidecarImage != "" &&
		extractSidecarSpecImage(pod) == r.StatusSidecarImage &&
		isSidecarReady(pod) {
		r.markSidecarImageVerified(r.StatusSidecarImage)
	}

	// 5a.1. Verification trigger for the kyber#529 runtime-image canary,
	// mirroring 5a. When the current pod's agent container is running the
	// controller's currently-desired runtime image (adapter.Image()) AND
	// that container is Ready (kubelet pulled successfully and the readiness
	// probe passes), record the image as verified. This is the positive
	// signal shouldRollRuntimeImage gates on: once the canary's replacement
	// pod is Ready on the new image, the rest of the fleet is released to
	// roll in bounded waves. A resolveAdapter error or empty image skips the
	// trigger (no verification, no error) — same fail-safe posture as the
	// drift roll itself.
	if pod != nil {
		if adapter, err := r.resolveAdapter(agent); err == nil {
			desiredRuntimeImage := adapter.Image()
			if desiredRuntimeImage != "" &&
				extractAgentSpecImage(pod) == desiredRuntimeImage &&
				isAgentReady(pod) {
				r.runtimeCanary.markVerified(desiredRuntimeImage)
			}
		}
	}

	// 5a.2. Verification trigger for the kyber#688 Telegram-sidecar canary,
	// mirroring 5a. A pod whose kyber-mcp-telegram container is both on the
	// controller's current TelegramSidecarImage AND Ready proves that image
	// pullable and the bridge startable, which releases the rest of the
	// Telegram fleet to converge onto it. Note this is a stronger signal than
	// the status sidecar's: the bridge exits rather than start without an
	// allowlist, so Ready here means it is actually polling.
	if pod != nil && r.TelegramSidecarImage != "" &&
		extractContainerSpecImage(pod, TelegramSidecarContainerName) == r.TelegramSidecarImage &&
		isTelegramSidecarReady(pod) {
		r.telegramCanary.markVerified(r.TelegramSidecarImage)
	}

	// 5b. Detect sidecar drift and surface via SidecarOutOfDate condition
	// (kyber#299). Best-effort patch — failures are logged but never
	// propagated. Runs every reconcile while the agent is Running so the
	// condition tracks the controller's CURRENT StatusSidecarImage env
	// var: when an operator bumps the sidecar pin in kyber-deploy, the
	// next reconcile after the new control-plane pod takes over flips
	// the condition on existing agent pods that haven't been recreated.
	{
		before := agent.DeepCopy()
		if r.reconcileSidecarDriftCondition(agent, pod) {
			if patchErr := r.Status().Patch(ctx, agent, client.MergeFrom(before)); patchErr != nil {
				logger.Info("sidecar-drift condition patch failed (best-effort)",
					"agent", agent.Name, "err", patchErr)
			}
		}
	}

	// 5b.1. Mismatch safety net — translate the runtime-report fields
	// (Status.Runtime.{RequestedVersion, RequestedSatisfied, ModelSupported}
	// — populated asynchronously by /internal/agents/{name}/runtime-version
	// from the pod's start-claude.sh probe + install outcome) into
	// AgentConditionRuntimeVersionMismatch and AgentConditionModelUnsupported.
	// The PWA's agent detail view renders one badge per True condition.
	// Conditions clear within one report cycle once the underlying signal
	// resolves. Same best-effort patch shape as 5b — kyber#379 / PR-E.
	{
		before := agent.DeepCopy()
		if r.reconcileRuntimeStatusConditions(agent) {
			if patchErr := r.Status().Patch(ctx, agent, client.MergeFrom(before)); patchErr != nil {
				logger.Info("runtime-status condition patch failed (best-effort)",
					"agent", agent.Name, "err", patchErr)
			}
		}
	}

	// 5c. Auto-roll the agent's pod when the SidecarOutOfDate condition
	// has been True long enough and the agent is idle (kyber#299
	// Option B; gated by SidecarAutoRollEnabled). When we delete the
	// pod, requeue immediately so the next reconcile rebuilds it; the
	// state machine doesn't need to fire this pass.
	if rolled, rollErr := r.maybeAutoRollSidecarForDrift(ctx, agent, pod); rollErr != nil {
		logger.Info("sidecar auto-roll attempt failed (best-effort)",
			"agent", agent.Name, "err", rollErr)
	} else if rolled {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// 5d. Tag-level sidecar convergence (kyber#358). When the pod's
	// status-sidecar spec image differs from the controller's current
	// StatusSidecarImage env, delete the pod so the next reconcile
	// rebuilds it on the new image. Without it, a `helm upgrade` that
	// bumps image.statusSidecar.tag leaves the existing pods on the old
	// sidecar forever (seen in a production metrics outage).
	//
	// Hardened by kyber#371: convergeSidecarImage now mirrors 5c's
	// safeguards (idle gate + concurrency cap) and adds an observed-
	// evidence canary so a bad StatusSidecarImage env can never delete
	// more than one pod per (controller process, bad image) pair (R2-D2
	// incident 2026-05-29). 5c and 5d share the concurrency budget;
	// 5c runs first so a digest-pin auto-roll takes precedence over a
	// tag-level convergence in the same reconcile.
	if rolled, rollErr := r.convergeSidecarImage(ctx, agent, pod); rollErr != nil {
		logger.Info("sidecar image convergence failed (best-effort)",
			"agent", agent.Name, "err", rollErr)
	} else if rolled {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// 5d.1. Telegram sidecar convergence (kyber#688). Same three gates as 5d
	// plus the channel's own preconditions, and one extra kind of drift the
	// status sidecar cannot have: a MISSING bridge container. Until this
	// existed, AppendTelegramSidecar ran only when a pod was built, so an
	// agent migrated off the retired in-process plugin kept the plugin until
	// something unrelated recreated its pod (observed live: `dave`, 68
	// minutes) and a telegramSidecar digest bump never reached a running pod
	// (`r2-d2`, stale bridge until a manual delete). Runs after 5d so the
	// status-sidecar convergence keeps precedence when a pod is behind on
	// both — either delete rebuilds the pod with both images current, so the
	// order only decides which Event an operator sees.
	if rolled, rollErr := r.convergeTelegramSidecar(ctx, agent, pod, telegramWiring); rollErr != nil {
		logger.Info("telegram sidecar convergence failed (best-effort)",
			"agent", agent.Name, "err", rollErr)
	} else if rolled {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if rolled, rollErr := r.convergeDiscordSidecar(ctx, agent, pod); rollErr != nil {
		logger.Info("discord sidecar convergence failed (best-effort)", "agent", agent.Name, "err", rollErr)
	} else if rolled {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// 5e. Mirror pod-derived state into Agent.Status (kyber#355). Populates
	// the Status card on the Agent detail page: PodName, PodIP, NodeName,
	// StartTime — all already-declared optional fields that nothing was
	// writing, so /api/v1/agents/{name} returned just `{"phase":"Running"}`
	// and the UI card rendered as a bare heading. Only runs in Running phase
	// (other phases the fields aren't meaningful yet) and only patches on
	// diff, so steady-state reconciles add zero API-server load.
	//
	// Status.RestartCount is intentionally NOT touched — it's the
	// agent-retry counter (reset by ActionResetRetryAndCreatePod), not a
	// k8s container-restart count. Repurposing it would silently invert
	// behavior for anyone watching the field. See issue #355 Decision 1.
	if pod != nil && agent.Status.Phase == kyberv1.AgentPhaseRunning && podDerivedStatusDiffers(agent, pod) {
		before := agent.DeepCopy()
		applyPodDerivedStatus(agent, pod)
		if patchErr := r.Status().Patch(ctx, agent, client.MergeFrom(before)); patchErr != nil {
			logger.Info("pod-derived status patch failed (best-effort)",
				"agent", agent.Name, "err", patchErr)
		}
	}

	// 5f. Surface a sidecar OOM/flap as an alert (kyber#584 Phase C). A native
	// sidecar (transcript-tailer / kyber-status-sidecar, kyber#575) that OOMs
	// under RestartPolicy:Always self-heals SILENTLY — the #575 masking that hid
	// the tailer's file-count memory regression for hours. Fire the existing
	// alert path (Echo Base / Telegram) when a monitored sidecar is OOMKilled or
	// flapping past threshold, deduped so a steady flap alerts once per
	// escalation rather than every reconcile. Best-effort and side-effect-only —
	// it never blocks or fails the reconcile (the state machine is driven by the
	// AGENT container's OOM path, not this).
	if pod != nil {
		if fire, restartCount, details := sidecarOOMOrFlapping(pod, sidecarOOMRestartThreshold); fire &&
			r.sidecarOOMAlerts.shouldFire(agent.Name, restartCount) {
			r.fireAlert(ctx, agent, "warning", "SidecarOOMRestart", details)
		}
	}

	// 6. Determine the event to drive the state machine.
	event, err := r.classifyEvent(ctx, agent, pod)
	if err != nil {
		return ctrl.Result{}, err
	}
	if event == "" {
		// No transition needed — agent is in a stable state.
		// Exceptions: some phases must keep polling so time-based checks (startup
		// timeout, machine replacement) eventually fire even when no watch events
		// arrive.
		var base time.Duration
		switch agent.Status.Phase {
		case kyberv1.AgentPhaseCreating, kyberv1.AgentPhaseStarting:
			base = requeueWaiting
			// kyber#210: while we wait, surface any scheduler/kubelet
			// failure that's been hanging on the pod past the grace
			// window. populateSchedulingStatus is best-effort; a List
			// failure is logged inside the helper and we just keep going.
			//
			// Creating is included because a pod that never starts its
			// runtime container never leaves it. On kyber-canary a test
			// agent sat in Creating for eleven hours: the pod existed and
			// was Pending the whole time, but this ran only for Starting,
			// so nothing ever looked at it and the operator got an
			// unchanging phase with no reason attached.
			if pod != nil && pod.Status.Phase == corev1.PodPending {
				before := agent.DeepCopy()
				if populateSchedulingStatus(ctx, r.Client, pod, agent) {
					if patchErr := r.Status().Patch(ctx, agent, client.MergeFrom(before)); patchErr != nil {
						logger.Info("scheduling-status patch failed (best-effort)", "agent", agent.Name, "err", patchErr)
					}
				}
			}
		case kyberv1.AgentPhaseWaitingForMachine:
			base = 15 * time.Second
		case kyberv1.AgentPhaseDiskExhausted:
			// PVC expansion is asynchronous. Poll as a fallback in addition to
			// the owned-PVC watch so a missed event cannot stall recovery.
			base = requeueWaiting
		case kyberv1.AgentPhaseRunning:
			// The recently-terminating wait guard (classifyEvent) emits no
			// event while a graceful roll's pod delete is in flight. The
			// stuck-Terminating recovery at the grace bound must not depend
			// on another watch event arriving — a dead node produces none —
			// so requeue for the remainder of the window ourselves.
			if pod != nil && pod.DeletionTimestamp != nil {
				remaining := terminatingPodGraceWindow - time.Since(pod.DeletionTimestamp.Time)
				if remaining < time.Second {
					remaining = time.Second
				}
				base = remaining + time.Second
			}
		}
		return ctrl.Result{RequeueAfter: minNonZero(base, identityRequeue)}, nil
	}

	// 6a. If the event is an auto-restart, check whether the backoff window has elapsed.
	// This enforces the 10s/30s/90s staircase defined by RetryBackoffDuration.
	if event == EventAutoRestartTriggered {
		if remaining, throttle := r.shouldThrottleRestart(agent); throttle {
			logger.Info("throttling auto-restart", "agent", agent.Name, "remaining", remaining)
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	}

	// 7. Call the state machine (pure function).
	result, err := NextPhase(agent.Status.Phase, event)
	if err != nil {
		// Invalid transition — log and do not requeue.
		logger.Error(err, "invalid state machine transition",
			"agent", agent.Name,
			"phase", agent.Status.Phase,
			"event", event)
		r.Recorder.Eventf(agent, corev1.EventTypeWarning, "InvalidTransition",
			"Phase=%s Event=%s: %v", agent.Status.Phase, event, err)
		return ctrl.Result{}, nil
	}

	logger.Info("state transition",
		"agent", agent.Name,
		"from", agent.Status.Phase,
		"event", event,
		"action", result.Action,
		"to", result.NextPhase)

	r.Recorder.Eventf(agent, corev1.EventTypeNormal, "StateTransition",
		"Phase changed: %s → %s (event: %s, action: %s)",
		agent.Status.Phase, result.NextPhase, event, result.Action)

	// Record state transition metrics.
	if result.NextPhase != agent.Status.Phase && telemetry.AgentStateTransitions != nil {
		telemetry.AgentStateTransitions.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("from", string(agent.Status.Phase)),
				attribute.String("to", string(result.NextPhase)),
			))
	}
	if result.NextPhase != agent.Status.Phase && telemetry.AgentStateChangesTotal != nil {
		telemetry.AgentStateChangesTotal.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("agent", agent.Name),
				attribute.String("to_state", string(result.NextPhase)),
			))
	}

	// Fire alert on transitions to Failed or Preempted-equivalent (retry limit reached).
	if result.NextPhase == kyberv1.AgentPhaseFailed {
		r.fireAlert(ctx, agent, "warning", "Failed",
			map[string]string{
				"from":         string(agent.Status.Phase),
				"event":        string(event),
				"restartCount": fmt.Sprintf("%d", agent.Status.RestartCount),
			})
	}

	// 8. Execute the transition action.
	requeueAfter, err := r.executeAction(ctx, agent, pod, result.Action, event)
	if err != nil {
		if telemetry.AgentReconcileErrors != nil {
			telemetry.AgentReconcileErrors.Add(ctx, 1,
				metric.WithAttributes(attribute.String("event", string(event))))
		}
		return ctrl.Result{}, err
	}

	// 9. Update the CRD status with the new phase.
	if err := r.updatePhase(ctx, agent, result.NextPhase, ""); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: minNonZero(requeueAfter, identityRequeue)}, nil
}

// minNonZero returns the smaller of two durations, treating 0 as "no
// preference". If both are zero the result is zero (controller-runtime
// interprets that as "don't requeue on a timer — just wait for watch events").
func minNonZero(a, b time.Duration) time.Duration {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// classifyEvent inspects the agent's current state and pod state to determine which
// event should drive the next state machine transition.
// Returns ("", nil) when the agent is in a stable state and no transition is needed.
func (r *AgentReconciler) classifyEvent(
	ctx context.Context,
	agent *kyberv1.Agent,
	pod *corev1.Pod,
) (Event, error) {

	phase := agent.Status.Phase
	desired := agent.Spec.DesiredPhase

	// New agent (no phase yet).
	if phase == "" {
		return EventCRDCreated, nil
	}

	// Operator-forced re-auth (#395): drop a wedged agent into NeedsAuth so it
	// can be re-authorized from scratch. Centralized here — ahead of the
	// per-phase switches below — for two reasons: (1) the Failed case further
	// down returns EventAutoRestartTriggered before any later check would run,
	// so a scattered guard would be pre-empted on a Failed agent; (2) the
	// recoverable-phase allowlist is the security-relevant gate (the API setter
	// setAgentDesiredPhase has no allowlist), so keeping it in one place makes
	// the boundary auditable. Honored only from recoverable phases; transient/
	// cleanup phases (Creating, Stopping, Restarting, Draining,
	// WaitingForMachine, NeedsAuth, Deleted) derive no event and are untouched.
	if desired == kyberv1.AgentPhaseNeedsAuth {
		switch phase {
		case kyberv1.AgentPhaseRunning, kyberv1.AgentPhaseStarting,
			kyberv1.AgentPhaseFailed, kyberv1.AgentPhaseMemoryExhausted, kyberv1.AgentPhaseDiskExhausted,
			kyberv1.AgentPhaseStopped:
			return EventDesiredNeedsAuth, nil
		}
	}

	// Authoritative Stop kill switch (#468): desiredPhase=Stopped must halt an
	// agent from every phase an operator can hit Stop during an incident — not
	// only Running. Centralized here, immediately after the NeedsAuth block and
	// ahead of the per-phase/pod-state switches below, for the same two reasons
	// (1) the Failed arm returns EventAutoRestartTriggered before any later check
	// runs, so a scattered guard would be pre-empted on exactly the crash-looping
	// agent we need to stop; placing the check first is what makes Stop win over
	// auto-restart and over a same-reconcile PodDied/OOM (the pod-state switch
	// also runs after this) — precedence by ordering, not a tiebreak. (2) the
	// honored-phase allowlist is the security-relevant gate (the API setter
	// setAgentDesiredPhase has no allowlist), so a single auditable source of
	// truth bounds the effect of a forged/stale desiredPhase=Stopped — which is
	// fail-safe regardless (it only halts; recoverable by setting Running).
	//
	// Allowlist deliberately EXCLUDES Stopped (unlike NeedsAuth): a Stopped agent
	// with desired==Stopped must derive no event so it is a stable fixed point
	// and stays down across resyncs until desired flips to Running. Transient/
	// cleanup phases (Creating, Stopping, Restarting, Draining, NeedsAuth,
	// Deleted) are likewise untouched. WaitingForMachine is included because an
	// operator must always be able to stop an Agent during a prolonged outage.
	if desired == kyberv1.AgentPhaseStopped {
		switch phase {
		case kyberv1.AgentPhaseRunning, kyberv1.AgentPhaseStarting,
			kyberv1.AgentPhaseFailed, kyberv1.AgentPhaseMemoryExhausted, kyberv1.AgentPhaseDiskExhausted,
			kyberv1.AgentPhaseWaitingForMachine:
			return EventDesiredStopped, nil
		}
	}

	// Infrastructure availability is not an agent crash. Active and retrying
	// phases park in WaitingForMachine until their assigned Machine is Ready
	// again, without consuming the agent's restart budget. This check must run
	// before the Failed auto-restart arm and before Starting's timeout logic.
	// Operator Stop/NeedsAuth intent above still wins by ordering.
	switch phase {
	case kyberv1.AgentPhaseCreating, kyberv1.AgentPhaseStarting,
		kyberv1.AgentPhaseRunning, kyberv1.AgentPhaseRestarting:
		if r.isMachineUnavailable(ctx, agent) {
			return EventMachineUnavailable, nil
		}
	}

	// Operator intent: desired phase signals (only valid in certain current phases).
	switch phase {
	case kyberv1.AgentPhaseRunning:
		if agent.Status.Activity != nil && agent.Status.Activity.Resources != nil && agent.Status.Activity.Resources.DiskReserveReached {
			return EventDiskReserveReached, nil
		}
		// Check for preemption notice annotation (set by machine controller HandlePreemptionNotice).
		if agent.Annotations["kyber.dev/preemption-notice"] != "" {
			delete(agent.Annotations, "kyber.dev/preemption-notice")
			if err := r.Update(ctx, agent); err != nil {
				return "", err
			}
			return EventPreemptionNotice, nil
		}
		// Note: desired==Stopped from Running is handled by the centralized
		// authoritative-Stop block above (#468), not here — single source of
		// truth, mirroring the NeedsAuth kill switch. It still routes to the
		// same {Running, EventDesiredStopped} → graceful SIGTERM → Stopping
		// transition, so the healthy-stop path is unchanged.
		if desired == kyberv1.AgentPhaseRestarting {
			return EventDesiredRestarting, nil
		}

		// Runtime-image drift (#523): roll a steady Running agent onto a new
		// runtime image. When the live pod's agent-container image differs from
		// the controller's currently-desired image (adapter.Image()), derive
		// EventDesiredRestarting — the same transition rollAgentForUserSecret
		// (routes_user_secrets.go, #515/#517) and operator restarts use — so the
		// state machine captures session state and recreates the pod on the new
		// image. We are already inside the reconcile loop, so we return the event
		// directly rather than writing spec.DesiredPhase (the idiom one switch
		// down at the Running pod-state case). Mirrors the sibling status-sidecar
		// spec-image drift detector (isSidecarSpecMismatched).
		//
		// Precedence: lower than the desired-phase checks above (preemption, the
		// desired Restarting check, and the centralized Stop/NeedsAuth
		// blocks) by ordering — operator intent always wins. Running-only by
		// construction: physically inside case AgentPhaseRunning, so
		// Starting/Restarting/Creating/dormant phases never reach it (they pick up
		// the new image on their next start via BuildPodSpec).
		//
		// Fail-safe: a resolveAdapter error or empty desired image skips the check
		// (no roll, no reconcile error) — a Running agent must never be disrupted
		// by an image-resolution hiccup. isAgentRuntimeImageDrifted's
		// desiredImage=="" guard is the load-bearing half (kyber#360 Cause D).
		//
		// kyber#529: the bare isAgentRuntimeImageDrifted check is now gated by
		// shouldRollRuntimeImage — a fleet-wide KYBER_AGENT_RUNTIME_IMAGE bump
		// rolls Running agents in bounded, canary-gated waves (shared delete
		// budget with 5c/5d) so a bad digest is contained to the canary instead
		// of fleet-widing into ImagePullBackOff. Drift DETECTION is unchanged;
		// only the rollout pacing is gated. A countAgentPodsBeingDeleted (List)
		// error propagates → reconcile requeues with no roll (hold, don't pile on).
		if pod != nil && pod.DeletionTimestamp == nil {
			if adapter, err := r.resolveAdapter(agent); err == nil {
				roll, rollErr := r.shouldRollRuntimeImage(ctx, agent, pod, adapter.Image())
				if rollErr != nil {
					return "", rollErr
				}
				if roll {
					return EventDesiredRestarting, nil
				}
			}
		}

	case kyberv1.AgentPhaseStopped:
		if desired == kyberv1.AgentPhaseRunning {
			return EventDesiredRunning, nil
		}

	case kyberv1.AgentPhaseDiskExhausted:
		if agent.Status.Activity != nil && agent.Status.Activity.Resources != nil && !agent.Status.Activity.Resources.DiskReserveReached {
			return EventDiskReserveCleared, nil
		}
		// Apply an operator-requested increase for both recovery shapes. A live
		// maintenance pod can grow online and will clear the reserve on its next
		// sample; a terminal hard-full pod additionally waits on capacity below.
		if err := r.ensurePVC(ctx, agent); err != nil {
			return "", err
		}
		// A hard-full runtime may have exited before the sidecar could observe
		// cleanup. Expand the claim and wait for its reported capacity before
		// consuming the size change as recovery input; otherwise the replacement
		// pod would mount the same full filesystem and immediately fail again.
		if desired == kyberv1.AgentPhaseRunning && (pod == nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded || isAgentContainerTerminated(pod)) {
			ready, err := r.ensureDiskRecoveryCapacity(ctx, agent)
			if err != nil {
				return "", err
			}
			if !ready {
				return "", nil
			}
			changed, err := r.recoveryInputChanged(ctx, agent)
			if err != nil {
				return "", err
			}
			if changed {
				return EventDesiredRunning, nil
			}
		}

	// Failed is a phase an agent can sit in indefinitely, so — like NeedsAuth
	// and MemoryExhausted below — it must not leave on the bare
	// desiredPhase==Running. That value is permanently true for every agent an
	// operator has ever started, so the unguarded edge fired on EVERY reconcile,
	// and because {Failed, EventDesiredRunning} runs ActionResetRetryAndCreatePod
	// (which zeroes restartCount on the way through) the retry cap below could
	// never be reached. A crash-looping agent rebuilt its pod forever. Same bug
	// as kyber#684, same shape of fix: require the operator's input to have
	// actually CHANGED since the pod we last built.
	//
	// The claim here is metadata.generation vs status.observedGeneration, and
	// restartRequestChanged RECORDS it before returning true — the claim cannot
	// rest on createPod's stamp, which is deliberately best-effort (a failed
	// stamp there is logged and swallowed so a just-created pod is never lost).
	// That was fine while the value only drove a PWA badge; it is not fine now
	// that it bounds a restart loop, because one failed status patch would leave
	// generation ahead forever and every reconcile would re-fire the edge — and
	// its action zeroes restartCount, so the retry cap could not catch it
	// either. Claiming at the decision point, and refusing to act if the claim
	// cannot be written, is the same shape recoveryInputChanged uses for
	// kyber#684.
	//
	// That keeps the two real operator-override paths working unchanged:
	// /set-resources on a Failed agent patches spec.resources (kyber#149), and
	// any lifecycle verb that genuinely changes desiredPhase bumps the
	// generation itself. A bare Start on an agent already carrying
	// desiredPhase==Running does not bump anything, which is why
	// setAgentDesiredPhase clears restartCount for that case — it buys a fresh
	// retry budget, not a loop.
	//
	// Falling through to the retry cap is the safe direction: the agent still
	// auto-restarts up to maxRestartRetries, then holds in Failed with a
	// RetryLimitReached alert instead of hammering the node. The counter resets
	// itself after 5 stable minutes in Running (step 4 of Reconcile).
	case kyberv1.AgentPhaseFailed:
		if desired == kyberv1.AgentPhaseRunning {
			changed, err := r.restartRequestChanged(ctx, agent)
			if err != nil {
				return "", err
			}
			if changed {
				return EventDesiredRunning, nil
			}
		}
		// Auto-restart: if retry count < max, trigger auto-restart.
		if agent.Status.RestartCount < maxRestartRetries {
			return EventAutoRestartTriggered, nil
		}
		return EventRetryLimitReached, nil
	}

	// Pod-state-driven events.
	switch phase {
	case kyberv1.AgentPhaseCreating:
		if pod == nil {
			// Pod not yet created or still being applied — requeue.
			return "", nil
		}
		switch pod.Status.Phase {
		case corev1.PodPending:
			// Check if unschedulable.
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodScheduled &&
					cond.Status == corev1.ConditionFalse &&
					cond.Reason == corev1.PodReasonUnschedulable {
					return EventPodScheduleFailed, nil
				}
			}
			// Still pending — wait.
			return "", nil
		case corev1.PodRunning, corev1.PodSucceeded:
			return EventPodScheduled, nil
		case corev1.PodFailed:
			return EventPodScheduleFailed, nil
		}

	case kyberv1.AgentPhaseStarting:
		if pod == nil || pod.DeletionTimestamp != nil {
			// Pod disappeared (or is stuck Terminating on a dead node) during startup —
			// check if machine was preempted first.
			if r.isMachinePreempted(ctx, agent, pod) {
				return EventMachinePreempted, nil
			}
			return EventPodScheduleFailed, nil
		}
		// Pod entered a terminal state during startup (e.g. overlay mount failure,
		// OAuth token refresh failure, OOM). Detect early instead of waiting for
		// the full startup timeout. Post-#248 the pod has a sidecar that keeps
		// pod-level Phase=Running even after the agent container exits, so
		// isAgentContainerTerminated catches the multi-container case the
		// pod-level Phase check misses (#274).
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded || isAgentContainerTerminated(pod) {
			if isOAuthRefreshFailure(pod) {
				return EventOAuthRefreshFailed, nil
			}
			// Check for kubelet-tagged OOM kill before falling through to a
			// generic PodDied auto-restart — bumping memory is the real fix,
			// auto-restart on the same limit would crash-loop (#272).
			if isOOMKilled(pod, AgentContainerName) {
				return EventOOMKilled, nil
			}
			// kyber#285: kernel OOM observed by the sidecar via the
			// recursive cgroup memory.events file. Catches the case
			// kubelet doesn't tag — PID 1 is dbus-run-session, kernel
			// killed the claude-code child, kubelet recorded
			// reason="Error".
			if hasRecentKernelOOMKill(agent, pod) {
				return EventOOMKilled, nil
			}
			// Suspicious-but-uncertain: agent died with exit 137 and no
			// kernel-OOM signal yet. Could be (a) an OOM the sidecar will
			// post any moment, or (b) an unrelated SIGKILL. Wait briefly
			// for the sidecar's POST. Other exit codes fall through
			// immediately — no point holding them.
			if isAgentExitCode137(pod) && agentTerminatedWithin(pod, oomDetectionWindow) {
				return "", nil
			}
			return EventPodDied, nil
		}
		if isPodReady(pod) {
			return EventPodReady, nil
		}
		// Check startup timeout.
		if agent.Status.LastTransition != nil {
			if time.Since(agent.Status.LastTransition.Time) > startupTimeoutSeconds {
				pending, err := r.codexDeviceAuthPending(ctx, agent)
				if err != nil {
					return "", err
				}
				if pending {
					return "", nil
				}
				return EventStartupTimeout, nil
			}
		}
		// Still starting — requeue.
		return "", nil

	case kyberv1.AgentPhaseRunning:
		// Post-#248 the kyber-status-sidecar container keeps pod-level
		// Phase=Running even after the agent container exits, so
		// isAgentContainerTerminated catches the multi-container case the
		// pod-level Phase check misses (#274). Without this, an exit-2
		// (OAuth refresh failed) lands the agent process dead but
		// Agent.status.phase stays Running and the PWA's Re-authorize
		// button never lights up.
		if pod == nil || pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded || isAgentContainerTerminated(pod) {
			// Pod is gone, stuck Terminating on a dead node, or exited — treat as dead.
			if r.isMachinePreempted(ctx, agent, pod) {
				return EventMachinePreempted, nil
			}
			// Check for OAuth refresh failure (exit code 2 from start-claude.sh).
			if isOAuthRefreshFailure(pod) {
				return EventOAuthRefreshFailed, nil
			}
			// Check for kubelet-tagged OOM kill before falling through to a
			// generic PodDied auto-restart. Bumping memory is the real fix;
			// auto-restart on the same limit would just crash-loop and hide
			// the underlying problem (#272).
			if isOOMKilled(pod, AgentContainerName) {
				return EventOOMKilled, nil
			}
			// kyber#285: kernel OOM observed by the sidecar via the
			// recursive cgroup memory.events file. Catches the case
			// kubelet doesn't tag.
			if hasRecentKernelOOMKill(agent, pod) {
				return EventOOMKilled, nil
			}
			// If the operator triggered a restart, route through the Restarting path
			// instead of Failed. This prevents the confusing Failed flash in the PWA.
			if agent.Spec.DesiredPhase == kyberv1.AgentPhaseRestarting {
				return EventDesiredRestarting, nil
			}
			// A pod with a RECENT DeletionTimestamp is being deleted
			// deliberately — pods never get a DeletionTimestamp from
			// crashing. The common case is our own graceful roll
			// (set-model / secret update / operator restart): the
			// roll's action patches spec (desiredPhase cleared) before
			// its status patch lands, and a reconcile triggered by that
			// spec patch can observe phase=Running + terminating pod +
			// no desiredPhase — which used to fall through to PodDied,
			// flashing Failed in the PWA, inflating restartCount and
			// emitting a false AgentCrashed warning (reproduced live on
			// the canary 2026-08-22, agent "biggs"). Wait instead; the
			// roll's own Restarting transition lands within seconds.
			// The 60s bound keeps the dead-node recovery path: a pod
			// stuck Terminating longer than that falls through to the
			// existing dead-pod handling.
			if pod != nil && pod.DeletionTimestamp != nil &&
				time.Since(pod.DeletionTimestamp.Time) < terminatingPodGraceWindow {
				return "", nil
			}
			// Suspicious-but-uncertain: agent died with exit 137 and no
			// kernel-OOM signal yet. Could be (a) an OOM the sidecar will
			// post any moment, or (b) an unrelated SIGKILL. Wait briefly
			// for the sidecar's POST. The DesiredRestarting branch above
			// takes precedence so an operator-triggered restart still
			// flows through the right path.
			if isAgentExitCode137(pod) && agentTerminatedWithin(pod, oomDetectionWindow) {
				return "", nil
			}
			return EventPodDied, nil
		}

	case kyberv1.AgentPhaseStopping:
		if pod == nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			return EventPodTerminated, nil
		}
		// Check grace period (30s).
		if agent.Status.LastTransition != nil {
			if time.Since(agent.Status.LastTransition.Time) > 30*time.Second {
				return EventGracePeriodExceeded, nil
			}
		}
		// Still terminating — requeue.
		return "", nil

	case kyberv1.AgentPhaseRestarting:
		if pod == nil {
			return EventPodDeleted, nil
		}
		// Pod still exists — requeue and wait for deletion.
		return "", nil

	case kyberv1.AgentPhaseWaitingForMachine:
		if r.isMachineReady(ctx, agent) {
			return EventMachineReady, nil
		}
		return "", nil

	// NeedsAuth and MemoryExhausted both mean "a human must supply something
	// before a retry can possibly help". Neither may leave on the bare
	// desiredPhase==Running check they used to use: that value is permanently
	// true for every normal agent, so it is not an edge, and the agent looped
	// Starting → NeedsAuth → Starting every ~20s forever, re-pulling images each
	// pass (kyber#684 — 515 pod creations in 53 minutes, measured). The retry
	// cap could not bound it either: RestartCount only increments on the
	// Failed-phase auto-restart actions, and ActionResetRetryAndCreatePod zeroes
	// it on the way through regardless.
	//
	// So gate on the operator-supplied input having actually CHANGED since the
	// last attempt. desiredPhase is still required — an operator who stopped the
	// agent must not get a surprise pod — but it is no longer sufficient.
	case kyberv1.AgentPhaseNeedsAuth:
		if agent.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
			return "", nil
		}
		changed, err := r.recoveryInputChanged(ctx, agent)
		if err != nil {
			return "", err
		}
		if changed {
			return EventDesiredRunning, nil
		}
		return "", nil

	case kyberv1.AgentPhaseMemoryExhausted:
		// Operator bumped spec.resources.memory and triggered recovery via
		// /set-resources or /start (#272). Same gate as NeedsAuth above, keyed
		// on the memory limit rather than the credential.
		if agent.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
			return "", nil
		}
		changed, err := r.recoveryInputChanged(ctx, agent)
		if err != nil {
			return "", err
		}
		if changed {
			return EventDesiredRunning, nil
		}
		return "", nil

	case kyberv1.AgentPhaseDraining:
		if pod == nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return EventPodDeleted, nil
		}
		if r.isMachinePreempted(ctx, agent, pod) {
			// Node died before drain completed.
			return EventMachinePreempted, nil
		}
		return "", nil
	}

	// Stable state — no action needed.
	return "", nil
}

// codexDeviceAuthPending reports whether a Codex subscription agent is
// intentionally waiting for the human-driven device flow. The API writes {}
// before starting that flow; the in-pod credential syncer replaces it with the
// real auth.json immediately after login. This precise marker keeps the normal
// startup timeout active for every other kind of stuck pod.
func (r *AgentReconciler) codexDeviceAuthPending(ctx context.Context, agent *kyberv1.Agent) (bool, error) {
	if agent == nil || agent.Spec.Runtime != "codex" || agent.Spec.Secrets.AuthType != kyberv1.AgentAuthTypeOAuth {
		return false, nil
	}
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: agent.Namespace, Name: agent.Name + "-codex-auth"}
	if err := r.Get(ctx, key, &secret); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading Codex device-auth secret: %w", err)
	}
	return strings.TrimSpace(string(secret.Data["auth.json"])) == "{}", nil
}

// executeAction performs the k8s operations required by the transition action.
// Returns how long to wait before requeuing (0 = no requeue, rely on watch events).
func (r *AgentReconciler) executeAction(
	ctx context.Context,
	agent *kyberv1.Agent,
	pod *corev1.Pod,
	action Action,
	event Event,
) (time.Duration, error) {
	switch action {
	case ActionCreatePVAndPod:
		// Both PVCs are ensured inside createPod, which covers every
		// pod-creation path including retry recreations, so no
		// explicit call is needed here.
		if err := r.createPod(ctx, agent); err != nil {
			return 0, err
		}
		return requeueWaiting, nil

	case ActionWaitForStart:
		return requeueWaiting, nil

	case ActionUpdateStatus:
		// Status update is handled in the caller (updatePhase) — nothing else to do.
		return 0, nil

	case ActionLogAndEmitEvent:
		r.Recorder.Eventf(agent, corev1.EventTypeWarning, "PodScheduleFailed",
			"Agent pod failed to schedule")
		return 0, nil

	case ActionKillPodAndEmitEvent:
		r.Recorder.Eventf(agent, corev1.EventTypeWarning, "StartupTimeout",
			"Agent pod did not become Ready within %s", startupTimeoutSeconds)
		if pod != nil {
			if err := r.deletePod(ctx, pod, false); err != nil {
				return 0, err
			}
		}
		return 0, nil

	case ActionSendSIGTERM:
		if pod != nil {
			if err := r.deletePod(ctx, pod, false); err != nil {
				return 0, err
			}
		}
		return requeueWaiting, nil

	case ActionCaptureStateAndDeletePod:
		// Session recall state (/persist/session-state.json) is written continuously
		// in-pod by the session-saver sidecar and read on the next boot by
		// start-claude.sh — NOT read here by the controller, which cannot read the
		// RWO /persist PVC off-node. So there is nothing to capture at this action;
		// the durable PVC carries the recall content across the recreate.
		if pod != nil {
			if err := r.deletePod(ctx, pod, false); err != nil {
				return 0, err
			}
		}
		// Clear transient Restarting intent so the next reconcile doesn't loop.
		if event == EventDesiredRestarting {
			specPatch := client.MergeFrom(agent.DeepCopy())
			agent.Spec.DesiredPhase = ""
			if err := r.Patch(ctx, agent, specPatch); err != nil {
				return 0, fmt.Errorf("clearing desiredPhase after Restarting: %w", err)
			}
		}
		return requeueWaiting, nil

	case ActionEmitEventAutoRestart:
		r.Recorder.Eventf(agent, corev1.EventTypeWarning, "AgentCrashed",
			"Agent pod died unexpectedly (restartCount=%d)", agent.Status.RestartCount)
		// Increment restart count.
		patch := client.MergeFrom(agent.DeepCopy())
		agent.Status.RestartCount++
		if err := r.Status().Patch(ctx, agent, patch); err != nil {
			return 0, fmt.Errorf("incrementing restart count: %w", err)
		}
		if telemetry.AgentRestarts != nil {
			telemetry.AgentRestarts.Add(ctx, 1,
				metric.WithAttributes(attribute.String("agent", agent.Name)))
		}
		return requeueWaiting, nil

	case ActionKillPodEmitEventAutoRestart:
		r.Recorder.Eventf(agent, corev1.EventTypeWarning, "LivenessProbeFailure",
			"Agent pod killed due to liveness probe failure (restartCount=%d)", agent.Status.RestartCount)
		if pod != nil {
			if err := r.deletePod(ctx, pod, true); err != nil {
				return 0, err
			}
		}
		patch := client.MergeFrom(agent.DeepCopy())
		agent.Status.RestartCount++
		if err := r.Status().Patch(ctx, agent, patch); err != nil {
			return 0, fmt.Errorf("incrementing restart count: %w", err)
		}
		if telemetry.AgentRestarts != nil {
			telemetry.AgentRestarts.Add(ctx, 1,
				metric.WithAttributes(attribute.String("agent", agent.Name)))
		}
		return requeueWaiting, nil

	case ActionForceKillPod:
		if pod != nil {
			if err := r.deletePod(ctx, pod, true); err != nil {
				return 0, err
			}
		}
		return requeueWaiting, nil

	case ActionWriteBriefAndCreatePod:
		// Build and store the session brief before creating the pod.
		// The init container will fetch it via GET /internal/agents/{name}/session-brief.
		r.writeBrief(ctx, agent, event)
		if err := r.createPod(ctx, agent); err != nil {
			return 0, err
		}
		return requeueWaiting, nil

	case ActionResetRetryAndCreatePod:
		patch := client.MergeFrom(agent.DeepCopy())
		agent.Status.RestartCount = 0
		if err := r.Status().Patch(ctx, agent, patch); err != nil {
			return 0, fmt.Errorf("resetting retry count: %w", err)
		}
		// Operator override (Failed→Running via /set-resources, NeedsAuth→Running
		// via re-auth) explicitly means "scrap the existing pod and recreate."
		// Sweep any pod still bound to the agent — including non-terminal ones
		// like a Pending pod stuck on FailedScheduling because of stale resource
		// requests (issue #149). createPod's safety guard at line 899 refuses to
		// clobber non-terminal pods, so we have to clear the way here. grace=0
		// is appropriate: either the pod never ran useful work (auto-retry
		// exhausted, FailedScheduling) or it already exited (OAuth refresh).
		if pod != nil {
			zero := int64(0)
			if err := r.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: &zero}); err != nil && !errors.IsNotFound(err) {
				return 0, fmt.Errorf("sweeping existing pod for reset-retry: %w", err)
			}
		}
		// Write a session brief before creating the pod — same as ActionWriteBriefAndCreatePod.
		// Shutdown type is "planned" / reason "operator" because this is an explicit operator
		// override after the agent exhausted its automatic retry limit.
		r.writeBrief(ctx, agent, event)
		if err := r.createPod(ctx, agent); err != nil {
			return 0, err
		}
		return requeueWaiting, nil

	case ActionStayFailedAndAlert:
		// Guard against event spam: only emit if we haven't already recorded this.
		if !strings.Contains(agent.Status.Message, "retry limit") {
			r.Recorder.Eventf(agent, corev1.EventTypeWarning, "RetryLimitReached",
				"Agent has exceeded maximum restart retries (%d). Manual intervention required.", maxRestartRetries)
		}
		return 0, nil

	case ActionDrainAgent:
		// Write brief with preemption context, then delete pod with short grace period.
		r.writePreemptionBrief(ctx, agent)
		if pod != nil {
			grace := int64(20)
			if err := r.Client.Delete(ctx, pod, &client.DeleteOptions{
				GracePeriodSeconds: &grace,
			}); err != nil && !errors.IsNotFound(err) {
				return 0, fmt.Errorf("draining agent pod: %w", err)
			}
		}
		r.Recorder.Eventf(agent, corev1.EventTypeWarning, "PreemptionDrain",
			"Draining agent due to machine preemption")
		return 5 * time.Second, nil

	case ActionTransitionToWaiting:
		// No retry counter increment — this is infra, not an agent bug. Remove
		// any pod pinned to the vanished node (including a still-Pending pod),
		// otherwise MachineReady would find it and createPod's safety guard would
		// refuse to build the replacement.
		if pod != nil {
			force := pod.Spec.NodeName == "" || r.isNodeUnavailable(ctx, pod.Spec.NodeName)
			if err := r.deletePod(ctx, pod, force); err != nil {
				return 0, fmt.Errorf("deleting pod while waiting for machine: %w", err)
			}
		}
		r.Recorder.Eventf(agent, corev1.EventTypeWarning, "MachineUnavailable",
			"Agent waiting for machine capacity; it will resume automatically")
		return 15 * time.Second, nil

	default:
		return 0, fmt.Errorf("unknown action: %q", action)
	}
}

// handleDeletion runs the agent cleanup finalizer.
func (r *AgentReconciler) handleDeletion(ctx context.Context, agent *kyberv1.Agent) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !containsString(agent.Finalizers, AgentFinalizer) {
		return ctrl.Result{}, nil
	}

	// If the namespace itself is Terminating, the namespace-scoped resources
	// this finalizer would otherwise clean up (Pod, PVC, Secrets) will be
	// garbage-collected by the namespace terminator anyway. Blocking on them
	// from here would deadlock: the controller's own Pod is typically being
	// torn down in the same teardown, and a dead controller can't remove
	// finalizers. Drop the finalizer fast so the terminator can finish.
	// See issue #64.
	if terminating, err := r.isNamespaceTerminating(ctx, agent.Namespace); err != nil {
		logger.Error(err, "checking namespace DeletionTimestamp (treating as not terminating)", "namespace", agent.Namespace)
	} else if terminating {
		logger.Info("namespace is Terminating; skipping in-namespace cleanup and self-removing finalizer",
			"agent", agent.Name, "namespace", agent.Namespace)
		patch := client.MergeFrom(agent.DeepCopy())
		agent.Finalizers = removeString(agent.Finalizers, AgentFinalizer)
		if err := r.Patch(ctx, agent, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing finalizer during namespace teardown: %w", err)
		}
		return ctrl.Result{}, nil
	}

	logger.Info("running agent finalizer", "agent", agent.Name)

	// Delete the agent pod if running.
	pod, err := r.getAgentPod(ctx, agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod != nil {
		if err := r.deletePod(ctx, pod, true); err != nil {
			return ctrl.Result{}, err
		}
		// Requeue to wait for pod termination before deleting PVC.
		return ctrl.Result{RequeueAfter: requeueImmediate}, nil
	}

	// Delete the PVC.
	pvc := &corev1.PersistentVolumeClaim{}
	pvcKey := types.NamespacedName{Name: PVCName(agent.Name), Namespace: agent.Namespace}
	if err := r.Get(ctx, pvcKey, pvc); err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("fetching PVC for deletion: %w", err)
		}
		// Already gone — continue.
	} else {
		if err := r.Delete(ctx, pvc); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting PVC: %w", err)
		}
		logger.Info("deleted agent PVC", "agent", agent.Name, "pvc", pvc.Name)
	}

	// Delete agent-scoped secrets (oauth, telegram, anthropic). These are
	// labeled kyber.io/agent=<name> by createAgentSecrets. Missing here means
	// they accumulate as orphans — see issue #1.
	secretList := &corev1.SecretList{}
	if err := r.List(ctx, secretList,
		client.InNamespace(agent.Namespace),
		client.MatchingLabels{"kyber.io/agent": agent.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing secrets for cleanup: %w", err)
	}
	for i := range secretList.Items {
		sec := &secretList.Items[i]
		if err := r.Delete(ctx, sec); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting secret %s: %w", sec.Name, err)
		}
		logger.Info("deleted agent secret", "agent", agent.Name, "secret", sec.Name)
	}

	// B2 (kyber#565): reap the agent's orphan-prone state in the external stores
	// so a confirmed delete leaves zero leaked identity material (AC-5). The
	// on-PVC git clone is already gone with the PVC above; the remote GitHub
	// identity repo is deliberately NOT deleted (high-blast-radius, Matt's locked
	// decision). Every store delete is idempotent, so a requeue-driven retry
	// never double-applies. If a store is durably unreachable we do NOT wedge
	// deletion forever — bound the retries, then complete deletion while emitting
	// a loud OrphanCleanupIncomplete Event naming what couldn't be reaped.
	// B3 will add: adapter-specific cleanup (e.g., revoke OAuth token).
	if unreaped := r.cleanupAgentStores(ctx, agent); len(unreaped) > 0 {
		attempts := orphanCleanupAttempts(agent) + 1
		if attempts < orphanCleanupMaxAttempts {
			if err := r.setOrphanCleanupAttempts(ctx, agent, attempts); err != nil {
				return ctrl.Result{}, fmt.Errorf("recording orphan-cleanup attempt: %w", err)
			}
			logger.Info("orphan cleanup incomplete; requeueing",
				"agent", agent.Name, "attempt", attempts, "max", orphanCleanupMaxAttempts,
				"unreaped", strings.Join(unreaped, ","))
			return ctrl.Result{RequeueAfter: requeueOrphanCleanup}, nil
		}
		// Bounded give-up: complete deletion but make the orphan loud so an
		// operator can reconcile the leaked rows manually.
		r.Recorder.Eventf(agent, corev1.EventTypeWarning, "OrphanCleanupIncomplete",
			"gave up reaping agent state after %d attempts; unreaped: %s — manual cleanup may be required",
			attempts, strings.Join(unreaped, ", "))
		logger.Error(fmt.Errorf("unreaped stores: %s", strings.Join(unreaped, ",")),
			"orphan cleanup gave up after bounded retries; completing deletion anyway",
			"agent", agent.Name, "attempts", attempts)
	}

	// Remove finalizer to allow CRD deletion to complete.
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Finalizers = removeString(agent.Finalizers, AgentFinalizer)
	if err := r.Patch(ctx, agent, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	r.Recorder.Eventf(agent, corev1.EventTypeNormal, "AgentDeleted",
		"Agent cleanup complete — pod, PVC, secrets, and external store state deleted; finalizer removed")

	return ctrl.Result{}, nil
}

// cleanupAgentStores reaps all of an agent's owned state in the external stores
// (Postgres brief + the Redis token-usage/accumulator/time-series keys) so
// a confirmed delete leaves zero orphaned identity material (kyber#565 AC-5).
//
// Each store is optional: a nil handle means that backend isn't configured, so
// there is nothing of its kind to orphan and it is skipped. Every delete is
// idempotent, so this is safe to re-run on a finalizer requeue. It does not stop
// at the first error — it attempts every store and returns the labels of the
// ones that failed, so the caller can apply the bounded-retry-then-give-up
// policy and name exactly what was left behind.
func (r *AgentReconciler) cleanupAgentStores(ctx context.Context, agent *kyberv1.Agent) []string {
	ns := agent.Namespace
	name := agent.Name

	type storeDelete struct {
		label string
		del   func() error
	}
	deletes := []storeDelete{
		{"brief", func() error {
			if r.BriefStore == nil {
				return nil
			}
			return r.BriefStore.Delete(ctx, name)
		}},
		{"token-usage", func() error {
			if r.TokenStore == nil {
				return nil
			}
			return r.TokenStore.Delete(ctx, name)
		}},
		{"token-accumulator", func() error {
			if r.TokenAccumulator == nil {
				return nil
			}
			return r.TokenAccumulator.DeleteAgent(ctx, ns, name)
		}},
		{"state-change-accumulator", func() error {
			if r.StateChangeAccumulator == nil {
				return nil
			}
			return r.StateChangeAccumulator.DeleteAgent(ctx, ns, name)
		}},
		{"metrics-timeseries", func() error {
			if r.MetricsStore == nil {
				return nil
			}
			return r.MetricsStore.DeleteAgent(ctx, ns, name)
		}},
		{"skills", func() error {
			if r.SkillStore == nil {
				return nil
			}
			return r.SkillStore.Delete(ctx, name)
		}},
	}

	var unreaped []string
	for _, d := range deletes {
		if err := d.del(); err != nil {
			log.FromContext(ctx).Error(err, "failed to reap agent store on delete",
				"agent", name, "store", d.label)
			unreaped = append(unreaped, d.label)
		}
	}
	return unreaped
}

// orphanCleanupAttempts reads the recorded count of failed orphan-cleanup
// attempts from the agent's annotations (0 if absent/unparseable).
func orphanCleanupAttempts(agent *kyberv1.Agent) int {
	if agent.Annotations == nil {
		return 0
	}
	n, err := strconv.Atoi(agent.Annotations[orphanCleanupAttemptsAnnotation])
	if err != nil {
		return 0
	}
	return n
}

// setOrphanCleanupAttempts persists the failed-attempt counter on the agent so
// the bounded-give-up policy survives across finalizer requeues. Patching a
// being-deleted object is allowed while the finalizer still holds it.
func (r *AgentReconciler) setOrphanCleanupAttempts(ctx context.Context, agent *kyberv1.Agent, n int) error {
	patch := client.MergeFrom(agent.DeepCopy())
	if agent.Annotations == nil {
		agent.Annotations = map[string]string{}
	}
	agent.Annotations[orphanCleanupAttemptsAnnotation] = strconv.Itoa(n)
	return r.Patch(ctx, agent, patch)
}

// isNamespaceTerminating reports whether the given namespace has been marked
// for deletion. A missing namespace is treated as "not terminating" so the
// caller can log and continue with the normal path.
func (r *AgentReconciler) isNamespaceTerminating(ctx context.Context, name string) (bool, error) {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, ns); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return !ns.DeletionTimestamp.IsZero(), nil
}

// ensureFinalizer adds the agent finalizer if not already present.
func (r *AgentReconciler) ensureFinalizer(ctx context.Context, agent *kyberv1.Agent) error {
	if containsString(agent.Finalizers, AgentFinalizer) {
		return nil
	}
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Finalizers = append(agent.Finalizers, AgentFinalizer)
	return r.Patch(ctx, agent, patch)
}

// migrateLegacyDiscordAction advances Kyber-generated Discord action text and
// adds canonical attachment extraction to bindings created before file
// support. Operator-authored action text and fields are preserved.
//
// Scoped deliberately narrowly:
//   - only agents with the Discord channel actually enabled,
//   - only the binding named `discord`,
//   - only generated action variants recognized by
//     IsLegacyDiscordDefaultAction are rewritten.
//
// Idempotent: once rewritten the action no longer matches, so later reconciles
// are a no-op and this costs one map-free scan of a short slice.
func (r *AgentReconciler) migrateLegacyDiscordAction(ctx context.Context, agent *kyberv1.Agent) error {
	if agent.Spec.Channels == nil || agent.Spec.Channels.Discord == nil {
		return nil
	}
	idx := -1
	for i := range agent.Spec.InboundBindings {
		if agent.Spec.InboundBindings[i].Name == DiscordInboundBindingName {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}

	binding := &agent.Spec.InboundBindings[idx]
	patch := client.MergeFrom(agent.DeepCopy())
	changed := false
	if IsLegacyDiscordDefaultAction(binding.Action) {
		binding.Action = DefaultDiscordAction(agent.Spec.Channels.Discord.MentionOnly)
		changed = true
	}
	requiredFields := []kyberv1.AgentInboundField{
		{Label: "attachments", JsonPath: "$.attachments"},
		{Label: "thread_id", JsonPath: "$.thread_id"},
		{Label: "thread_name", JsonPath: "$.thread_name"},
		{Label: "parent_channel_id", JsonPath: "$.parent_channel_id"},
		{Label: "referenced_message", JsonPath: "$.referenced_message"},
		{Label: "recent_context", JsonPath: "$.recent_context"},
	}
	for _, required := range requiredFields {
		found := false
		for _, field := range binding.Fields {
			if field.Label == required.Label && field.JsonPath == required.JsonPath {
				found = true
				break
			}
		}
		if !found {
			binding.Fields = append(binding.Fields, required)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := r.Patch(ctx, agent, patch); err != nil {
		return fmt.Errorf("migrating Discord binding: %w", err)
	}
	log.FromContext(ctx).Info(
		"migrated Discord binding to the current MCP action and attachment fields",
		"agent", agent.Name, "binding", DiscordInboundBindingName)
	return nil
}

// ensureUserSecrets creates the two user-secret shell Secrets for the agent
// if they do not already exist (#75). Both are owner-ref'd to the Agent CR
// and labeled kyber.io/agent=<name> so the existing deletion finalizer
// (see handleDeletion) garbage-collects them when the Agent is deleted.
//
// This runs on every reconcile rather than only on first agent creation so
// a manually deleted Secret is restored on the next reconcile — user-secrets
// must always be present for the pod's unconditional mounts to resolve.
func (r *AgentReconciler) ensureUserSecrets(ctx context.Context, agent *kyberv1.Agent) error {
	for _, name := range []string{
		UserSecretKVName(agent.Name),
		UserSecretFilesName(agent.Name),
	} {
		sec := &corev1.Secret{}
		key := types.NamespacedName{Name: name, Namespace: agent.Namespace}
		if err := r.Get(ctx, key, sec); err == nil {
			continue
		} else if !errors.IsNotFound(err) {
			return fmt.Errorf("checking user-secret %s: %w", name, err)
		}

		newSec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: agent.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "kyber-controller",
					"kyber.io/agent":               agent.Name,
					"kyber.io/secret-kind":         "user-secrets",
				},
			},
			Type: corev1.SecretTypeOpaque,
		}
		if err := ctrl.SetControllerReference(agent, newSec, r.Scheme); err != nil {
			return fmt.Errorf("setting user-secret %s owner reference: %w", name, err)
		}
		if err := r.Create(ctx, newSec); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("creating user-secret %s: %w", name, err)
		}
	}
	return nil
}

// ensurePodTokenSecret mints the agent's control-plane-signed pod-token into a
// per-agent Secret (kyber#566). The Secret is labeled kyber.io/agent=<name> and
// owner-ref'd to the Agent so the existing deletion finalizer (handleDeletion)
// GCs it — no new orphan path. pod_builder.go mounts it at PodTokenMountDir so
// the pod's clients can present it as a Bearer to the internal API, which
// enforces act-on-self-only against the signed identity.
//
// No-op when PodTokenKey is empty (no signing key delivered yet): the Secret is
// not created. Combined with the Optional pod-token volume in pod_builder.go and
// the internal API's grace mode, this keeps the fleet working through the
// staged rollout (mint+mount first, flip enforcement after the re-roll).
//
// Idempotent and rotation-safe: if the Secret already holds the current signed
// token it is left untouched; if the signing key rotated (the stored token no
// longer matches), the value is updated in place.
func (r *AgentReconciler) ensurePodTokenSecret(ctx context.Context, agent *kyberv1.Agent) error {
	if len(r.PodTokenKey) == 0 {
		return nil
	}
	token := podtoken.Sign(agent.Name, r.PodTokenKey)
	name := PodTokenSecretName(agent.Name)
	key := types.NamespacedName{Name: name, Namespace: agent.Namespace}

	existing := &corev1.Secret{}
	err := r.Get(ctx, key, existing)
	if err == nil {
		// Already present — update only if the token changed (key rotation).
		if string(existing.Data[PodTokenSecretKey]) == token {
			return nil
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data[PodTokenSecretKey] = []byte(token)
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("updating pod-token secret %s: %w", name, err)
		}
		return nil
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("checking pod-token secret %s: %w", name, err)
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: agent.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kyber-controller",
				"kyber.io/agent":               agent.Name,
				"kyber.io/secret-kind":         "pod-token",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{PodTokenSecretKey: []byte(token)},
	}
	if err := ctrl.SetControllerReference(agent, sec, r.Scheme); err != nil {
		return fmt.Errorf("setting pod-token secret %s owner reference: %w", name, err)
	}
	if err := r.Create(ctx, sec); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating pod-token secret %s: %w", name, err)
	}
	return nil
}

// ensurePVC creates the agent's PVC if it does not already exist and reconciles
// monotonic storage increases onto an existing claim. Kubernetes forbids PVC
// shrink, so a lower Agent request deliberately leaves the claim untouched.
func (r *AgentReconciler) ensurePVC(ctx context.Context, agent *kyberv1.Agent) error {
	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: PVCName(agent.Name), Namespace: agent.Namespace}
	if err := r.Get(ctx, key, pvc); err == nil {
		requested := agent.Spec.Resources.Disk
		current := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		if requested.IsZero() || requested.Cmp(current) <= 0 {
			return nil
		}
		patch := client.MergeFrom(pvc.DeepCopy())
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = requested
		if err := r.Patch(ctx, pvc, patch); err != nil {
			return fmt.Errorf("expanding PVC from %s to %s: %w", current.String(), requested.String(), err)
		}
		return nil
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("checking PVC: %w", err)
	}

	newPVC := BuildPVC(agent, r.AgentStorageClass)
	if err := ctrl.SetControllerReference(agent, newPVC, r.Scheme); err != nil {
		return fmt.Errorf("setting PVC owner reference: %w", err)
	}
	if err := r.Create(ctx, newPVC); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating PVC: %w", err)
	}

	// Creating this claim for an agent that has ALREADY RUN means its previous
	// volume is gone, and the agent is about to boot Running with an empty
	// /persist: no identity-repo checkout, no session state, nothing it wrote
	// before. Sometimes that is intended (an operator deleted the claim to
	// change its immutable StorageClass); sometimes it is a local-path volume
	// that went with a node, or a mis-targeted `kubectl delete pvc`.
	//
	// The reconciler cannot tell those apart, and it should still recreate the
	// claim either way — an agent stuck forever is worse. But silence would
	// mean data loss presenting as a perfectly healthy agent, with every status
	// surface green. So say it out loud, once, where an operator will see it.
	if r.Recorder != nil && (agent.Status.RestartCount > 0 || agent.Status.Phase != "") {
		r.Recorder.Eventf(agent, corev1.EventTypeWarning, "PersistVolumeRecreated",
			"Recreated missing persistent volume %q for an agent that has run before — it will start with empty storage. "+
				"If you did not delete this claim deliberately, its previous contents are gone.",
			PVCName(agent.Name))
	}
	return nil
}

// ensureDiskRecoveryCapacity starts any requested expansion and reports true
// only after Kubernetes exposes at least that much capacity on the claim.
func (r *AgentReconciler) ensureDiskRecoveryCapacity(ctx context.Context, agent *kyberv1.Agent) (bool, error) {
	if err := r.ensurePVC(ctx, agent); err != nil {
		return false, err
	}
	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: PVCName(agent.Name), Namespace: agent.Namespace}
	if err := r.Get(ctx, key, pvc); err != nil {
		return false, fmt.Errorf("reading PVC expansion status: %w", err)
	}
	requested := agent.Spec.Resources.Disk
	claimRequest := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	capacity := pvc.Status.Capacity[corev1.ResourceStorage]
	return !requested.IsZero() && claimRequest.Cmp(requested) >= 0 && capacity.Cmp(requested) >= 0, nil
}

// ensureOffsetsPVC creates the agent's durable transcript-offsets PVC if it does
// not already exist (kyber#467). This is a separate, tiny RWO PVC from the
// persist PVC: it holds the transcript-tailer's per-file line-count checkpoints
// so they survive pod recreation (no full-backlog re-ship), while the persist
// PVC stays read-only (#446). It MUST be ensured before the pod is created —
// the pod spec references it as a volume (AppendTranscriptTailer), so a missing
// claim would leave the pod unschedulable. Owner-referenced to the agent so it
// is garbage-collected on agent deletion, mirroring ensurePVC.
func (r *AgentReconciler) ensureOffsetsPVC(ctx context.Context, agent *kyberv1.Agent) error {
	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: OffsetsPVCName(agent.Name), Namespace: agent.Namespace}
	if err := r.Get(ctx, key, pvc); err == nil {
		// Already exists.
		return nil
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("checking offsets PVC: %w", err)
	}

	newPVC := BuildOffsetsPVC(agent, r.TranscriptOffsetsStorageClass, r.TranscriptOffsetsSize)
	if err := ctrl.SetControllerReference(agent, newPVC, r.Scheme); err != nil {
		return fmt.Errorf("setting offsets PVC owner reference: %w", err)
	}
	if err := r.Create(ctx, newPVC); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating offsets PVC: %w", err)
	}
	return nil
}

// createPod builds and applies the agent pod.
// It resolves the adapter from the registry and looks up the node name from the Machine CRD.
func (r *AgentReconciler) createPod(ctx context.Context, agent *kyberv1.Agent) error {
	adapter, err := r.resolveAdapter(agent)
	if err != nil {
		return err
	}

	// kyber#674: refuse to build a pod when the runtime has no image
	// configured on this install. Without this guard the empty string flows
	// into containers[0].image (BuildPodSpec below), the API server rejects
	// the pod, and Reconcile returns before updatePhase — leaving a blank
	// status and a ~20s error loop with the reason visible only in the
	// control-plane log. Surface it as a condition instead, the same way an
	// unresolvable model does, so the operator sees the actual remediation.
	//
	// Runs BEFORE ensureOffsetsPVC deliberately: an agent whose runtime this
	// cluster cannot launch should not leave a provisioned PVC behind on every
	// one of those retries.
	if err := r.reconcileRuntimeImageCondition(ctx, agent, adapter); err != nil {
		return err
	}

	// Ensure BOTH PVCs exist before building the pod — the pod spec references
	// each as a volume, so a missing claim leaves the pod stuck Pending.
	//
	// kyber#467 established this for the offsets PVC: it MUST live in createPod
	// rather than only in the ActionCreatePVAndPod case, so EVERY pod-creation
	// path is covered — an agent recreated via ActionWriteBriefAndCreatePod
	// or ActionResetRetryAndCreatePod (retry) never runs the birth-time
	// ensure.
	//
	// The persist PVC needed the same treatment and did not have it, which made
	// a missing volume unrecoverable: the reconciler rebuilt the POD forever
	// while nothing recreated the CLAIM, so the agent flapped
	// Starting → Failed → Starting with "persistentvolumeclaim not found" and
	// no path back short of deleting the agent or hand-writing the PVC. Hit for
	// real on the canary, recovering from the kyber-pd StorageClass default.
	//
	// Both are idempotent (Get-then-Create) and never touch an existing claim,
	// so the birth path is unaffected and a bound volume is never modified.
	if err := r.ensurePVC(ctx, agent); err != nil {
		return err
	}
	if err := r.ensureOffsetsPVC(ctx, agent); err != nil {
		return err
	}

	nodeName, err := r.resolveNodeName(ctx, agent)
	if err != nil {
		return err
	}

	// Resolve runtime-scoped fleet defaults. If both model layers are empty,
	// the adapter deliberately omits a model selection and the harness chooses
	// its current default.
	resolved, resolveErr := r.resolveAgentForPod(ctx, agent)
	if resolveErr != nil {
		return resolveErr
	}

	podSpec, err := BuildPodSpec(resolved, adapter, nodeName)
	if err != nil {
		return fmt.Errorf("building pod spec: %w", err)
	}

	// Inject the platform-managed status sidecar (kyber#248). When the
	// image ref is unset (dev installs / tests), this is a no-op.
	// OtelEndpoint + Runtime drive per-agent metrics (kyber#256) — both
	// tolerate empty (dev installs / older deployments simply emit no
	// metrics rather than crashing).
	AppendStatusSidecar(&podSpec, SidecarConfig{
		AgentName:    agent.Name,
		Image:        r.StatusSidecarImage,
		DiskBytes:    agent.Spec.Resources.Disk.Value(),
		OtelEndpoint: r.SidecarOtelEndpoint,
		Runtime:      agent.Spec.Runtime,
		LogLevel:     r.SidecarLogLevel,
	})

	// Inject the Discord channel sidecar (kyber#646) when the agent enables
	// the Discord channel. No-op when the image ref or the channel is unset —
	// same image-guard pattern as the status sidecar.
	if agent.Spec.Channels != nil && agent.Spec.Channels.Discord != nil {
		AppendDiscordSidecar(&podSpec, DiscordSidecarConfig{
			AgentName:      agent.Name,
			Image:          r.DiscordSidecarImage,
			ExistingSecret: agent.Spec.Channels.Discord.ExistingSecret,
			MentionOnly:    agent.Spec.Channels.Discord.MentionOnly,
			LogLevel:       r.DiscordLogLevel,
		})
	}
	// Runtime-neutral since kyber#684: every agent with Telegram enabled gets
	// this sidecar, not just Codex. It was Codex-only because Codex has no
	// plugin system while Claude Code does — but that left two implementations
	// of one channel, and the in-process plugin is the half that carries the
	// reboot-409 race (#678/#679). The sidecar now speaks MCP, so a Claude Code
	// agent keeps its tool interface (reply/react/edit_message/
	// download_attachment) rather than dropping to curl.
	//
	// The plugin is disabled on the runtime side whenever this is injected
	// (KYBER_TELEGRAM_SIDECAR in the claude-code adapter env) — two pollers on
	// one bot token is a guaranteed 409 storm, so the two must never coexist.
	if agent.Spec.Secrets.TelegramEnabled {
		AppendTelegramSidecar(&podSpec, TelegramSidecarConfig{
			AgentName: agent.Name, Image: r.TelegramSidecarImage,
			ExistingSecret: agent.Name + "-telegram",
			LogLevel:       r.TelegramLogLevel,
		})
	}

	// Inject the transcript-tailer sidecar (kyber#446): ships the agent's
	// Claude Code session JSONL off the PVC on a clean, isolated stream for the
	// ?source=transcript archive surface. It reuses the agent's OWN runtime
	// image (the first container's image, just assembled by BuildPodSpec) so it
	// runs as the same uid that owns the JSONL — required to read the read-only
	// PVC mount without loosening perms. No-op when the runtime image is unset
	// (dev installs / tests), same guard pattern as the status sidecar.
	AppendTranscriptTailer(&podSpec, TranscriptTailerConfig{
		AgentName:    agent.Name,
		RuntimeImage: podSpec.Containers[0].Image,
		Runtime:      agent.Spec.Runtime,
	})

	// Inject the session-saver sidecar (session-continuity): maintains the
	// /persist/session-state.json recall snapshot (last activity + recent turns)
	// so a recreated pod recalls what the prior session was doing. Separate
	// RW-mounted container (like the pruner) so the tailer's read-only mount
	// (kyber#446) is untouched; writes only to the durable persist PVC, which
	// survives recreation, so no new PVC and no cross-node controller read is
	// needed. No-op when the runtime image is unset (same guard as the tailer).
	AppendSessionSaver(&podSpec, SessionSaverConfig{
		AgentName:    agent.Name,
		RuntimeImage: podSpec.Containers[0].Image,
		Runtime:      agent.Spec.Runtime,
	})

	// Inject the transcript-pruner sidecar (kyber#471): bounds the on-PVC
	// transcript backlog by removing already-archived session JSONL past the
	// retention policy. Separate RW-mounted container so the tailer's read-only
	// mount (kyber#446) is untouched. No-op unless retention is enabled with a
	// positive age policy and a runtime image (same guard family as the tailer).
	AppendTranscriptPruner(&podSpec, TranscriptPrunerConfig{
		AgentName:            agent.Name,
		RuntimeImage:         podSpec.Containers[0].Image,
		Runtime:              agent.Spec.Runtime,
		Enabled:              r.TranscriptRetentionEnabled,
		MaxAgeDays:           r.TranscriptRetentionMaxAgeDays,
		MaxBytesPerAgent:     r.TranscriptRetentionMaxBytesPerAgent,
		PruneIntervalMinutes: r.TranscriptPruneIntervalMinutes,
		ArchiveCrosscheck:    r.TranscriptRetentionArchiveCrosscheck,
	})

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentPodName(agent.Name),
			Namespace: agent.Namespace,
			Labels:    AgentPodLabels(agent, adapter),
			Annotations: map[string]string{
				DiscordConfigRevisionAnnotation: agent.Annotations[DiscordConfigRevisionAnnotation],
			},
		},
		Spec: podSpec,
	}

	if err := ctrl.SetControllerReference(agent, pod, r.Scheme); err != nil {
		return fmt.Errorf("setting pod owner reference: %w", err)
	}

	// Sweep any stale terminal pod blocking Create. This happens after an
	// OAuth refresh failure or any exit-on-startup path: the pod lands in
	// Failed state and is never deleted, so a subsequent ResetRetryAndCreatePod
	// would have silently no-op'd (Create → AlreadyExists → ignored) and the
	// stale pod would outlive the re-auth, leaving the fresh refresh_token in
	// the secret permanently unread. Only sweeps terminal phases AND the
	// orphaned-Terminating-on-dead-node case; a live pod on a healthy node
	// here signals an upstream state-machine bug and we fail loud.
	existing := &corev1.Pod{}
	podKey := types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}
	switch err := r.Get(ctx, podKey, existing); {
	case err == nil:
		if existing.Status.Phase != corev1.PodFailed && existing.Status.Phase != corev1.PodSucceeded {
			// Special-case: pod is Terminating (DeletionTimestamp set) but
			// kubelet on its assigned node is gone, so the deletion will
			// never complete on its own. Without this branch the agent
			// reconciler loops forever on "existing pod in non-terminal
			// phase Running" — see kyber-gcp incident 2026-04-27, where a
			// preempted spot VM left a pod stuck Terminating for 26+ hours.
			if r.isOrphanedTerminating(ctx, existing) {
				zero := int64(0)
				if err := r.Delete(ctx, existing, &client.DeleteOptions{GracePeriodSeconds: &zero}); err != nil && !errors.IsNotFound(err) {
					return fmt.Errorf("force-deleting orphaned terminating pod: %w", err)
				}
				log.FromContext(ctx).Info("force-deleted orphaned Terminating pod on dead node",
					"pod", existing.Name, "node", existing.Spec.NodeName)
				if r.Recorder != nil {
					r.Recorder.Eventf(agent, corev1.EventTypeWarning, "OrphanedPodForceDeleted",
						"Force-deleted pod %s stuck Terminating on dead/missing node %s",
						existing.Name, existing.Spec.NodeName)
				}
				// Fall through to the create below.
				break
			}
			return fmt.Errorf("cannot create pod %s: existing pod in non-terminal phase %s", pod.Name, existing.Status.Phase)
		}
		zero := int64(0)
		if err := r.Delete(ctx, existing, &client.DeleteOptions{GracePeriodSeconds: &zero}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleting stale terminal pod: %w", err)
		}
	case !errors.IsNotFound(err):
		return fmt.Errorf("checking for existing pod: %w", err)
	}

	if err := r.Create(ctx, pod); err != nil {
		return fmt.Errorf("creating pod: %w", err)
	}
	// Stamp the spec generation we just rolled into a pod. The PWA derives
	// its "restart required" badge from metadata.generation > status.observedGeneration
	// (kyber#157 PR-A); spec changes that don't trigger another createPod
	// (e.g. spec.resources today, see kyber#149) will leave it lagging until
	// the operator rolls the pod manually. Failure here is intentionally
	// non-fatal — the pod is already up and the next createPod will retry
	// the stamp; over-reporting "restart required" is preferable to losing
	// the just-created pod.
	if err := r.stampObservedGeneration(ctx, agent); err != nil {
		log.FromContext(ctx).Error(err, "stamping observedGeneration after pod create",
			"agent", agent.Name, "generation", agent.Generation)
	}
	return nil
}

// restartRequestChanged reports whether the operator has issued a NEW
// instruction to a Failed agent since the last one this controller acted on,
// and claims that instruction so it cannot be acted on twice.
//
// The instruction is a spec edit (Agent.SpecChangedSinceLastPod); the claim is
// stamping observedGeneration up to generation, which is the same write
// createPod makes a moment later — so on the happy path this only moves the
// stamp earlier, and the createPod stamp becomes a no-op.
//
// Claiming here rather than trusting that later stamp is the point. createPod's
// stamp is intentionally non-fatal: it runs after the pod exists, and losing a
// live pod to a status-patch error would be a bad trade for a stale badge. But
// an unclaimed generation is a permanently-open gate, and the action behind
// this gate resets restartCount, so the retry cap cannot bound what the gate
// lets through. If the claim cannot be written we return the error and derive
// no event: the reconcile requeues, the agent stays Failed, and the retry cap
// still governs. Same shape as recoveryInputChanged (kyber#684).
func (r *AgentReconciler) restartRequestChanged(ctx context.Context, agent *kyberv1.Agent) (bool, error) {
	if !agent.SpecChangedSinceLastPod() {
		return false, nil
	}
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.ObservedGeneration = agent.Generation
	if err := r.Status().Patch(ctx, agent, patch); err != nil {
		return false, fmt.Errorf("claiming restart request generation: %w", err)
	}
	return true, nil
}

// stampObservedGeneration patches agent.status.observedGeneration to the
// current metadata.generation. Called after a successful pod create/recreate
// so the PWA can derive "restart required" as generation > observedGeneration.
// No-op when the value is already current.
func (r *AgentReconciler) stampObservedGeneration(ctx context.Context, agent *kyberv1.Agent) error {
	if agent.Status.ObservedGeneration == agent.Generation {
		return nil
	}
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.ObservedGeneration = agent.Generation
	return r.Status().Patch(ctx, agent, patch)
}

// isOrphanedTerminating reports whether `pod` is Terminating on a node that
// the kubelet can no longer drain — either the Node object is gone, or the
// Node's Ready condition is False or unknown. In that state the pod will
// never reach a terminal phase on its own (kubelet is the only writer), and
// the agent reconciler must force-delete it to recover.
//
// The Terminating-grace gate (60s) keeps a pod that's mid-graceful-stop
// from being yanked the instant its NotReady-toleration kicks in — the
// happy path is for kubelet to ack the delete within seconds.
func (r *AgentReconciler) isOrphanedTerminating(ctx context.Context, pod *corev1.Pod) bool {
	if pod.DeletionTimestamp == nil {
		return false
	}
	if time.Since(pod.DeletionTimestamp.Time) < 60*time.Second {
		return false
	}
	if pod.Spec.NodeName == "" {
		// No node was ever assigned — odd Terminating state, but a force-delete
		// is safe (no kubelet to break things on).
		return true
	}
	node := &corev1.Node{}
	err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node)
	if errors.IsNotFound(err) {
		return true
	}
	if err != nil {
		// Conservative: if we can't read the node, don't force-delete. The
		// next reconcile will retry and (likely) succeed.
		return false
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			// Ready=True → kubelet is alive, let it drain naturally.
			// Anything else (False, Unknown) → kubelet is gone or stuck.
			return c.Status != corev1.ConditionTrue
		}
	}
	// No Ready condition at all → treat as not-ready.
	return true
}

// deletePod deletes the agent pod. If force is true, the grace period is set to 0.
func (r *AgentReconciler) deletePod(ctx context.Context, pod *corev1.Pod, force bool) error {
	opts := []client.DeleteOption{}
	if force {
		gracePeriod := int64(0)
		opts = append(opts, client.GracePeriodSeconds(gracePeriod))
	}
	if err := r.Delete(ctx, pod, opts...); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting pod: %w", err)
	}
	return nil
}

// getAgentPod returns the agent's current pod, or nil if it does not exist.
func (r *AgentReconciler) getAgentPod(ctx context.Context, agent *kyberv1.Agent) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	key := types.NamespacedName{Name: AgentPodName(agent.Name), Namespace: agent.Namespace}
	if err := r.Get(ctx, key, pod); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fetching pod: %w", err)
	}
	return pod, nil
}

// updatePhase patches the agent's status.phase and updates LastTransition.
// When transitioning to Running for the first time (StartTime == nil), also sets StartTime.
func (r *AgentReconciler) updatePhase(
	ctx context.Context,
	agent *kyberv1.Agent,
	newPhase kyberv1.AgentPhase,
	message string,
) error {
	if agent.Status.Phase == newPhase && agent.Status.Message == message {
		return nil // nothing to update
	}
	// Capture the patch base BEFORE mutating — all mutations below appear in the diff.
	patch := client.MergeFrom(agent.DeepCopy())
	now := metav1.Now()
	if newPhase == kyberv1.AgentPhaseWaitingForMachine && message == "" {
		agent.Status.Message = fmt.Sprintf("Waiting for machine %s to recover. Kyber will resume this agent automatically.", agent.Spec.Machine)
	} else if message == "" {
		agent.Status.Message = ""
	} else {
		agent.Status.Message = message
	}
	agent.Status.Phase = newPhase
	agent.Status.LastTransition = &now
	// Baseline the disk request on entry so a hard-full terminal pod cannot
	// consume the standing desiredPhase=Running as an immediate retry. Only a
	// later size change may unlock the bounded recreation path.
	if newPhase == kyberv1.AgentPhaseDiskExhausted {
		agent.Status.RecoveryInput = "disk=" + agent.Spec.Resources.Disk.String()
	}
	// Record when the agent first enters Running so ShouldResetRetryCount has a reference.
	if newPhase == kyberv1.AgentPhaseRunning && agent.Status.StartTime == nil {
		agent.Status.StartTime = &now
	}
	// kyber#210: clear the scheduling-failure status when the agent is up.
	// The PWA banner keys off this field; leaving it stale would render
	// "stuck Pending" on a Running agent.
	if newPhase == kyberv1.AgentPhaseRunning || newPhase == kyberv1.AgentPhaseWaitingForMachine {
		clearSchedulingStatus(agent)
	}
	return r.Status().Patch(ctx, agent, patch)
}

// resolveAgentForPod returns a deep copy of agent with spec fields filled
// in from fleet defaults:
//
//   - spec.Model: if empty, populated from the runtime-specific model key.
//   - spec.RuntimeVersion: if empty, populated from the runtime-specific
//     harness-version key. The legacy fields are Claude Code defaults and must
//     not leak into Codex agents. Unlike model, an empty
//     resolved runtimeVersion is **valid** — start-claude.sh treats it as
//     "use the baked-in version," which is the existing behavior. So no
//     condition fires when both spec and default are empty here.
//
// An empty resolved model is valid: both supported harnesses interpret it as
// "use the harness default." Any stale ModelUnresolved condition from an older
// control plane is cleared. The
// original `agent` object is mutated only for the condition write — the
// returned copy is what the pod builder consumes, so any spec mutation
// stays local to this reconcile pass.
//
// Returns an error (non-nil) when resolution can't proceed (e.g., a ConfigMap
// read failure that isn't a NotFound).
func (r *AgentReconciler) resolveAgentForPod(ctx context.Context, agent *kyberv1.Agent) (*kyberv1.Agent, error) {
	resolved := agent.DeepCopy()

	var defaults fleetdefaults.Defaults
	if r.FleetDefaults != nil {
		d, err := r.FleetDefaults.Resolve(ctx)
		if err != nil {
			// Surface as a transient error — let the next reconcile retry.
			// Don't flip the ModelUnresolved condition: we don't know yet
			// whether the eventual answer is "default present" or "default
			// empty," and flapping that flag in the PWA would be worse than
			// a brief log line.
			return nil, fmt.Errorf("resolving fleet defaults: %w", err)
		}
		defaults = d
	}

	if resolved.Spec.Model == "" {
		switch resolved.Spec.Runtime {
		case "codex":
			resolved.Spec.Model = defaults.CodexModel
		default:
			resolved.Spec.Model = defaults.Model
		}
	}
	if resolved.Spec.RuntimeVersion == "" {
		switch resolved.Spec.Runtime {
		case "codex":
			resolved.Spec.RuntimeVersion = defaults.CodexRuntimeVersion
		default:
			// Mirrors the model switch above: anything that is not Codex falls
			// back to the legacy (Claude Code) fleet default. Listing
			// "claude-code" alone would silently drop fleet-default version
			// resolution for any runtime added later — a failure that surfaces
			// only as an agent quietly running the image's baked-in version.
			resolved.Spec.RuntimeVersion = defaults.RuntimeVersion
		}
	}

	// Empty now means runtime-selected default. Clear a condition left by an
	// older controller that treated the same state as an error.
	if meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionModelUnresolved) != nil {
		before := agent.DeepCopy()
		if r.setModelUnresolvedCondition(agent, false) {
			if patchErr := r.Status().Patch(ctx, agent, client.MergeFrom(before)); patchErr != nil {
				log.FromContext(ctx).Info("ModelUnresolved condition clear failed (best-effort)",
					"agent", agent.Name, "err", patchErr)
			}
		}
	}

	return resolved, nil
}

// reconcileRuntimeImageCondition refuses pod construction when the agent's
// runtime adapter resolves to an empty image, and keeps
// AgentConditionRuntimeImageMissing in sync either way.
//
// This is a CONFIGURATION fault, not a transient one: it stays broken until an
// operator pins the image in the install's Helm values, so the value of this
// function is the operator-visible signal, not the retry. Returning the error
// still leaves the agent un-pod'd (same contract as an unresolvable model),
// but now with a condition that names the exact value to set. See kyber#674.
func (r *AgentReconciler) reconcileRuntimeImageCondition(
	ctx context.Context,
	agent *kyberv1.Agent,
	adapter pkgruntimes.Adapter,
) error {
	if adapter.Image() != "" {
		// Configured — clear a previously-set condition if present.
		if meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionRuntimeImageMissing) != nil {
			before := agent.DeepCopy()
			if r.setRuntimeImageMissingCondition(agent, "", false) {
				if patchErr := r.Status().Patch(ctx, agent, client.MergeFrom(before)); patchErr != nil {
					log.FromContext(ctx).Info("RuntimeImageMissing condition clear failed (best-effort)",
						"agent", agent.Name, "err", patchErr)
				}
			}
		}
		return nil
	}

	before := agent.DeepCopy()
	if r.setRuntimeImageMissingCondition(agent, agent.Spec.Runtime, true) {
		if patchErr := r.Status().Patch(ctx, agent, client.MergeFrom(before)); patchErr != nil {
			log.FromContext(ctx).Info("RuntimeImageMissing condition patch failed (best-effort)",
				"agent", agent.Name, "err", patchErr)
		}
	}
	return fmt.Errorf(
		"agent %s/%s: runtime %q has no image configured on this cluster — set image.%s.tag in the Helm values",
		agent.Namespace, agent.Name, agent.Spec.Runtime, pkgruntimes.HelmImageKey(agent.Spec.Runtime))
}

// reconcileTelegramCondition keeps AgentConditionTelegramUnavailable in sync
// with whether this install can actually give the agent a Telegram sidecar
// (kyber#684).
//
// It runs on the RECONCILE path, not inside createPod, and that placement is
// the whole point. The runtime-image condition can live in createPod because
// that fault refuses pod construction, so the agent keeps re-entering createPod
// until it clears. This one deliberately does NOT block the pod — Telegram is
// one channel, not the agent's reason to exist — so a healthy agent never
// re-enters createPod, and a condition set there would be written once and then
// never cleared when the operator finally pins the image.
//
// Best-effort: a failed status patch must not fail the reconcile. The agent is
// running and doing everything except Telegram; losing the annotation about it
// is strictly worse than losing the agent.
func (r *AgentReconciler) reconcileTelegramCondition(ctx context.Context, agent *kyberv1.Agent, wiring TelegramWiring) {
	reason, message := "", ""
	switch {
	case !agent.Spec.Secrets.TelegramEnabled:
		// Nothing to say about a channel this agent never asked for.
	case r.TelegramSidecarImage == "":
		reason = "NoTelegramSidecarImage"
		message = "Telegram is enabled for this agent but this cluster has no kyber-mcp-telegram image " +
			"configured, so the agent can neither receive nor send Telegram messages. Pin it by setting " +
			"image.telegramSidecar.tag in the install's Helm values, then this clears automatically. " +
			"Nothing needs to change on the agent itself."
	case !wiring.HasAllowlist:
		// The sidecar refuses to start without an allowlist rather than answer
		// strangers, so this surfaces as a crash-looping container with the
		// reason buried in its logs. Say it here instead (kyber#684).
		reason = "NoTelegramAllowlist"
		message = "Telegram is enabled for this agent but nothing says who is allowed to message it, so the " +
			"Telegram sidecar will not start. This agent was configured under the retired in-process plugin, " +
			"which kept its allowlist on the agent's own disk where the control plane cannot read it. Set " +
			"allowedUserIds for this agent through the /comms API, or set telegram.defaultAllowedUserIds in " +
			"the install's Helm values to cover every migrated agent at once."
	}

	if reason == "" && meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionTelegramUnavailable) == nil {
		return
	}
	before := agent.DeepCopy()
	if !r.setTelegramUnavailableCondition(agent, reason, message) {
		return
	}
	if err := r.Status().Patch(ctx, agent, client.MergeFrom(before)); err != nil {
		log.FromContext(ctx).Info("TelegramUnavailable condition patch failed (best-effort)",
			"agent", agent.Name, "err", err)
	}
}

// setTelegramUnavailableCondition sets or clears
// AgentConditionTelegramUnavailable (kyber#684). Same shape as the
// runtime-image condition — remediation named verbatim, row removed rather than
// left as a stale "False" — but NOT fatal to pod construction: an agent with no
// Telegram should still run and do everything else.
func (r *AgentReconciler) setTelegramUnavailableCondition(agent *kyberv1.Agent, reason, message string) bool {
	if reason == "" {
		return meta.RemoveStatusCondition(&agent.Status.Conditions, kyberv1.AgentConditionTelegramUnavailable)
	}
	return meta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type:    kyberv1.AgentConditionTelegramUnavailable,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})
}

// setRuntimeImageMissingCondition sets or clears
// AgentConditionRuntimeImageMissing. Mirrors setModelUnresolvedCondition: the
// True case carries a verbatim remediation message that the PWA renders, and
// the False case removes the row rather than leaving a stale "False".
func (r *AgentReconciler) setRuntimeImageMissingCondition(agent *kyberv1.Agent, runtime string, missing bool) bool {
	if !missing {
		return meta.RemoveStatusCondition(&agent.Status.Conditions, kyberv1.AgentConditionRuntimeImageMissing)
	}
	key := pkgruntimes.HelmImageKey(runtime)
	cond := metav1.Condition{
		Type:   kyberv1.AgentConditionRuntimeImageMissing,
		Status: metav1.ConditionTrue,
		Reason: "NoRuntimeImageConfigured",
		Message: fmt.Sprintf(
			"This cluster has no container image configured for runtime %q, so the agent cannot be started. "+
				"Pin it by setting image.%s.tag in the install's Helm values, then this clears automatically. "+
				"Nothing needs to change on the agent itself.",
			runtime, key),
	}
	return meta.SetStatusCondition(&agent.Status.Conditions, cond)
}

// setModelUnresolvedCondition sets or clears AgentConditionModelUnresolved
// on the agent's status. Returns true when the condition changed (i.e.,
// the caller should patch status). The True case carries a remediation
// message — the PWA renders this verbatim, so operators have a one-glance
// fix path. The False case clears via RemoveStatusCondition so we don't
// leave stale "False" rows cluttering the conditions list once a model
// is in place.
func (r *AgentReconciler) setModelUnresolvedCondition(agent *kyberv1.Agent, unresolved bool) bool {
	if !unresolved {
		return meta.RemoveStatusCondition(&agent.Status.Conditions, kyberv1.AgentConditionModelUnresolved)
	}
	cond := metav1.Condition{
		Type:    kyberv1.AgentConditionModelUnresolved,
		Status:  metav1.ConditionTrue,
		Reason:  "NoModelConfigured",
		Message: "Agent has no resolvable model: spec.model is empty and the fleet-default defaultModel is also empty. Set spec.model on this agent OR set the fleet default via the PWA Settings panel.",
	}
	return meta.SetStatusCondition(&agent.Status.Conditions, cond)
}

// resolveAdapter returns the runtimes.Adapter for the agent's runtime type.
// Tests inject AdapterRegistry directly; production main.go leaves it nil and
// the lookup falls through to the global pkg/runtimes registry populated by
// blank-imported runtime packages (kyber#250).
func (r *AgentReconciler) resolveAdapter(agent *kyberv1.Agent) (pkgruntimes.Adapter, error) {
	if r.AdapterRegistry != nil {
		adapter, ok := r.AdapterRegistry[agent.Spec.Runtime]
		if !ok {
			return nil, fmt.Errorf("unknown runtime type: %q", agent.Spec.Runtime)
		}
		return adapter, nil
	}
	rt, ok := pkgruntimes.Get(agent.Spec.Runtime)
	if !ok {
		return nil, fmt.Errorf("unknown runtime type: %q", agent.Spec.Runtime)
	}
	return rt.Adapter(), nil
}

// currentRecoveryInput returns an opaque identity for the operator-supplied
// input that a recovery out of the agent's current human-required phase would
// consume: the credential Secret's resourceVersion for NeedsAuth, the memory
// limit for MemoryExhausted, or disk request for DiskExhausted (kyber#684).
//
// A missing Secret yields a stable sentinel rather than an error — an agent
// whose credential Secret has not been created yet must sit in NeedsAuth, not
// spin, and must still recover the moment the Secret appears (its
// resourceVersion will differ from the sentinel).
func (r *AgentReconciler) currentRecoveryInput(ctx context.Context, agent *kyberv1.Agent) (string, error) {
	switch agent.Status.Phase {
	case kyberv1.AgentPhaseMemoryExhausted:
		return "mem=" + agent.Spec.Resources.Memory.String(), nil
	case kyberv1.AgentPhaseDiskExhausted:
		return "disk=" + agent.Spec.Resources.Disk.String(), nil

	case kyberv1.AgentPhaseNeedsAuth:
		adapter, err := r.resolveAdapter(agent)
		if err != nil {
			// Unknown runtime: no way to name the credential. Return a constant
			// so the gate holds shut rather than oscillating.
			return "unresolved-adapter", nil //nolint:nilerr // deliberate: hold, don't churn
		}
		name := adapter.CredentialSecretName(agent)
		if name == "" {
			// Runtime has no credential Secret (or an auth type we don't map).
			// Nothing to detect a change against — hold.
			return "no-credential-secret", nil
		}
		var secret corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: name}, &secret); err != nil {
			if errors.IsNotFound(err) {
				return "absent:" + name, nil
			}
			return "", fmt.Errorf("reading credential secret %q: %w", name, err)
		}
		return "rv:" + name + ":" + secret.ResourceVersion, nil
	}
	return "", nil
}

// recoveryInputChanged reports whether the operator has supplied something new
// since the last recovery attempt out of a human-required phase. It records the
// current input as a side effect when it returns true, so a single change
// yields exactly one attempt (kyber#684).
func (r *AgentReconciler) recoveryInputChanged(ctx context.Context, agent *kyberv1.Agent) (bool, error) {
	current, err := r.currentRecoveryInput(ctx, agent)
	if err != nil {
		return false, err
	}
	if current == "" || current == agent.Status.RecoveryInput {
		return false, nil
	}
	// Claim this input BEFORE the caller acts on it. If the pod creation that
	// follows fails, the agent lands back here with the input already recorded
	// and holds — which is the safe direction: a human is already required, and
	// a genuinely new credential will differ again.
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.RecoveryInput = current
	if err := r.Status().Patch(ctx, agent, patch); err != nil {
		return false, fmt.Errorf("recording recovery input: %w", err)
	}
	return true, nil
}

// resolveNodeName looks up the k8s node name for the agent's spec.machine.
// It reads the Machine CRD's status.nodeName field, which is set by the Machine
// Controller once the VM boots and the k3s agent joins the cluster.
//
// Falls back to spec.machine directly if the Machine CRD is not found or has no
// nodeName yet — this preserves backward compatibility with tests and k3d environments
// where machines are pre-provisioned nodes identified by name.
func (r *AgentReconciler) resolveNodeName(ctx context.Context, agent *kyberv1.Agent) (string, error) {
	machine := &kyberv1.Machine{}
	key := types.NamespacedName{
		Name:      agent.Spec.Machine,
		Namespace: agent.Namespace,
	}
	if err := r.Get(ctx, key, machine); err != nil {
		if errors.IsNotFound(err) {
			// Machine CRD not found — fall back to spec.machine (pre-provisioned node).
			return agent.Spec.Machine, nil
		}
		return "", fmt.Errorf("fetching machine %s: %w", agent.Spec.Machine, err)
	}

	if machine.Status.NodeName != "" {
		return machine.Status.NodeName, nil
	}

	// Machine CRD exists but nodeName not yet set (still provisioning).
	// Fall back to spec.machine so tests using pre-named nodes still work.
	return agent.Spec.Machine, nil
}

// isPodReady returns true if the pod is Running and all containers report Ready.
func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// shouldThrottleRestart returns the remaining backoff duration and true if the reconciler
// should defer the auto-restart because not enough time has elapsed since the last failure
// transition. It uses RetryBackoffDuration(status.RestartCount) as the required wait.
// Returns (0, false) when the backoff window has passed and the restart can proceed.
func (r *AgentReconciler) shouldThrottleRestart(agent *kyberv1.Agent) (time.Duration, bool) {
	if agent.Status.LastTransition == nil {
		return 0, false
	}
	required := RetryBackoffDuration(agent.Status.RestartCount)
	elapsed := time.Since(agent.Status.LastTransition.Time)
	if elapsed >= required {
		return 0, false
	}
	return required - elapsed, true
}

// writeBrief builds and stores a session brief for the agent before pod creation.
// Brief write failures are logged but do not block pod creation — the init container
// falls back to an empty brief ({}) if the endpoint returns an error.
func (r *AgentReconciler) writeBrief(ctx context.Context, agent *kyberv1.Agent, event Event) {
	if r.BriefStore == nil {
		return
	}
	logger := log.FromContext(ctx)
	input := briefInputForEvent(agent, event)

	brief := BuildBrief(agent, input)
	if err := r.BriefStore.Put(ctx, agent.Name, brief); err != nil {
		logger.Error(err, "failed to write session brief — pod will start without context",
			"agent", agent.Name)
	} else {
		logger.Info("session brief written",
			"agent", agent.Name,
			"shutdown_type", brief.ShutdownType,
			"restart_reason", brief.RestartReason)
	}
}

// fireAlert sends an alert to the configured AlertSink. Errors are logged but
// never returned — alert delivery must not disrupt the reconcile loop.
func (r *AgentReconciler) fireAlert(ctx context.Context, agent *kyberv1.Agent, severity, reason string, details map[string]string) {
	if r.AlertSink == nil {
		return
	}
	logger := log.FromContext(ctx)
	if err := r.AlertSink.Fire(ctx, telemetry.Alert{
		Severity: severity,
		Kind:     "agent",
		Name:     agent.Name,
		Reason:   reason,
		Details:  details,
	}); err != nil {
		logger.Error(err, "alert sink error (non-fatal)", "agent", agent.Name, "reason", reason)
	}
}

// isMachinePreempted returns true if the agent's machine is in a preemption-related phase.
// Direct preemption phases are: Preempted, Replacing, Provisioning.
// Additionally, a spot machine in Failed state is treated as preempted — this handles the
// case where a node dies but the machine controller was already in Failed (e.g., from a
// previous preemption cycle) and never transitioned to Preempted again. Without this,
// agents on failed spot machines would burn retry counts instead of waiting for a new node.
// Finally, if the agent's node is NotReady and the machine is Spot, the pod death is treated
// as preemption even if the machine controller hasn't caught up yet — this closes the ~5-minute
// gap between k8s detecting the node gone and the machine transitioning to Preempted.
// Returns false if MachineGetter is nil, the agent has no machine, or the machine cannot be fetched.
func (r *AgentReconciler) isMachinePreempted(ctx context.Context, agent *kyberv1.Agent, pod *corev1.Pod) bool {
	if r.MachineGetter == nil || agent.Spec.Machine == "" {
		return false
	}
	machine, err := r.MachineGetter.Get(ctx, agent.Spec.Machine, agent.Namespace)
	if err != nil || machine == nil {
		return false
	}
	// Direct preemption states from machine controller.
	switch machine.Status.Phase {
	case kyberv1.MachinePhasePreempted, kyberv1.MachinePhaseReplacing, kyberv1.MachinePhaseProvisioning:
		return true
	case kyberv1.MachinePhaseFailed:
		// A spot machine that is Failed likely lost its node to preemption — treat it as
		// preempted so the agent waits for a replacement rather than counting it as a crash.
		return machine.Spec.Spot
	}
	// Node-status early detection: machine controller may lag behind k8s node status by
	// several minutes on spot preemption. If the agent's node is NotReady and the machine
	// is Spot, treat pod death as preemption rather than a crash.
	//
	// Prefer pod.Spec.NodeName (always populated once pod is scheduled) over
	// agent.Status.NodeName (may not be set by the controller).
	var nodeName string
	if pod != nil && pod.Spec.NodeName != "" {
		nodeName = pod.Spec.NodeName
	} else {
		nodeName = agent.Status.NodeName
	}
	if machine.Spec.Spot && nodeName != "" {
		if r.isNodeNotReady(ctx, nodeName) {
			return true
		}
	}
	return false
}

// isMachineUnavailable reports whether an assigned Machine currently lacks
// schedulable capacity. The Agent lifecycle deliberately consumes only this
// provider-neutral readiness contract; cloud interruption details remain in
// the Machine controller and compute adapter.
func (r *AgentReconciler) isMachineUnavailable(ctx context.Context, agent *kyberv1.Agent) bool {
	if r.MachineGetter == nil || agent.Spec.Machine == "" {
		return false
	}
	machine, err := r.MachineGetter.Get(ctx, agent.Spec.Machine, agent.Namespace)
	if err != nil || machine == nil {
		return false
	}
	return machine.Status.Phase != kyberv1.MachinePhaseReady &&
		machine.Status.Phase != kyberv1.MachinePhaseRunning
}

// isNodeNotReady returns true if the named node has a Ready condition that is not True.
// Returns false if the node cannot be fetched or has no Ready condition.
func (r *AgentReconciler) isNodeNotReady(ctx context.Context, nodeName string) bool {
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return false
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status != corev1.ConditionTrue
		}
	}
	return false
}

// isNodeUnavailable distinguishes vanished/dead capacity from a planned
// Machine transition whose Node is still healthy. Only the former justifies
// bypassing pod termination grace while parking an Agent.
func (r *AgentReconciler) isNodeUnavailable(ctx context.Context, nodeName string) bool {
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return errors.IsNotFound(err)
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status != corev1.ConditionTrue
		}
	}
	return true
}

// isMachineReady returns true if the agent's machine is in the Ready or Running phase,
// indicating that it can accept a new agent pod. Returns false if MachineGetter is nil,
// the agent has no machine, or the machine cannot be fetched.
func (r *AgentReconciler) isMachineReady(ctx context.Context, agent *kyberv1.Agent) bool {
	if r.MachineGetter == nil || agent.Spec.Machine == "" {
		return false
	}
	machine, err := r.MachineGetter.Get(ctx, agent.Spec.Machine, agent.Namespace)
	if err != nil || machine == nil {
		return false
	}
	return machine.Status.Phase == kyberv1.MachinePhaseReady || machine.Status.Phase == kyberv1.MachinePhaseRunning
}

// writePreemptionBrief builds and stores a session brief for a preemption-triggered drain.
// Brief write failures are logged but do not block the drain — the init container falls back
// to an empty brief ({}) if the endpoint returns an error.
func (r *AgentReconciler) writePreemptionBrief(ctx context.Context, agent *kyberv1.Agent) {
	if r.BriefStore == nil {
		return
	}
	logger := log.FromContext(ctx)

	var pctx *briefstore.PreemptionContext
	if r.MachineGetter != nil && agent.Spec.Machine != "" {
		machine, err := r.MachineGetter.Get(ctx, agent.Spec.Machine, agent.Namespace)
		if err == nil && machine != nil {
			pctx = &briefstore.PreemptionContext{
				InstanceId:    machine.Status.InstanceId,
				Zone:          machine.Spec.Zone,
				Timestamp:     time.Now().UTC().Format(time.RFC3339),
				GracefulDrain: true,
			}
		}
	}

	brief := &briefstore.Brief{
		Version:       1,
		AgentName:     agent.Name,
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		ShutdownType:  briefstore.ShutdownTypePreemption,
		RestartReason: briefstore.RestartReasonPreemption,
		Metadata: briefstore.BriefMetadata{
			PreviousModel:     agent.Status.CurrentModel,
			RestartCount:      agent.Status.RestartCount,
			PreemptionContext: pctx,
		},
	}
	if err := r.BriefStore.Put(ctx, agent.Name, brief); err != nil {
		logger.Error(err, "failed to write preemption brief", "agent", agent.Name)
	}
}

// containsString returns true if the slice contains the given string.
func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// removeString returns a new slice with the given string removed.
func removeString(slice []string, s string) []string {
	result := make([]string, 0, len(slice))
	for _, v := range slice {
		if v != s {
			result = append(result, v)
		}
	}
	return result
}

// Credential-failure exit codes. Each runtime's start script exits with its own
// code when the harness cannot authenticate, so the reconciler can tell
// "bad/expired credentials" apart from a generic crash and route the agent to
// NeedsAuth (which lights up the PWA's Re-authorize button) rather than to a
// pointless auto-restart loop on the same broken credential.
const (
	// claudeCodeAuthFailureExitCode is start-claude.sh's signal that the
	// Anthropic OAuth refresh failed.
	claudeCodeAuthFailureExitCode int32 = 2
	// codexAuthFailureExitCode is start-codex.sh's signal that `codex login
	// status` failed — the ChatGPT auth.json is missing or no longer valid.
	codexAuthFailureExitCode int32 = 42
)

// isOAuthRefreshFailure checks whether the agent container's CURRENT termination
// carried one of the runtimes' credential-failure exit codes. Only inspects
// State.Terminated (not LastTerminationState) — a previous auth failure
// followed by a non-auth crash must not be misclassified as NeedsAuth.
//
// The code is matched regardless of spec.runtime: the two values don't collide,
// and reading the code the container actually exited with is more robust than
// trusting the spec to describe the binary that ran.
func isOAuthRefreshFailure(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "agent" {
			continue
		}
		if cs.State.Terminated == nil {
			continue
		}
		switch cs.State.Terminated.ExitCode {
		case claudeCodeAuthFailureExitCode, codexAuthFailureExitCode:
			return true
		}
	}
	return false
}

// isAgentContainerTerminated returns true when the agent container in the pod
// is currently in a terminated state, regardless of pod-level Phase. Post-#248
// the pod includes a kyber-status-sidecar container that stays running even
// after the agent process exits, which keeps pod.Status.Phase=Running and
// hides the death from the broader pod-level checks. Used in the Starting and
// Running phase branches to detect agent death without depending on
// pod-level Phase (#274).
func isAgentContainerTerminated(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "agent" && cs.State.Terminated != nil {
			return true
		}
	}
	return false
}

// isOOMKilled returns true when the agent container's CURRENT termination
// was tagged by kubelet as OOMKilled. Only inspects State.Terminated — a
// previous OOM kill followed by a non-OOM crash must not be misclassified.
//
// Caveat (kyber#272 V1): kubelet sets Reason=OOMKilled only when the
// kernel's OOM-killer victim is the container's PID 1. If the killer picks
// a child (common when PID 1 is dbus-run-session shepherding claude-code),
// PID 1 exits with a generic "Error" reason and this predicate returns
// false. The kyber#285 follow-up closes that gap via the sidecar's
// cgroup-counter signal — see hasRecentKernelOOMKill.
//
// kyber#584: generalized to take a container name so the same predicate serves
// the agent-container caller (the only one today) and any future per-container
// OOM check. The sidecar OOM/flap signal (Phase C) uses its own
// containerStatusOOMKilled helper instead, because a flapping native sidecar's
// OOM is in LastTerminationState (already restarted), which this CURRENT-only
// predicate intentionally ignores.
func isOOMKilled(pod *corev1.Pod, containerName string) bool {
	if pod == nil {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != containerName {
			continue
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled" {
			return true
		}
	}
	return false
}

// oomDetectionWindow is how long classifyEvent waits for the sidecar's
// memory_oom signal after observing an exit-137 agent termination
// (kyber#285). The sidecar polls the recursive cgroup memory.events
// file every heartbeatInterval (5s) so giving it ~30s covers a few ticks plus
// HTTP roundtrip + status patch latency. After the window, treat the
// death as a generic non-OOM crash — better to auto-restart than to
// hold the agent in limbo indefinitely.
const oomDetectionWindow = 30 * time.Second

// hasRecentKernelOOMKill returns true when the sidecar reported a kernel
// OOM kill (via the pod-level cgroup memory.events oom_kill counter
// increment) AFTER the agent container's current life began. The
// "after" check attributes the kill to THIS terminated container, not
// a previous one in the same pod. kyber#285 is the issue this
// implements.
func hasRecentKernelOOMKill(agent *kyberv1.Agent, pod *corev1.Pod) bool {
	if agent == nil || agent.Status.LastKernelOOMKillAt == nil {
		return false
	}
	if pod == nil {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "agent" || cs.State.Terminated == nil {
			continue
		}
		// Container started, then died; LastKernelOOMKillAt must fall
		// between StartedAt and FinishedAt to attribute the kill.
		if cs.State.Terminated.StartedAt.IsZero() {
			// No StartedAt — can't safely attribute. Defensive false.
			return false
		}
		return agent.Status.LastKernelOOMKillAt.After(cs.State.Terminated.StartedAt.Time)
	}
	return false
}

// isAgentExitCode137 returns true when the agent container's CURRENT
// termination has exit code 137 (SIGKILL). Used as a "this might be a
// kubelet-untagged OOM" hint that triggers the oomDetectionWindow wait
// in classifyEvent (kyber#285). Other exit codes don't get the wait —
// they're either documented (e.g. exit 2 → OAuth refresh) or genuine
// non-OOM crashes that should auto-restart immediately.
func isAgentExitCode137(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "agent" || cs.State.Terminated == nil {
			continue
		}
		return cs.State.Terminated.ExitCode == 137
	}
	return false
}

// agentTerminatedWithin returns true when the agent container's CURRENT
// termination FinishedAt is within window of now. Pairs with
// isAgentExitCode137 to bound the oomDetectionWindow wait —
// classifyEvent stops waiting after window elapses and routes the death
// as a generic PodDied (kyber#285).
func agentTerminatedWithin(pod *corev1.Pod, window time.Duration) bool {
	if pod == nil {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "agent" || cs.State.Terminated == nil {
			continue
		}
		if cs.State.Terminated.FinishedAt.IsZero() {
			// No FinishedAt — be generous and say "within window" so we
			// at least try to wait for the sidecar.
			return true
		}
		return time.Since(cs.State.Terminated.FinishedAt.Time) < window
	}
	return false
}

// isSidecarDrifted reports whether the pod's status-sidecar spec image
// differs from the controller's current StatusSidecarImage. Originally
// (kyber#299) this compared kubelet-resolved sha256 digests; the digest
// extractor false-positives on multi-arch images whose index-manifest
// digest (env-side) never equals the per-platform manifest digest
// (kubelet-side). See kyber#371 Defect B for the GHCR repro. Switched
// to spec-string equality (Matt's Option b): both ends derive the same
// string from the same controller env, so the comparison is symmetric
// without a registry call.
//
// Thin wrapper around isSidecarSpecMismatched preserved so the existing
// kyber#299 callers (reconcileSidecarDriftCondition,
// maybeAutoRollSidecarForDrift) keep their original naming.
func isSidecarDrifted(pod *corev1.Pod, controllerImage string) bool {
	return isSidecarSpecMismatched(pod, controllerImage)
}

// Auto-roll defaults (kyber#299 Option B). Matched against
// SidecarAutoRollMinStable; see AgentReconciler doc-comments for the
// chart wiring.
const (
	sidecarAutoRollDefaultMinStable     = 5 * time.Minute
	sidecarAutoRollDefaultMaxConcurrent = 1

	// sidecarImageCanaryDefaultWindow is how long the observed-evidence
	// canary (kyber#371) waits for the first delete's replacement pod to
	// reach Ready on a freshly-pinned StatusSidecarImage. Long enough to
	// absorb a cold-cache image pull on a slow registry; short enough
	// that a permanently bad pin fails fast instead of dripping deletes
	// indefinitely. 3 minutes is the same order of magnitude as the
	// cluster's pod-pull-timeout defaults.
	sidecarImageCanaryDefaultWindow = 3 * time.Minute

	// runtimeImageRollDefaultMaxConcurrent caps concurrent runtime-image
	// rolls (kyber#529). Deliberately equal to
	// sidecarAutoRollDefaultMaxConcurrent AND enforced via the SAME
	// path-agnostic countAgentPodsBeingDeleted counter, so the runtime
	// roll shares ONE cluster-wide delete budget with the 5c sidecar
	// auto-roll and the 5d sidecar convergence: at most this many agent-pod
	// deletes in flight across all three causes. =1 preserves the
	// #523/#527 single-agent behavior (one agent → one roll → converge),
	// and contains a bad fleet-wide digest to the canary.
	runtimeImageRollDefaultMaxConcurrent = sidecarAutoRollDefaultMaxConcurrent

	// runtimeImageCanaryDefaultWindow bounds how long the first runtime-
	// image roll (the canary) has to bring its replacement pod Ready on the
	// new image before the rest of the fleet's rolls are frozen (kyber#529).
	// Same rationale and magnitude as sidecarImageCanaryDefaultWindow.
	runtimeImageCanaryDefaultWindow = sidecarImageCanaryDefaultWindow
)

// reconcileSidecarDriftCondition mutates agent.Status.Conditions in
// place to reflect the current SidecarOutOfDate state. Returns true
// when the condition state actually changed, so callers can skip a
// no-op patch round-trip. Must be called before the caller's status
// patch DeepCopy so the diff captures the change.
//
// Sets the condition only when phase is Running AND a pod exists —
// other phases the condition isn't meaningful (controller is mid-roll,
// pod doesn't exist yet, etc.). When transitioning out of Running, the
// condition is cleared via meta.RemoveStatusCondition so the surface
// stays clean.
func (r *AgentReconciler) reconcileSidecarDriftCondition(agent *kyberv1.Agent, pod *corev1.Pod) bool {
	if agent.Status.Phase != kyberv1.AgentPhaseRunning || pod == nil {
		return meta.RemoveStatusCondition(&agent.Status.Conditions, kyberv1.AgentConditionSidecarOutOfDate)
	}
	drifted := isSidecarDrifted(pod, r.StatusSidecarImage)
	cond := metav1.Condition{
		Type:   kyberv1.AgentConditionSidecarOutOfDate,
		Status: metav1.ConditionFalse,
		Reason: "Current",
	}
	if drifted {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "PodPredatesSidecarUpdate"
		cond.Message = fmt.Sprintf(
			"Pod's status-sidecar spec image (%s) does not match controller's current sidecar image (%s); restart the pod to apply.",
			extractSidecarSpecImage(pod),
			r.StatusSidecarImage,
		)
	} else {
		cond.Message = "Pod's status-sidecar spec image matches the controller's current sidecar image."
	}
	return meta.SetStatusCondition(&agent.Status.Conditions, cond)
}

// reconcileRuntimeStatusConditions translates the runtime-report fields
// in Status.Runtime into the two PR-E conditions
// (AgentConditionRuntimeVersionMismatch, AgentConditionModelUnsupported).
// Returns true when either condition was changed (caller patches status).
//
// Decision table:
//
//	RuntimeVersionMismatch:
//	  installedVersion=="" OR requestedVersion=="" → Remove (no signal)
//	  requestedSatisfied==true                     → False (Reason=Match)
//	  installed == requested                       → False (Reason=Match)
//	  otherwise                                    → True  (Reason=InstallNotConverged)
//
//	requestedSatisfied is consulted first because a floating request like
//	`latest` never string-matches the concrete installed version, yet the
//	boot-time install did converge. A nil value falls through to the
//	string comparison, which is the pre-`latest` behavior.
//
//	ModelUnsupported:
//	  modelSupported nil, probe message present → Unknown (Reason=ProbeInconclusive)
//	  modelSupported nil, no probe message      → Remove (probe didn't run / old sidecar)
//	  modelSupported true                       → False (Reason=ProbeOK)
//	  modelSupported false                      → True  (Reason=ProbeFailed, message includes probe output)
//
// Both conditions stay absent (rather than False) when the underlying
// signal is unknown — keeps the conditions list clean for agents that
// haven't reported yet, and matches the pattern other "absent ≠ false"
// surfaces (e.g., RequestedSatisfied is a *bool because nil-vs-false
// matters for the staggered roll). See kyber#379.
func (r *AgentReconciler) reconcileRuntimeStatusConditions(agent *kyberv1.Agent) bool {
	rs := &agent.Status.Runtime
	changed := false

	// RuntimeVersionMismatch
	switch {
	case rs.InstalledVersion == "" || rs.RequestedVersion == "":
		if meta.RemoveStatusCondition(&agent.Status.Conditions, kyberv1.AgentConditionRuntimeVersionMismatch) {
			changed = true
		}
	case rs.RequestedSatisfied != nil && *rs.RequestedSatisfied || rs.InstalledVersion == rs.RequestedVersion:
		cond := metav1.Condition{
			Type:    kyberv1.AgentConditionRuntimeVersionMismatch,
			Status:  metav1.ConditionFalse,
			Reason:  "Match",
			Message: fmt.Sprintf("Installed harness version (%s) satisfies the requested version (%s).", rs.InstalledVersion, rs.RequestedVersion),
		}
		if meta.SetStatusCondition(&agent.Status.Conditions, cond) {
			changed = true
		}
	default:
		cond := metav1.Condition{
			Type:   kyberv1.AgentConditionRuntimeVersionMismatch,
			Status: metav1.ConditionTrue,
			Reason: "InstallNotConverged",
			Message: fmt.Sprintf(
				"Installed harness version (%s) differs from the requested version (%s) — likely a failed boot-time install; restart the agent after fixing the cause (e.g., bad version string, registry outage).",
				rs.InstalledVersion, rs.RequestedVersion,
			),
		}
		if meta.SetStatusCondition(&agent.Status.Conditions, cond) {
			changed = true
		}
	}

	// ModelUnsupported
	switch {
	case rs.ModelSupported == nil && rs.ModelProbeMessage != "":
		// The probe ran and failed, but not in a way attributable to the
		// model (auth, network, unrecognized error shape). Surface it as
		// Unknown with the diagnostic instead of removing the condition —
		// "we could not verify the model" must be distinguishable from
		// "the probe never ran" (canary regression 2026-08-22: an invalid
		// fleet-default model rendered as all-green because inconclusive
		// collapsed to silence).
		cond := metav1.Condition{
			Type:    kyberv1.AgentConditionModelUnsupported,
			Status:  metav1.ConditionUnknown,
			Reason:  "ProbeInconclusive",
			Message: "Pre-flight model probe failed without a clear model-rejection signal: " + rs.ModelProbeMessage,
		}
		if meta.SetStatusCondition(&agent.Status.Conditions, cond) {
			changed = true
		}
	case rs.ModelSupported == nil:
		if meta.RemoveStatusCondition(&agent.Status.Conditions, kyberv1.AgentConditionModelUnsupported) {
			changed = true
		}
	case *rs.ModelSupported:
		cond := metav1.Condition{
			Type:    kyberv1.AgentConditionModelUnsupported,
			Status:  metav1.ConditionFalse,
			Reason:  "ProbeOK",
			Message: "Pre-flight model probe succeeded — the installed Claude Code accepts the configured model.",
		}
		if meta.SetStatusCondition(&agent.Status.Conditions, cond) {
			changed = true
		}
	default:
		msg := "Pre-flight model probe failed — the installed Claude Code rejected the configured model. Fix the model (per-agent set-model, or the fleet default), or apply a newer Claude Code version (spec.runtimeVersion per-agent, or defaultRuntimeVersion fleet-wide) that supports it."
		if rs.ModelProbeMessage != "" {
			msg += " Probe output: " + rs.ModelProbeMessage
		}
		cond := metav1.Condition{
			Type:    kyberv1.AgentConditionModelUnsupported,
			Status:  metav1.ConditionTrue,
			Reason:  "ProbeFailed",
			Message: msg,
		}
		if meta.SetStatusCondition(&agent.Status.Conditions, cond) {
			changed = true
		}
	}

	return changed
}

// maybeAutoRollSidecarForDrift implements kyber#299 Option B: when an
// agent's SidecarOutOfDate condition has been True past a stability
// window AND the agent reports idle AND no other agent's pod is
// already mid-roll, request the normal Restarting transition so it recreates
// the pod on the new sidecar digest. Returns (rolled, err); rolled=true means
// the caller should requeue and skip the rest of the reconcile.
//
// All gates are intentionally restrictive — auto-roll is a
// best-effort convenience for visibility-aware operators, not a
// guaranteed fleet refresh. The Option A condition + PWA dirty flag
// remain the source of truth. False positives just mean an operator
// click goes unanswered for a few minutes longer; false negatives
// here are far worse (interrupting a working agent mid-turn).
func (r *AgentReconciler) maybeAutoRollSidecarForDrift(ctx context.Context, agent *kyberv1.Agent, pod *corev1.Pod) (bool, error) {
	if !r.SidecarAutoRollEnabled || pod == nil || pod.DeletionTimestamp != nil {
		return false, nil
	}
	if agent.Status.Phase != kyberv1.AgentPhaseRunning {
		return false, nil
	}
	cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionSidecarOutOfDate)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return false, nil
	}
	minStable := r.SidecarAutoRollMinStable
	if minStable <= 0 {
		minStable = sidecarAutoRollDefaultMinStable
	}
	if cond.LastTransitionTime.IsZero() || time.Since(cond.LastTransitionTime.Time) < minStable {
		return false, nil
	}
	// Idle gate (kyber#249): never interrupt an agent the runtime
	// reports as working. Empty/unknown state also blocks — we'd rather
	// hold an out-of-date pod than roll one we can't characterize.
	if agent.Status.Activity == nil || agent.Status.Activity.State != tokenreport.ActivityIdle {
		return false, nil
	}
	// Concurrency gate: at most one auto-roll cluster-wide so a fleet of
	// drifted agents drains gradually instead of all rebooting at once.
	// We approximate "in-flight roll" with "any agent pod currently
	// being deleted" — overcounts user-driven restarts/stops, which is
	// the safe direction (defer rather than pile on).
	inflight, err := r.countAgentPodsBeingDeleted(ctx, agent.Namespace)
	if err != nil {
		return false, fmt.Errorf("counting in-flight pod deletions: %w", err)
	}
	if inflight >= sidecarAutoRollDefaultMaxConcurrent {
		return false, nil
	}
	requested, err := r.requestIntentionalRestart(ctx, agent)
	if err != nil {
		return false, fmt.Errorf("requesting restart for sidecar auto-roll: %w", err)
	}
	if !requested {
		return false, nil
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(agent, corev1.EventTypeNormal, "SidecarOutOfDateAutoRoll",
			"requested restart of pod %s to refresh status-sidecar (was %s, expected %s); kyber#299",
			pod.Name,
			extractSidecarSpecImage(pod),
			r.StatusSidecarImage,
		)
	}
	return true, nil
}

// requestIntentionalRestart persists rollout intent before any pod deletion.
// The next reconcile derives EventDesiredRestarting and uses the normal
// Running -> Restarting state-machine path. This ordering is load-bearing: a
// direct delete can race another reconcile, which then mistakes Kyber's own
// rollout for PodDied and consumes crash backoff.
func (r *AgentReconciler) requestIntentionalRestart(ctx context.Context, agent *kyberv1.Agent) (bool, error) {
	current := &kyberv1.Agent{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(agent), current); err != nil {
		return false, fmt.Errorf("fetching current agent: %w", err)
	}
	// Restarting is consumed only from Running. Persisting it from a parked or
	// failed phase would leave a sticky request that also reserves the shared
	// rollout budget forever.
	if current.Status.Phase != kyberv1.AgentPhaseRunning {
		return false, nil
	}
	// Never overwrite newer operator intent. Running is the steady desired
	// value for active agents; empty is also valid for legacy/internal paths.
	if current.Spec.DesiredPhase != "" && current.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		return false, nil
	}
	before := current.DeepCopy()
	current.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
	if err := r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("persisting restart intent: %w", err)
	}
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
	return true, nil
}

// countAgentPodsBeingDeleted returns the number of distinct agent rollouts in
// flight. A rollout counts as soon as DesiredPhase=Restarting is persisted,
// before its pod receives a DeletionTimestamp; this closes the reservation gap
// introduced by routing convergence through the state machine.
func (r *AgentReconciler) countAgentPodsBeingDeleted(ctx context.Context, namespace string) (int, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(namespace),
		client.HasLabels{"kyber.io/agent"},
	); err != nil {
		return 0, err
	}
	inflight := make(map[string]struct{})
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp != nil {
			inflight[pods.Items[i].Labels["kyber.io/agent"]] = struct{}{}
		}
	}
	var agents kyberv1.AgentList
	if err := r.List(ctx, &agents, client.InNamespace(namespace)); err != nil {
		return 0, err
	}
	for i := range agents.Items {
		if agents.Items[i].Spec.DesiredPhase == kyberv1.AgentPhaseRestarting {
			inflight[agents.Items[i].Name] = struct{}{}
		}
	}
	return len(inflight), nil
}

// extractContainerSpecImage returns the named container's spec image (what
// pod_builder.go requested at pod-create time), or "" when no container of that
// name is in either pod.Spec.InitContainers or pod.Spec.Containers.
//
// kyber#575: the status-sidecar is a native sidecar in Spec.InitContainers
// (RestartPolicy:Always). Scan the Init list FIRST, then fall back to the regular
// Containers so a pod built by a pre-#575 controller (sidecar still a regular
// container) is still resolved during the rollover — without the fallback,
// kyber#299 tag-pinned helm convergence would silently stop on every
// not-yet-recreated pod. Scanning both lists also lets one helper serve sidecars
// that are regular containers (Discord, Telegram) and native ones alike, so a
// later promotion to a native sidecar needs no change here (kyber#688).
func extractContainerSpecImage(pod *corev1.Pod, name string) string {
	if pod == nil {
		return ""
	}
	for _, c := range pod.Spec.InitContainers {
		if c.Name == name {
			return c.Image
		}
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return c.Image
		}
	}
	return ""
}

// extractSidecarSpecImage returns the kyber-status-sidecar container's spec
// image. Thin wrapper over extractContainerSpecImage kept so the kyber#299 /
// kyber#358 call sites and tests keep their original naming.
func extractSidecarSpecImage(pod *corev1.Pod) string {
	return extractContainerSpecImage(pod, StatusSidecarContainerName)
}

// isContainerImageMismatched reports whether the pod's container `name` is
// running something other than desiredImage. Tag-level string comparison (no
// digest normalization) — both ends derive the same string from the same
// controller env, so the comparison is symmetric without a registry call.
//
// absentIsDrift decides what an ABSENT container means, and it is the whole
// reason this is parameterized (kyber#688). For the status sidecar, absence is
// "no signal": every pod has one, so an empty read means mid-rebuild or a legacy
// pod, and deleting on it would be guessing. For an opt-in channel sidecar,
// absence is the drift — a pod built before the channel was enabled, or before
// the sidecar existed at all, is exactly the pod that needs rolling. That case
// is what left a migrated agent on the retired in-process plugin for 68 minutes
// with a green pod and a satisfied-looking Agent CR.
//
// Returns false (no drift signal) when pod is nil or desiredImage is empty. The
// empty-image guard is load-bearing on every path that consumes this: never
// delete a pod over an unset env var.
func isContainerImageMismatched(pod *corev1.Pod, name, desiredImage string, absentIsDrift bool) bool {
	if pod == nil || desiredImage == "" {
		return false
	}
	specImage := extractContainerSpecImage(pod, name)
	if specImage == "" {
		return absentIsDrift
	}
	return specImage != desiredImage
}

// isSidecarSpecMismatched returns true when the pod's status-sidecar container
// spec image differs from the controller's current StatusSidecarImage. Catches
// the chart-bump case where the control-plane pod rolled with a new env but
// existing agent pods still carry the old sidecar image in their spec.
//
// Absence is deliberately NOT drift here (absentIsDrift=false): the status
// sidecar is unconditional, so a pod without one is mid-rebuild or pre-dates the
// sidecar, and neither is a reason to delete it.
//
// Complements isSidecarDrifted (digest-pin comparison, kyber#299). That layer
// runs the careful Option B auto-roll (idle gate, stability window, concurrency
// cap) for operators on digest pins. This layer makes `helm upgrade` actually
// converge the fleet for tag-pinned installs — the common case.
func isSidecarSpecMismatched(pod *corev1.Pod, controllerImage string) bool {
	return isContainerImageMismatched(pod, StatusSidecarContainerName, controllerImage, false)
}

// extractAgentSpecImage returns the agent container's spec image (what
// pod_builder.go requested at pod-create time via adapter.Image()), or "" when
// the container is not in pod.Spec.Containers. Direct mirror of
// extractSidecarSpecImage, keyed on AgentContainerName.
func extractAgentSpecImage(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == AgentContainerName {
			return c.Image
		}
	}
	return ""
}

// isAgentRuntimeImageDrifted returns true when the pod's agent container spec
// image differs from the controller's currently-desired runtime image
// (adapter.Image()). Direct mirror of isSidecarSpecMismatched for the runtime
// image (#523).
//
// Full-ref (repo:tag@digest) string comparison: production pins repo:latest@sha256:…
// (digest written by an image-sync workflow), so a digest change is a string
// change and is detected, while a bare tag-to-tag compare would miss it.
// Spec-to-spec (the pod's requested image, not status/ImageID) avoids kubelet
// digest-normalization noise that could re-fire the roll. Because the recreated
// pod's agent container is set from the same adapter.Image(), one roll converges
// — the next reconcile compares equal and derives no event.
//
// Returns false (no drift signal) when:
//   - pod is nil
//   - the agent container is not in pod.Spec.Containers
//   - desiredImage is empty — load-bearing (kyber#360 Cause D: never roll a live
//     agent onto an empty ref; fail rather than fall back)
func isAgentRuntimeImageDrifted(pod *corev1.Pod, desiredImage string) bool {
	if pod == nil || desiredImage == "" {
		return false
	}
	specImage := extractAgentSpecImage(pod)
	if specImage == "" {
		return false
	}
	return specImage != desiredImage
}

// isSidecarReady reports whether the kyber-status-sidecar container in
// pod is fully running per kubelet — Ready=true and State.Running set.
// Used as the kyber#371 observed-evidence canary's positive signal:
// kubelet only flips Ready after pulling the image and passing readiness
// checks, so an image observed Ready on any pod is proven pullable for
// the rest of this controller process's lifetime.
//
// kyber#575: a native sidecar's runtime status reports under
// Status.InitContainerStatuses, not Status.ContainerStatuses. Scan the Init list
// FIRST, then fall back to the regular list so a pre-#575 pod (sidecar still a
// regular container) keeps feeding the canary its positive signal during the
// rollover.
func isSidecarReady(pod *corev1.Pod) bool {
	return isContainerReady(pod, StatusSidecarContainerName)
}

// isContainerReady reports whether the named container in pod is fully running
// per kubelet — Ready=true and State.Running set. Generalized out of
// isSidecarReady (kyber#688) so every image-canary path asks the same question
// of its own container. Scans InitContainerStatuses first for the same reason
// extractContainerSpecImage scans Spec.InitContainers first.
func isContainerReady(pod *corev1.Pod, name string) bool {
	if pod == nil {
		return false
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.Name == name {
			return cs.Ready && cs.State.Running != nil
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == name {
			return cs.Ready && cs.State.Running != nil
		}
	}
	return false
}

// canaryWindow returns the active canary window — caller-configured if
// SidecarImageCanaryWindow > 0, the package default otherwise.
func (r *AgentReconciler) canaryWindow() time.Duration {
	if r.SidecarImageCanaryWindow > 0 {
		return r.SidecarImageCanaryWindow
	}
	return sidecarImageCanaryDefaultWindow
}

// runtimeCanaryWindow returns the active runtime-image canary window —
// caller-configured if RuntimeImageCanaryWindow > 0, the package default
// otherwise (kyber#529). Mirror of canaryWindow for the runtime roll.
func (r *AgentReconciler) runtimeCanaryWindow() time.Duration {
	if r.RuntimeImageCanaryWindow > 0 {
		return r.RuntimeImageCanaryWindow
	}
	return runtimeImageCanaryDefaultWindow
}

// isAgentReady reports whether the agent container in pod is fully
// running per kubelet — Ready=true and State.Running set. The runtime-
// image canary's positive signal (kyber#529), the direct mirror of
// isSidecarReady keyed on AgentContainerName: kubelet only flips Ready
// after pulling the agent image and passing the readiness probe, so an
// image observed Ready on any pod is proven pullable for the rest of
// this controller process's lifetime.
func isAgentReady(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != AgentContainerName {
			continue
		}
		return cs.Ready && cs.State.Running != nil
	}
	return false
}

// recordRuntimeImageRollHeld emits a Normal-type Kubernetes Event
// documenting why a runtime-image roll was deferred. Single Reason
// string (mirroring recordSidecarImageRollHeld); the distinction between
// "canary mid-window" and "canary failed" lives in the Message so
// operators can grep one Reason and triage from the message.
func (r *AgentReconciler) recordRuntimeImageRollHeld(agent *kyberv1.Agent, podName, image, detail string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(agent, corev1.EventTypeNormal, "RuntimeImageRollHeld",
		"runtime-image roll held for pod %s on image %s: %s; kyber#529",
		podName, image, detail,
	)
}

// shouldRollRuntimeImage gates the runtime-image-drift → EventDesiredRestarting
// derivation (kyber#529). It sits IN FRONT of the classifyEvent drift check
// (which until #527 was a bare isAgentRuntimeImageDrifted call) so a fleet-wide
// KYBER_AGENT_RUNTIME_IMAGE bump rolls Running agents in bounded, canary-gated
// waves instead of all at once. A bad/unpullable digest is then contained to
// the canary instead of fleet-widing every agent into ImagePullBackOff.
//
// Returns (true, nil) only when the pod is genuinely drifted AND the shared
// concurrency budget + observed-evidence canary allow THIS agent to be the next
// to roll. The gate sequence mirrors convergeSidecarImage exactly, with ONE
// deliberate omission: there is NO idle gate. AC#4 requires single-agent envs
// (a single-agent install) to keep the immediate roll-and-converge behavior #523/#527
// shipped and verified; an idle gate would change that. The runtime roll also
// goes through the state machine (capture-state-and-delete), as do the sidecar
// convergence paths. All arm the canary at the rollout decision point; the
// canary measures image pullability over wall-clock, not the delete instant.
//
// Concurrency is the SHARED cluster-wide budget: countAgentPodsBeingDeleted is
// path-agnostic, so the runtime roll, the 5c sidecar auto-roll, and the 5d
// sidecar convergence cooperatively cap at runtimeImageRollDefaultMaxConcurrent
// (=1) deletes in flight across all causes.
//
// Fail-safe: a countAgentPodsBeingDeleted error returns (false, err) — the
// agent simply isn't rolled this pass (hold, don't pile on), the same posture
// as convergeSidecarImage. Drift DETECTION itself (isAgentRuntimeImageDrifted)
// is unchanged from #523/#527 — only the rollout pacing is gated.
func (r *AgentReconciler) shouldRollRuntimeImage(ctx context.Context, agent *kyberv1.Agent, pod *corev1.Pod, desiredImage string) (bool, error) {
	if !isAgentRuntimeImageDrifted(pod, desiredImage) {
		return false, nil
	}
	// NO idle gate here (AC#4) — see doc-comment.
	//
	// Concurrency gate: shared cluster-wide budget across 5c/5d/runtime via
	// the path-agnostic countAgentPodsBeingDeleted. Drains the fleet in
	// bounded waves rather than rolling every drifted agent at once.
	inflight, err := r.countAgentPodsBeingDeleted(ctx, agent.Namespace)
	if err != nil {
		return false, fmt.Errorf("counting in-flight pod deletions: %w", err)
	}
	if inflight >= runtimeImageRollDefaultMaxConcurrent {
		return false, nil
	}
	// Pullability gate: observed-evidence canary (kyber#371 machinery,
	// kyber#529). Same FSM sequence as convergeSidecarImage's pullability gate.
	switch {
	case r.runtimeCanary.failedCanary(desiredImage):
		r.recordRuntimeImageRollHeld(agent, pod.Name, desiredImage,
			"canary window elapsed without a Ready pod on the new runtime image; operator must verify the image pin")
		return false, nil
	case r.runtimeCanary.wasVerified(desiredImage):
		// Image proven pullable — steady-state wave; allow.
		return true, nil
	default:
		started, inFlight := r.runtimeCanary.canaryInFlight(desiredImage)
		switch {
		case inFlight && time.Since(started) > r.runtimeCanaryWindow():
			r.runtimeCanary.markCanaryFailed(desiredImage)
			r.recordRuntimeImageRollHeld(agent, pod.Name, desiredImage,
				"canary window elapsed without a Ready pod on the new runtime image; further rolls held until the image is fixed")
			return false, nil
		case inFlight:
			// Canary still mid-window — hold the rest of the fleet silently.
			// The first eligible agent's replacement pod is the one we watch.
			return false, nil
		default:
			// No canary attempt yet for this image — THIS agent is the canary.
			// Arm the clock and allow the roll.
			r.runtimeCanary.markCanaryStarted(desiredImage)
			return true, nil
		}
	}
}

// The six methods below are the status-sidecar roll's binding to the
// shared observed-evidence canary FSM. Since kyber#529 the FSM lives in
// the image-agnostic imageCanaryTracker (image_canary.go); these thin
// wrappers delegate to r.sidecarCanary so convergeSidecarImage's call
// sites and the existing kyber#371 sidecar tests keep their original
// surface and behavior unchanged (AC#7). The runtime-image roll uses
// r.runtimeCanary directly.

// markSidecarImageVerified records that some pod has been observed Ready
// on the sidecar image. See imageCanaryTracker.markVerified.
func (r *AgentReconciler) markSidecarImageVerified(image string) {
	r.sidecarCanary.markVerified(image)
}

// sidecarImageWasVerified reports whether any pod has been observed
// Ready on image during this controller process's lifetime.
func (r *AgentReconciler) sidecarImageWasVerified(image string) bool {
	return r.sidecarCanary.wasVerified(image)
}

// markSidecarCanaryStarted records the first delete attempt against image.
func (r *AgentReconciler) markSidecarCanaryStarted(image string) {
	r.sidecarCanary.markCanaryStarted(image)
}

// sidecarCanaryInFlight returns the canary start time and whether one
// is recorded for image.
func (r *AgentReconciler) sidecarCanaryInFlight(image string) (time.Time, bool) {
	return r.sidecarCanary.canaryInFlight(image)
}

// markSidecarCanaryFailed marks image as canary-failed.
func (r *AgentReconciler) markSidecarCanaryFailed(image string) {
	r.sidecarCanary.markCanaryFailed(image)
}

// sidecarImageFailedCanary reports whether image's canary window
// elapsed without a Ready observation.
func (r *AgentReconciler) sidecarImageFailedCanary(image string) bool {
	return r.sidecarCanary.failedCanary(image)
}

// recordSidecarImageRollHeld emits a Normal-type Kubernetes Event
// documenting why a sidecar convergence delete was deferred. Single
// Reason string (per Obi-wan's design § Open questions builder pick
// #2); the distinction between "canary mid-window" and "canary failed"
// lives in the Message text so operators can grep one Reason and triage
// from the message.
func (r *AgentReconciler) recordSidecarImageRollHeld(agent *kyberv1.Agent, podName, image, detail string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(agent, corev1.EventTypeNormal, "SidecarImageRollHeld",
		"sidecar convergence held for pod %s on image %s: %s; kyber#371",
		podName, image, detail,
	)
}

// convergeSidecarImage closes the gap between the controller's current
// StatusSidecarImage env and what each agent pod's spec requests. When
// the pod's status-sidecar spec image differs from the controller's
// current image, the pod is deleted; the next reconcile rebuilds it
// via createPod with the current env (kyber#358).
//
// Hardened by kyber#371 against the R2-D2 foot-gun (a bad
// StatusSidecarImage env deleted every Working agent in production)
// via three serial gates that mirror maybeAutoRollSidecarForDrift's
// safeguards:
//
//  1. Idle gate — Working agents are never interrupted by a sidecar
//     version skew; defer until the agent reports idle. Walks back
//     kyber#358's original "always converge" trade-off because R2-D2
//     proved an invalid image makes that trade-off catastrophic.
//  2. Concurrency cap — reuses countAgentPodsBeingDeleted and
//     sidecarAutoRollDefaultMaxConcurrent (=1) so 5c (kyber#299) and
//     5d (this) cooperatively cap one delete cluster-wide; a fleet of
//     drifted agents drains gradually instead of all rebooting at once.
//  3. Observed-evidence pullability gate — kubelet is the pullability
//     oracle. If any pod on the target image has been observed Ready
//     in this controller process, the image is verified and
//     convergence proceeds at steady state. If not, the first eligible
//     delete is bookkept as the canary; subsequent reconciles defer
//     while the canary window is open. If the window elapses without
//     a Ready observation, the image is marked failed; further rolls
//     are held and SidecarImageRollHeld events are emitted until the
//     operator hot-fixes the env (producing a new image string) or
//     restarts the controller (re-arming the canary). No registry
//     client, no image-pull-secret threading, no external network on
//     the reconcile hot path.
//
// Canary FSM per (controller process, image string):
//
//	unknown ──first eligible delete──▶ CANARY IN FLIGHT
//	  │                                 │            │
//	  │  any pod observed Ready         │ window     │
//	  │  on image (verification         │ elapses    │
//	  │  trigger at top of Reconcile)   │ without    │
//	  ▼                                 ▼ Ready      ▼
//	VERIFIED  ◀─ Ready pod observed ─┘            FAILED
//	(steady-state convergence)                 (rolls held)
//
// Returns (requested, err). When requested=true, caller requeues and skips the
// rest of reconcile; the next pass drives the normal Restarting transition.
func (r *AgentReconciler) convergeSidecarImage(ctx context.Context, agent *kyberv1.Agent, pod *corev1.Pod) (bool, error) {
	if pod == nil || pod.DeletionTimestamp != nil {
		return false, nil
	}
	if !isSidecarSpecMismatched(pod, r.StatusSidecarImage) {
		return false, nil
	}
	// Idle gate (kyber#371 Defect A): never interrupt an agent the
	// runtime reports as working. Empty/unknown state also blocks —
	// same conservative posture as maybeAutoRollSidecarForDrift; we'd
	// rather hold a skewed pod than roll one we can't characterize.
	if agent.Status.Activity == nil || agent.Status.Activity.State != tokenreport.ActivityIdle {
		return false, nil
	}
	// Concurrency gate (kyber#371 Defect A): at most one agent pod
	// deleting cluster-wide across 5c and 5d combined. Drains the
	// fleet gradually rather than rebooting it all at once.
	inflight, err := r.countAgentPodsBeingDeleted(ctx, agent.Namespace)
	if err != nil {
		return false, fmt.Errorf("counting in-flight pod deletions: %w", err)
	}
	if inflight >= sidecarAutoRollDefaultMaxConcurrent {
		return false, nil
	}
	// Pullability gate (kyber#371 Defect A): observed-evidence canary. Decide
	// whether this request is the canary here, but arm it only after restart
	// intent is successfully persisted. A conflicting operator intent must not
	// create a phantom canary for a rollout that never happens.
	target := r.StatusSidecarImage
	armCanary := false
	switch {
	case r.sidecarImageFailedCanary(target):
		r.recordSidecarImageRollHeld(agent, pod.Name, target,
			"canary window elapsed without a Ready pod; operator must verify the sidecar image pin")
		return false, nil
	case r.sidecarImageWasVerified(target):
		// Image is proven pullable — steady-state convergence.
	default:
		started, inFlight := r.sidecarCanaryInFlight(target)
		switch {
		case inFlight && time.Since(started) > r.canaryWindow():
			r.markSidecarCanaryFailed(target)
			r.recordSidecarImageRollHeld(agent, pod.Name, target,
				"canary window elapsed without a Ready pod; further rolls held until env is fixed")
			return false, nil
		case inFlight:
			// Canary still mid-window — wait silently. The first eligible
			// pod's replacement is the one we're watching.
			return false, nil
		default:
			// No canary attempt yet for this image — THIS pod is the canary.
			armCanary = true
		}
	}
	specImage := extractSidecarSpecImage(pod)
	requested, err := r.requestIntentionalRestart(ctx, agent)
	if err != nil {
		return false, fmt.Errorf("requesting restart for sidecar image convergence: %w", err)
	}
	if !requested {
		return false, nil
	}
	if armCanary {
		r.markSidecarCanaryStarted(target)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(agent, corev1.EventTypeNormal, "SidecarImageConverge",
			"requested restart of pod %s for sidecar image convergence (had %s, expected %s); kyber#358+kyber#371",
			pod.Name, specImage, target,
		)
	}
	return true, nil
}

// podDerivedStatusDiffers reports whether agent.Status's pod-derived fields
// (PodName, PodIP, NodeName, StartTime) differ from what pod currently has.
// Used as the diff gate before issuing a status patch (kyber#355) so
// steady-state reconciles produce zero patches.
//
// pod must be non-nil; callers gate on that already. StartTime comparison
// uses metav1.Time.Equal which handles the *metav1.Time pointer pair
// correctly. An empty pod.Status.PodIP (pod hasn't received an IP yet)
// won't trigger a diff against an already-empty agent.Status.PodIP — we
// only mirror the value, not write zero-values unconditionally.
func podDerivedStatusDiffers(agent *kyberv1.Agent, pod *corev1.Pod) bool {
	if agent.Status.PodName != pod.Name {
		return true
	}
	if agent.Status.PodIP != pod.Status.PodIP {
		return true
	}
	if agent.Status.NodeName != pod.Spec.NodeName {
		return true
	}
	switch {
	case pod.Status.StartTime == nil && agent.Status.StartTime == nil:
		// Both unset — no diff.
	case pod.Status.StartTime == nil || agent.Status.StartTime == nil:
		// One set, one unset — diff. (Pod loses StartTime only on a clean
		// reset; surfacing that to the CRD is the right behavior.)
		return true
	case !agent.Status.StartTime.Equal(pod.Status.StartTime):
		return true
	}
	return false
}

// applyPodDerivedStatus mutates agent.Status in place to match pod's
// observed fields. Caller is responsible for the surrounding
// DeepCopy+Patch dance. Counterpart to podDerivedStatusDiffers.
//
// StartTime is overwritten from pod.Status.StartTime when the pod has one.
// updatePhase sets StartTime to time.Now() on first Running entry; the
// next reconcile through this path replaces that with the pod's actual
// StartTime, which is the value the godoc at agent_types.go:497 promises.
// Retry-reset logic at Reconcile() step 4 reads StartTime as
// "Running-for-N-min" gate; pod.Status.StartTime is the more accurate
// reference for that check, so the swap is a small improvement, not a
// regression.
func applyPodDerivedStatus(agent *kyberv1.Agent, pod *corev1.Pod) {
	agent.Status.PodName = pod.Name
	agent.Status.PodIP = pod.Status.PodIP
	agent.Status.NodeName = pod.Spec.NodeName
	if pod.Status.StartTime != nil {
		agent.Status.StartTime = pod.Status.StartTime
	}
}
