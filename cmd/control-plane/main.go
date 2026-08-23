// Package main is the entrypoint for the Kyber control plane.
//
// The control plane is a modular monolith that manages agent and machine
// lifecycle via Kubernetes CRDs. It runs as a single pod in the k8s cluster
// and uses controller-runtime for CRD reconciliation.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apitypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/matty-v/kyber/pkg/adapters"
	internalapi "github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/contextwindowmap"
	agentcontroller "github.com/matty-v/kyber/pkg/controllers/agent"
	machinecontroller "github.com/matty-v/kyber/pkg/controllers/machine"
	"github.com/matty-v/kyber/pkg/fleetdefaults"
	"github.com/matty-v/kyber/pkg/gceemulator"
	"github.com/matty-v/kyber/pkg/githubapp"
	"github.com/matty-v/kyber/pkg/inbound"
	"github.com/matty-v/kyber/pkg/logging"
	"github.com/matty-v/kyber/pkg/metrics"
	"github.com/matty-v/kyber/pkg/metricsstore"
	"github.com/matty-v/kyber/pkg/podtoken"
	"github.com/matty-v/kyber/pkg/runtimedetect"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
	"github.com/matty-v/kyber/pkg/runtimes/claudecode"
	_ "github.com/matty-v/kyber/pkg/runtimes/codex"
	"github.com/matty-v/kyber/pkg/statechangestore"
	"github.com/matty-v/kyber/pkg/telemetry"

	// pkg/runtimes/claudecode now needs both blank-import side effect
	// (init() registers the adapter against the global registry) AND a
	// named reference (so main.go can type-assert each entry of
	// adapterRegistry and assign the context-window resolver — kyber#378
	// PR-D). The named import at line 47 satisfies both: Go elides the
	// duplicate package load and runs init() exactly once.
	"github.com/matty-v/kyber/pkg/configexport"
	"github.com/matty-v/kyber/pkg/tokenstore"
	"github.com/matty-v/kyber/pkg/updates"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kyberv1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":9090", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.Parse()

	processLog, err := logging.New(logging.Config{
		Component: "control-plane",
		Level:     os.Getenv("KYBER_LOG_LEVEL"),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	slog.SetDefault(processLog)
	ctrl.SetLogger(logr.FromSlogHandler(processLog.Handler()))

	ctx := ctrl.SetupSignalHandler()

	// Fail fast if the Helm-injected self-reference URL is missing.
	// Without it the agent pod builder cannot produce correct briefURL or
	// KYBER_REFRESH_TOKEN_URL values, so launching would create broken pods silently.
	if os.Getenv("KYBER_CONTROL_PLANE_INTERNAL_URL") == "" {
		setupLog.Error(nil, "KYBER_CONTROL_PLANE_INTERNAL_URL is required (set by Helm chart on the control-plane Deployment)")
		os.Exit(1)
	}

	// --- Telemetry (OTEL) ---
	// Enabled by default; set KYBER_TELEMETRY_ENABLED=false to disable.
	telemetryCfg := telemetry.Config{
		Enabled:        os.Getenv("KYBER_TELEMETRY_ENABLED") != "false",
		Endpoint:       os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		ServiceName:    "kyber-control-plane",
		ServiceVersion: "v0.1.0",
	}
	tel, err := telemetry.Init(ctx, telemetryCfg)
	if err != nil {
		setupLog.Error(err, "initializing telemetry")
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tel.Shutdown(shutdownCtx)
	}()

	if err := telemetry.InitMetrics(); err != nil {
		setupLog.Error(err, "registering metrics")
		os.Exit(1)
	}

	// Alert sink — always log (LogAlertSink floor); optionally also POST to a webhook.
	// Fail-LOUD, not fail-closed (#586): if no webhook is configured, the floor keeps
	// running but we warn unmistakably at startup that alerts are NOT delivered, so the
	// log-only state can't hide (mirrors the #566 startup-loudness precedent).
	alertSink, alertConfigured := telemetry.BuildAlertSink(os.Getenv("KYBER_ALERT_WEBHOOK_URL"))
	if !alertConfigured {
		setupLog.Info("WARN: KYBER_ALERT_WEBHOOK_URL is unset — platform alerts (incl. Phase-C SidecarOOMRestart) are LOG-ONLY and are NOT delivered to any phone-actionable receiver. Set KYBER_ALERT_WEBHOOK_URL to enable delivery.")
	}

	restCfg := ctrl.GetConfigOrDie()

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		setupLog.Error(err, "unable to create clientset")
		os.Exit(1)
	}

	// Scope the controller's informer cache to this deployment's namespace so
	// preview control-planes don't reconcile prod Agents (and vice-versa).
	// Without this, controller-runtime watches every namespace via a single
	// cluster-scoped informer — the prod and preview control-planes both end
	// up reconciling the same Agent CRs and racing on status writes. Observed
	// 2026-04-19 while standing up PR previews: a preview without the GitHub
	// App client kept stamping identityRepo.phase=Failed on prod agents.
	//
	// KYBER_NAMESPACE is read below (same env the HTTP handlers use); duplicate
	// the default here so a dev install without Helm still works.
	watchNamespace := os.Getenv("KYBER_NAMESPACE")
	if watchNamespace == "" {
		watchNamespace = "kyber-system"
	}
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "kyber-control-plane",
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				watchNamespace: {},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// BriefStore: use PostgreSQL if KYBER_POSTGRES_URL is set (Helm sub-chart
	// or external Postgres), otherwise fall back to in-memory (dev / tests).
	// The PostgresStore persists briefs across pod restarts; MemoryStore does not.
	//
	// When KYBER_POSTGRES_URL IS set, we never silently fall back to in-memory
	// on connection failure — that caused briefs to vanish on the next restart
	// without any alerting (issue #60). Instead, retry the migration on a
	// bounded loop (~2min) and os.Exit on persistent failure so the pod
	// crashes and Kubernetes re-schedules it.
	var briefStore briefstore.BriefStore
	if pgURL := os.Getenv("KYBER_POSTGRES_URL"); pgURL != "" {
		db, err := sql.Open("postgres", pgURL)
		if err != nil {
			setupLog.Error(err, "opening postgres connection for BriefStore — exiting so Kubernetes restarts the pod")
			os.Exit(1)
		}
		db.SetMaxOpenConns(5)
		db.SetMaxIdleConns(2)
		db.SetConnMaxLifetime(30 * time.Minute)
		pgStore := briefstore.NewPostgresStore(db)

		// Retry the migration for up to ~2min (25 attempts × 5s) so ArgoCD
		// rolling syncs that bring up postgres + control-plane together don't
		// lose briefs due to the initial connection race.
		migrateCtx, cancel := context.WithTimeout(ctx, 2*time.Minute+30*time.Second)
		err = briefstore.MigrateWithRetry(migrateCtx, pgStore.Migrate,
			2*time.Minute, 5*time.Second, setupLog.Info)
		cancel()
		if err != nil {
			setupLog.Error(err, "BriefStore Postgres migration never succeeded within the retry budget — exiting so Kubernetes restarts the pod")
			_ = db.Close()
			os.Exit(1)
		}
		setupLog.Info("BriefStore: using Postgres", "url", maskDSN(pgURL))
		briefStore = pgStore
	} else {
		setupLog.Info("BriefStore: KYBER_POSTGRES_URL not set — using in-memory store (briefs will not survive pod restart)")
		briefStore = briefstore.NewMemoryStore()
	}

	// TokenStore + MetricsStore + NodeStore + StateChangeAccumulator:
	// all share a Redis client when KYBER_REDIS_URL is set.
	// Fall back to in-memory implementations otherwise.
	var (
		tokenStore       tokenstore.TokenStore
		tokenAccumulator tokenstore.Accumulator
		metricsStore     metricsstore.MetricsStore
		nodeStore        metricsstore.NodeStore
		stateChangeAccum statechangestore.Accumulator
		redisClient      *redis.Client
	)
	if redisURL := os.Getenv("KYBER_REDIS_URL"); redisURL != "" {
		rOpts, err := redis.ParseURL(redisURL)
		if err != nil {
			setupLog.Error(err, "parsing KYBER_REDIS_URL — falling back to in-memory stores")
		} else {
			redisClient = redis.NewClient(rOpts)
			// Ping so we fail fast if Redis is unreachable at startup.
			pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := redisClient.Ping(pingCtx).Err(); err != nil {
				cancel()
				setupLog.Error(err, "pinging Redis — falling back to in-memory stores")
				_ = redisClient.Close()
				redisClient = nil
			} else {
				cancel()
				setupLog.Info("Redis client connected", "addr", rOpts.Addr)
			}
		}
	}
	redisStoreEnabled := os.Getenv("KYBER_METRICS_REDIS_STORE_ENABLED") != "false"
	if redisClient != nil {
		tokenStore = tokenstore.NewRedisStore(redisClient)
		tokenAccumulator = tokenstore.NewRedisAccumulator(redisClient)
		if redisStoreEnabled {
			metricsStore = metricsstore.NewRedisMetricsStore(redisClient)
			nodeStore = metricsstore.NewRedisNodeStore(redisClient)
			stateChangeAccum = statechangestore.NewRedisAccumulator(redisClient)
			setupLog.Info("TokenStore + MetricsStore: using Redis")
		} else {
			setupLog.Info("TokenStore: using Redis; MetricsStore disabled by KYBER_METRICS_REDIS_STORE_ENABLED=false")
			metricsStore = metricsstore.NewMemoryMetricsStore()
			nodeStore = metricsstore.NewMemoryNodeStore()
			stateChangeAccum = statechangestore.NewMemoryAccumulator()
		}
	} else {
		setupLog.Info("WARN: metrics: Redis not configured; using in-memory metrics store. Data evaporates on restart and is not shared across replicas.")
		tokenStore = tokenstore.NewMemoryStore()
		tokenAccumulator = tokenstore.NewMemoryAccumulator()
		metricsStore = metricsstore.NewMemoryMetricsStore()
		nodeStore = metricsstore.NewMemoryNodeStore()
		stateChangeAccum = statechangestore.NewMemoryAccumulator()
	}

	// Retention worker: evict time-series points older than the configured horizon.
	retentionSeconds := int64(7 * 24 * 3600) // default 7d
	if v := os.Getenv("KYBER_METRICS_RETENTION_SECONDS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			retentionSeconds = n
		}
	}
	if evictable, ok := metricsStore.(interface {
		EvictBefore(context.Context, int64) error
	}); ok {
		metricsstore.StartRetentionWorker(ctx, evictable, retentionSeconds, 5*time.Minute)
		setupLog.Info("metrics retention worker started", "retentionSeconds", retentionSeconds)
	}

	// Build the per-runtime Adapter map from the global pkg/runtimes
	// registry. Each blank-imported runtime package self-registered via
	// init() at process start; runtimes.All() returns whatever's in. The
	// reconciler still accepts an explicit AdapterRegistry for tests, but
	// production reads it from the registry.
	//
	// The Claude Code adapter's ContextWindows resolver is assigned later
	// (after kyberNamespace is resolved) — see context-window override
	// map setup below the namespace block.
	adapterRegistry := make(map[string]pkgruntimes.Adapter)
	for _, rt := range pkgruntimes.All() {
		adapterRegistry[rt.Type()] = rt.Adapter()
	}
	if len(adapterRegistry) == 0 {
		setupLog.Error(nil, "no runtimes registered; check blank-imports in cmd/control-plane/main.go")
		os.Exit(1)
	}

	// Read the namespace from the environment — set via the Helm chart's KYBER_NAMESPACE
	// env var (sourced from the control-plane ConfigMap). Falls back to "kyber-system" so
	// the binary still works in local dev without Helm.
	kyberNamespace := os.Getenv("KYBER_NAMESPACE")
	if kyberNamespace == "" {
		kyberNamespace = "kyber-system"
		setupLog.Info("KYBER_NAMESPACE not set — defaulting to kyber-system")
	}

	// GitHub App client (optional): drives identity-repo scaffolding and the
	// GitHub API routes. As of kyber#509 it no longer mints/delivers a per-agent
	// git token — agent git auth rides the generic PAT user-secret. The Secret
	// is operator-managed (see docs/installation.md §5b). If it's missing or
	// malformed, we log and continue without a minter — agents that don't use
	// identity-repo scaffolding keep working, and any that do will surface the
	// misconfiguration in their status.
	var githubAppClient *githubapp.Client
	{
		directClient, err := client.New(restCfg, client.Options{Scheme: scheme})
		if err != nil {
			setupLog.Error(err, "creating direct client for GitHub App Secret load — identity-repo feature disabled")
		} else {
			// Use Background (not ctx) so a fast SIGTERM during boot doesn't
			// silently disable the feature — we'd rather the pod die mid-load
			// and retry on the next schedule.
			loadCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			ghCfg, err := githubapp.LoadConfigFromSecret(loadCtx, directClient, kyberNamespace)
			cancel()
			if err != nil {
				setupLog.Info("GitHub App Secret not loaded — identity-repo feature disabled",
					"namespace", kyberNamespace, "secret", githubapp.SecretName, "err", err.Error())
			} else {
				ghClient, err := githubapp.NewClient(ghCfg)
				if err != nil {
					setupLog.Error(err, "constructing GitHub App client — identity-repo feature disabled")
				} else {
					githubAppClient = ghClient
					setupLog.Info("GitHub App client configured",
						"appID", ghCfg.AppID, "installationID", ghCfg.InstallationID)
				}
			}
		}
	}

	// Register the Agent controller.
	// KYBER_AGENT_STORAGE_CLASS pins the StorageClass used for agent PVCs.
	// Empty string (or unset) means the cluster's default StorageClass binds
	// the volume — the standalone-friendly default on k3s/kind.
	agentStorageClass := os.Getenv("KYBER_AGENT_STORAGE_CLASS")
	// KYBER_TRANSCRIPT_OFFSETS_STORAGE_CLASS / _SIZE configure the small durable
	// per-agent transcript-offsets PVC (kyber#467). The StorageClass defaults to
	// empty (cluster default = local-path) on ALL targets — never kyber-pd, whose
	// 1Gi PD minimum wastes space for sub-1KB checkpoints. Empty size falls back
	// to the in-code default (10Mi).
	transcriptOffsetsStorageClass := os.Getenv("KYBER_TRANSCRIPT_OFFSETS_STORAGE_CLASS")
	transcriptOffsetsSize := os.Getenv("KYBER_TRANSCRIPT_OFFSETS_SIZE")
	// KYBER_IDENTITY_REPO_OWNER is the GitHub user/org under which auto-created
	// identity repos are placed. Required when identityRepo.template is used.
	// Defaults to empty (auto-create disabled for agents that don't set template).
	identityRepoOwner := os.Getenv("KYBER_IDENTITY_REPO_OWNER")
	// Status sidecar image (kyber#248). Read from Helm-provided env var; the
	// reconciler injects this container into every agent pod. Empty string
	// disables the sidecar — useful for tests / dev installs that don't
	// build the image. Production should always have it set.
	statusSidecarImage := os.Getenv("KYBER_STATUS_SIDECAR_IMAGE")
	// kyber-mcp-discord channel sidecar image (kyber#646). Empty disables
	// Discord-sidecar injection; only agents with spec.channels.discord get it.
	discordSidecarImage := os.Getenv("KYBER_DISCORD_SIDECAR_IMAGE")
	telegramSidecarImage := os.Getenv("KYBER_TELEGRAM_SIDECAR_IMAGE")
	// Telegram user IDs this install trusts by default (kyber#684). Used ONLY
	// to seed the allowlist of an agent migrated off the retired in-process
	// plugin, whose allowlist lived on its own PVC where we cannot read it.
	// Never overrides an allowlist that already exists.
	telegramDefaultAllowedUserIDs := os.Getenv("KYBER_TELEGRAM_DEFAULT_ALLOWED_USER_IDS")
	// Sidecar OTLP endpoint (kyber#256, #247 Phase C1). Forwarded to each
	// agent pod's status-sidecar container as KYBER_OTEL_ENDPOINT so it
	// can push per-agent metrics. Empty string disables sidecar metrics.
	sidecarOtelEndpoint := os.Getenv("KYBER_SIDECAR_OTEL_ENDPOINT")
	// Sidecar log level (kyber#360). Propagated to every injected sidecar
	// pod as KYBER_SIDECAR_LOG_LEVEL. Empty (default) → sidecars log at
	// info. Set to "debug" on the CP deployment to enable the forwarder +
	// snapshot diagnostic logs across the fleet without per-pod patches.
	sidecarLogLevel := os.Getenv("KYBER_SIDECAR_LOG_LEVEL")
	discordLogLevel := os.Getenv("KYBER_DISCORD_LOG_LEVEL")
	telegramLogLevel := os.Getenv("KYBER_TELEGRAM_LOG_LEVEL")
	// Sidecar auto-roll (kyber#299 Option B). Off by default — operators
	// opt in by setting KYBER_SIDECAR_AUTO_ROLL=true in the chart's
	// controlPlane.env. The optional KYBER_SIDECAR_AUTO_ROLL_MIN_STABLE
	// (a Go duration, e.g. "5m", "10m") tunes how long the
	// SidecarOutOfDate condition must hold before the controller acts;
	// empty/invalid falls back to the package default.
	sidecarAutoRollEnabled := os.Getenv("KYBER_SIDECAR_AUTO_ROLL") == "true"
	var sidecarAutoRollMinStable time.Duration
	if v := os.Getenv("KYBER_SIDECAR_AUTO_ROLL_MIN_STABLE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			sidecarAutoRollMinStable = d
		} else {
			setupLog.Info("ignoring invalid KYBER_SIDECAR_AUTO_ROLL_MIN_STABLE; using default",
				"value", v, "err", err.Error())
		}
	}
	// Internal-API signing key (kyber#566, Option A). The cluster signing key
	// the control plane uses to mint per-agent and node-agent pod-tokens and to
	// verify them on the internal API (:8082). Delivered by the
	// kyber-internal-signing-key Secret via KYBER_INTERNAL_SIGNING_KEY.
	//
	// Fail-CLOSED when the key is absent (kyber#566 revision, Matt's security
	// call): the internal API (:8082) refuses to serve — every internal route
	// 503s — rather than serving unauthenticated. Serving :8082 wide-open on a
	// missing key would silently re-open the exact agent→agent hole this change
	// closes, so a misconfigured deploy must fail closed, not open (mirrors the
	// #564 fail-closed intent). The gate is scoped to the :8082 handlers via
	// WithInternalAuthFailClosed, so the control plane's other surfaces (public
	// API, health, metrics, controllers) keep running — the process does not
	// crashloop; only the internal API is refused until the key is delivered.
	// Ackbar's delivery verifies the key is present. AC-7's enforce-on-by-default
	// is satisfied by the shipped chart config (key set, graceMode false).
	internalSigningKey := []byte(os.Getenv("KYBER_INTERNAL_SIGNING_KEY"))
	// One-release migration posture (kyber#566 rollout, Matt-locked). When
	// KYBER_INTERNAL_AUTH_GRACE=true the internal API accepts-and-logs
	// unauthenticated calls (covers pods not yet re-rolled onto a mounted
	// token); it never softens a cross-identity denial. Ship false (enforce)
	// in steady state.
	internalAuthGrace := os.Getenv("KYBER_INTERNAL_AUTH_GRACE") == "true"
	// kyber#578 cutover safeguard: decide the startup posture + the one-shot
	// fail-closed alert in one pure place. CONSERVATIVE (Matt's gate, Q1=NO): a
	// missing key fails closed regardless of grace — never serves unauthenticated.
	// The alert is fired below (after the manager exists) so a keyless internal
	// API PAGES instead of degrading silently (the v2.1.0 ~2h incident).
	authBoot := decideInternalAuthBoot(len(internalSigningKey) > 0, internalAuthGrace)
	if authBoot.KeyPresent {
		setupLog.Info("internal API per-agent auth enabled (kyber#566)",
			"graceMode", internalAuthGrace)
	} else {
		setupLog.Error(nil, "KYBER_INTERNAL_SIGNING_KEY is empty — internal API (:8082) "+
			"FAILING CLOSED: every internal route will 503 until the "+
			"kyber-internal-signing-key Secret is delivered. Agent telemetry / "+
			"OAuth rotation / brief fetch on :8082 are refused; the control "+
			"plane's other surfaces are unaffected (kyber#566). An alert has been "+
			"raised (kyber#578).")
	}

	// Transcript-pruner retention config (kyber#471). Propagated to the per-agent
	// pruner sidecar that bounds on-PVC transcript backlog growth. Enabled via the
	// chart's transcripts.retention block; absent env (e.g. helm --reuse-values
	// without the block) leaves the Go zero values, which disable injection.
	transcriptRetentionEnabled := os.Getenv("KYBER_TRANSCRIPT_RETENTION_ENABLED") == "true"
	transcriptRetentionMaxAgeDays := 0
	if v := os.Getenv("KYBER_TRANSCRIPT_RETENTION_MAX_AGE_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			transcriptRetentionMaxAgeDays = n
		} else {
			setupLog.Info("ignoring invalid KYBER_TRANSCRIPT_RETENTION_MAX_AGE_DAYS; pruning disabled",
				"value", v)
		}
	}
	var transcriptRetentionMaxBytesPerAgent int64
	if v := os.Getenv("KYBER_TRANSCRIPT_RETENTION_MAX_BYTES_PER_AGENT"); v != "" {
		// Accept a k8s quantity string (e.g. "200Mi") for operator ergonomics,
		// matching how other size values are written in the chart.
		if q, err := resource.ParseQuantity(v); err == nil && q.Value() >= 0 {
			transcriptRetentionMaxBytesPerAgent = q.Value()
		} else {
			setupLog.Info("ignoring invalid KYBER_TRANSCRIPT_RETENTION_MAX_BYTES_PER_AGENT; using age-only",
				"value", v)
		}
	}
	transcriptPruneIntervalMinutes := 0
	if v := os.Getenv("KYBER_TRANSCRIPT_RETENTION_PRUNE_INTERVAL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			transcriptPruneIntervalMinutes = n
		}
	}
	transcriptRetentionArchiveCrosscheck := os.Getenv("KYBER_TRANSCRIPT_RETENTION_ARCHIVE_CROSSCHECK") == "true"
	// Fleet defaults resolver (kyber#376 / PR-B of #374). Wired only when
	// the chart populates KYBER_FLEET_DEFAULTS_CONFIGMAP — otherwise the
	// reconciler leaves nil and the fallback layer is disabled (agents
	// with empty spec.model land on AgentConditionModelUnresolved).
	fleetDefaultsCM := os.Getenv("KYBER_FLEET_DEFAULTS_CONFIGMAP")
	var fleetDefaultsResolver *fleetdefaults.Resolver
	if fleetDefaultsCM != "" {
		fleetDefaultsResolver = &fleetdefaults.Resolver{
			Client:        mgr.GetClient(),
			Namespace:     kyberNamespace,
			ConfigMapName: fleetDefaultsCM,
		}
		setupLog.Info("fleet defaults resolver enabled",
			"configMap", fleetDefaultsCM, "namespace", kyberNamespace)
	} else {
		setupLog.Info("fleet defaults disabled: KYBER_FLEET_DEFAULTS_CONFIGMAP not set")
	}

	// Context-window override map (kyber#378 / PR-D of #374). Operators
	// edit kyber-model-context-windows to add a new model's window size
	// without a Kyber release. The same Resolver feeds (a) the detection
	// poller's snapshot enrichment so /available carries real windows +
	// ContextWindowKnown=true, and (b) the Claude Code adapter's
	// KYBER_MODEL_CONTEXT_WINDOW env var so start-claude.sh's [1m] decision
	// reads the operator-supplied value. Empty env disables the override
	// path — every model falls back to tokenreport.LimitFor.
	contextWindowsCM := os.Getenv("KYBER_MODEL_CONTEXT_WINDOWS_CONFIGMAP")
	var contextWindowResolver *contextwindowmap.Resolver
	if contextWindowsCM != "" {
		contextWindowResolver = &contextwindowmap.Resolver{
			Client:        mgr.GetClient(),
			Namespace:     kyberNamespace,
			ConfigMapName: contextWindowsCM,
		}
		// Inject into the Claude Code adapter so EnvVars() can consult
		// the override map when sizing KYBER_MODEL_CONTEXT_WINDOW. Other
		// runtimes don't model context windows; the assertion fails
		// silently for them.
		if cc, ok := adapterRegistry["claude-code"].(*claudecode.ClaudeCodeAdapter); ok {
			cc.ContextWindows = contextWindowResolver
		}
		setupLog.Info("context-window override map enabled",
			"configMap", contextWindowsCM, "namespace", kyberNamespace)
	} else {
		setupLog.Info("context-window override map disabled: KYBER_MODEL_CONTEXT_WINDOWS_CONFIGMAP not set")
	}

	agentReconciler := &agentcontroller.AgentReconciler{
		Client:                        mgr.GetClient(),
		Scheme:                        mgr.GetScheme(),
		Recorder:                      mgr.GetEventRecorderFor("agent-controller"),
		AdapterRegistry:               adapterRegistry,
		BriefStore:                    briefStore,
		AlertSink:                     alertSink,
		AgentStorageClass:             agentStorageClass,
		TranscriptOffsetsStorageClass: transcriptOffsetsStorageClass,
		TranscriptOffsetsSize:         transcriptOffsetsSize,
		IdentityRepoOwner:             identityRepoOwner,
		StatusSidecarImage:            statusSidecarImage,
		DiscordSidecarImage:           discordSidecarImage,
		TelegramSidecarImage:          telegramSidecarImage,
		TelegramDefaultAllowedUserIDs: telegramDefaultAllowedUserIDs,
		SidecarOtelEndpoint:           sidecarOtelEndpoint,
		SidecarLogLevel:               sidecarLogLevel,
		DiscordLogLevel:               discordLogLevel,
		TelegramLogLevel:              telegramLogLevel,
		SidecarAutoRollEnabled:        sidecarAutoRollEnabled,
		SidecarAutoRollMinStable:      sidecarAutoRollMinStable,
		FleetDefaults:                 fleetDefaultsResolver,
		MachineGetter:                 &agentcontroller.KubernetesMachineGetter{Client: mgr.GetClient()},

		TranscriptRetentionEnabled:           transcriptRetentionEnabled,
		TranscriptRetentionMaxAgeDays:        transcriptRetentionMaxAgeDays,
		TranscriptRetentionMaxBytesPerAgent:  transcriptRetentionMaxBytesPerAgent,
		TranscriptPruneIntervalMinutes:       transcriptPruneIntervalMinutes,
		TranscriptRetentionArchiveCrosscheck: transcriptRetentionArchiveCrosscheck,

		// kyber#565: the delete finalizer reaps these external stores so a
		// confirmed agent delete leaves zero orphaned identity state. Same
		// handles the InternalServer uses for the metrics read/write path.
		TokenStore:             tokenStore,
		TokenAccumulator:       tokenAccumulator,
		MetricsStore:           metricsStore,
		StateChangeAccumulator: stateChangeAccum,

		// Mint per-agent pod-tokens when the signing key is configured (#566).
		PodTokenKey: internalSigningKey,
	}
	if githubAppClient != nil {
		agentReconciler.GithubTokenMinter = githubAppClient
		agentReconciler.Scaffolder = githubAppClient
		if identityRepoOwner != "" {
			setupLog.Info("identity-repo auto-create enabled",
				"owner", identityRepoOwner)
		} else {
			setupLog.Info("identity-repo auto-create disabled: KYBER_IDENTITY_REPO_OWNER not set")
		}
	}
	if err := agentReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create agent controller")
		os.Exit(1)
	}

	// Start the internal HTTP API on port 8082.
	// It shares the BriefStore with the reconciler so init containers can fetch briefs.
	// WithKubeClient gives the rotation endpoint access to Secrets for OAuth token updates.
	internalOpts := []internalapi.InternalServerOption{
		internalapi.WithKubeClient(mgr.GetClient(), kyberNamespace),
		internalapi.WithTokenStore(tokenStore),
		internalapi.WithTokenAccumulator(tokenAccumulator),
		internalapi.WithMetricsStore(metricsStore),
		internalapi.WithNodeStore(nodeStore),
		internalapi.WithStateChangeAccumulator(stateChangeAccum),
	}
	// kyber#508 Stage 3/4: when a GitHub App is configured, let each agent mint a
	// short-lived token scoped to its OWN identity repo via
	// GET /internal/agents/{name}/identity-repo-token. Absent (no Kyber App wired)
	// → the endpoint 503s and identity-repo git fails loudly (no PAT fallback);
	// identity-repo management is simply disabled on such installs.
	if githubAppClient != nil {
		internalOpts = append(internalOpts, internalapi.WithIdentityRepoTokenMinter(githubAppClient))
	}
	// Per-agent authn/authz on :8082 (kyber#566). With the signing key present,
	// enforce (or grace). Absent → fail CLOSED: every internal route 503s rather
	// than serving unauthenticated, so a missing-key deploy cannot silently
	// re-open the cross-agent hole (Matt's security call; logged above). The
	// :8082 listener still starts so callers get a clean 503, and the gate is
	// scoped to :8082 — the rest of the control plane is unaffected.
	if authBoot.KeyPresent {
		internalOpts = append(internalOpts, internalapi.WithInternalAuth(
			internalapi.NewHMACInternalAuthenticator(internalSigningKey), authBoot.Grace))
	} else {
		// Conservative (kyber#578, Q1=NO): key absent → fail closed regardless of
		// grace. No unauthenticated window is opened; the relaxation waits on the
		// alert-delivery path being proven live.
		internalOpts = append(internalOpts, internalapi.WithInternalAuthFailClosed())
	}
	// kyber#578 L3: fire the one-shot fail-closed startup alert through the
	// kyber#586 alert sink (telemetry.BuildAlertSink above — the LogAlertSink floor
	// plus a redacted, fail-loud WebhookAlertSink when KYBER_ALERT_WEBHOOK_URL is
	// set). Registered as a leader-gated manager Runnable so it fires exactly once
	// per cluster (not per replica) and never blocks startup. It is a startup/state
	// alert, NOT per-request, so it cannot flood (AC3). nil Alert (healthy
	// key-present rollout) registers nothing.
	if authBoot.Alert != nil {
		startupAlert := *authBoot.Alert
		// Integration with kyber#586's alertConfigured: when the webhook is
		// unconfigured this critical page is LOG-ONLY (it won't reach a phone).
		// #586 already warns generally at startup; here we tie the two failures
		// together for the fail-closed case — a fail-closed internal API whose page
		// can't be delivered is the EXACT v2.1.0 compound failure (silent for ~2h).
		// Logged once, alongside the alert, so an operator tailing logs sees both.
		if !alertConfigured {
			setupLog.Info("WARN: internal-auth is FAIL-CLOSED and KYBER_ALERT_WEBHOOK_URL is unset — " +
				"this critical page is LOG-ONLY and will NOT reach a phone-actionable receiver " +
				"(the v2.1.0 silent-fail-closed shape). Deliver the kyber-internal-signing-key Secret " +
				"AND configure KYBER_ALERT_WEBHOOK_URL (kyber#578/#586).")
		}
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			if ferr := alertSink.Fire(ctx, startupAlert); ferr != nil {
				setupLog.Error(ferr, "failed to fire internal-auth fail-closed startup alert (kyber#578)",
					"reason", startupAlert.Reason)
			}
			return nil // never block control-plane startup on the alert
		})); err != nil {
			setupLog.Error(err, "unable to register internal-auth startup alert (kyber#578)")
		}
	}
	internalSrv := internalapi.NewInternalServer(briefStore, internalOpts...)
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		setupLog.Info("starting internal API", "addr", internalapi.DefaultInternalPort)
		return internalSrv.Start(ctx, internalapi.DefaultInternalPort)
	})); err != nil {
		setupLog.Error(err, "unable to register internal API with manager")
		os.Exit(1)
	}

	// Mint the node-agent's pod-token Secret at startup (kyber#566). The
	// node-agent runs as a DaemonSet (not an Agent CR), so it has no reconciler
	// to mint its token; the control plane — the sole holder of the signing key
	// — ensures it once here. The DaemonSet mounts kyber-node-agent-token, and
	// the internal API's machine/node routes admit only this identity. Skipped
	// when no key is configured. Runs as a manager Runnable so the cached client
	// is ready; idempotent (re-running re-signs only on key rotation).
	if len(internalSigningKey) > 0 {
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			if err := ensureNodeAgentToken(ctx, mgr.GetClient(), kyberNamespace, internalSigningKey); err != nil {
				setupLog.Error(err, "failed to ensure node-agent pod-token Secret (kyber#566)")
			} else {
				setupLog.Info("ensured node-agent pod-token Secret", "secret", nodeAgentTokenSecretName)
			}
			return nil // never block control-plane startup on this
		})); err != nil {
			setupLog.Error(err, "unable to register node-agent token ensure")
			os.Exit(1)
		}
	}

	// Compute providers are constructed through the provider registry. An
	// explicitly configured provider fails closed: substituting mock behavior
	// for a broken real provider would make the process look healthy while
	// silently changing Machine lifecycle semantics.
	provider := os.Getenv("KYBER_COMPUTE_PROVIDER")
	if provider == "" {
		provider = "mock"
	}
	var computeSimulation adapters.SimulationController
	gceEndpoint := os.Getenv("KYBER_GCE_ENDPOINT")
	if os.Getenv("KYBER_GCE_EMULATOR") == "true" {
		if provider != "gce" {
			setupLog.Error(nil, "KYBER_GCE_EMULATOR requires compute provider gce", "provider", provider)
			os.Exit(1)
		}
		emulator := gceemulator.New()
		computeSimulation = emulator
		gceEndpoint = "http://127.0.0.1:8083"
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			setupLog.Info("starting local GCE API emulator", "addr", ":8083")
			return emulator.Start(ctx, ":8083")
		})); err != nil {
			setupLog.Error(err, "registering local GCE API emulator")
			os.Exit(1)
		}
	}
	gceProject := os.Getenv("KYBER_GCE_PROJECT")
	gceNetwork := os.Getenv("KYBER_GCE_NETWORK")
	gceSubnet := os.Getenv("KYBER_GCE_SUBNET")
	gkeProject := os.Getenv("KYBER_GKE_PROJECT")
	gkeLocation := os.Getenv("KYBER_GKE_LOCATION")
	gkeCluster := os.Getenv("KYBER_GKE_CLUSTER")
	gkeProfiles := os.Getenv("KYBER_GKE_PROFILES")
	gkeNodeLocations := os.Getenv("KYBER_GKE_NODE_LOCATIONS")
	providerCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	computeAdapter, err := adapters.NewComputeAdapter(providerCtx, provider, adapters.ProviderConfig{
		adapters.GCEConfigProject:       gceProject,
		adapters.GCEConfigNetwork:       gceNetwork,
		adapters.GCEConfigSubnet:        gceSubnet,
		adapters.GCEConfigEndpoint:      gceEndpoint,
		adapters.GKEConfigProject:       gkeProject,
		adapters.GKEConfigLocation:      gkeLocation,
		adapters.GKEConfigCluster:       gkeCluster,
		adapters.GKEConfigProfiles:      gkeProfiles,
		adapters.GKEConfigNodeLocations: gkeNodeLocations,
	})
	cancel()
	if err != nil {
		setupLog.Error(err, "creating compute provider", "provider", provider)
		os.Exit(1)
	}
	setupLog.Info("compute provider initialized", "provider", computeAdapter.Type())

	// Load the GCE VM type catalog from KYBER_GCE_VM_TYPE_CATALOG (a JSON array
	// rendered by the Helm chart from compute.gce.vmTypeCatalog). Falls back to
	// the built-in shortlist when the env var is absent.
	gceVMTypeCatalog := internalapi.DefaultGCEVMTypeCatalog()
	if catalogJSON := os.Getenv("KYBER_GCE_VM_TYPE_CATALOG"); catalogJSON != "" {
		parsed, err := internalapi.ParseVMTypeCatalog(catalogJSON)
		if err != nil {
			setupLog.Error(err, "failed to parse KYBER_GCE_VM_TYPE_CATALOG — using built-in defaults")
		} else {
			gceVMTypeCatalog = parsed
			setupLog.Info("GCE VM type catalog loaded from chart values", "count", len(gceVMTypeCatalog))
		}
	}

	// Platform reservation: chart-configured carve-out for kube-system / kyber-system
	// overhead. Defaults match values.yaml's controlPlane.platformReservation.
	platformResCPU := os.Getenv("KYBER_PLATFORM_RESERVATION_CPU")
	if platformResCPU == "" {
		platformResCPU = "1"
	}
	platformResMem := os.Getenv("KYBER_PLATFORM_RESERVATION_MEMORY")
	if platformResMem == "" {
		platformResMem = "1Gi"
	}
	platformResDisk := os.Getenv("KYBER_PLATFORM_RESERVATION_EPHEMERAL_STORAGE")
	if platformResDisk == "" {
		platformResDisk = "10Gi"
	}
	platformReservationCPU, err := resource.ParseQuantity(platformResCPU)
	if err != nil {
		setupLog.Error(err, "invalid KYBER_PLATFORM_RESERVATION_CPU; using 1")
		platformReservationCPU = resource.MustParse("1")
	}
	platformReservationMem, err := resource.ParseQuantity(platformResMem)
	if err != nil {
		setupLog.Error(err, "invalid KYBER_PLATFORM_RESERVATION_MEMORY; using 1Gi")
		platformReservationMem = resource.MustParse("1Gi")
	}
	platformReservationDisk, err := resource.ParseQuantity(platformResDisk)
	if err != nil {
		setupLog.Error(err, "invalid KYBER_PLATFORM_RESERVATION_EPHEMERAL_STORAGE; using 10Gi")
		platformReservationDisk = resource.MustParse("10Gi")
	}
	platformReservation := kyberv1.MachineCapacity{
		CPU:              platformReservationCPU,
		Memory:           platformReservationMem,
		EphemeralStorage: platformReservationDisk,
	}
	setupLog.Info("platform reservation configured",
		"cpu", platformReservation.CPU.String(),
		"memory", platformReservation.Memory.String(),
		"ephemeralStorage", platformReservation.EphemeralStorage.String())

	// NewMachineReconciler reads KYBER_K3S_JOIN_TOKEN and KYBER_K3S_SERVER_URL from env
	// (mounted from the Helm Secret) and stores them on the reconciler for use when
	// provisioning k3s worker VMs. Logs a warning when unset (dev/mock mode only).
	machineReconciler := machinecontroller.NewMachineReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		mgr.GetEventRecorderFor("machine-controller"),
		computeAdapter,
	)
	machineReconciler.AlertSink = alertSink
	machineReconciler.PlatformReservation = platformReservation
	if err := machineReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to setup machine controller")
		os.Exit(1)
	}

	// Start the public HTTP API on port 8080.
	// This serves /api/v1/* (API key auth) and /webhooks/* (Telegram secret auth).
	// Build ValidRuntimes set from adapter registry keys so the API can
	// reject agents with unknown runtimes at creation time (400) rather than
	// letting the controller fail later with an adapter-not-found error.
	validRuntimes := make(map[string]bool, len(adapterRegistry))
	// restartSessionCommands mirrors the adapter registry — runtimes that
	// return nil from RestartSessionCommand() are omitted so the API's 501
	// branch fires for them (#128 D5).
	restartSessionCommands := make(map[string][]string, len(adapterRegistry))
	// compactSessionCommands is the same idea for in-session compaction —
	// runtimes returning nil are omitted so the API answers 501 for them.
	compactSessionCommands := make(map[string][]string, len(adapterRegistry))
	// runtimeImages lets the API reject an agent whose runtime is registered
	// but has no image pinned on this install (kyber#674). Registration alone
	// is not enough: the codex adapter self-registers unconditionally, so
	// "codex" is a valid runtime even when image.codex.tag was never set and
	// KYBER_CODEX_RUNTIME_IMAGE is therefore absent. Creating such an agent
	// used to succeed and then fail invisibly forever in the controller.
	runtimeImages := make(map[string]string, len(adapterRegistry))
	for rt, ad := range adapterRegistry {
		validRuntimes[rt] = true
		img := ad.Image()
		runtimeImages[rt] = img
		if img == "" {
			setupLog.Info("runtime registered but has no image configured on this install; "+
				"agents for it will be rejected at creation until the image is pinned",
				"runtime", rt)
		}
		if cmd := ad.RestartSessionCommand(); len(cmd) > 0 {
			restartSessionCommands[rt] = cmd
		}
		if cmd := ad.CompactSessionCommand(); len(cmd) > 0 {
			compactSessionCommands[rt] = cmd
		}
	}

	// Inbound prompts (kyber#208 Phase 1): per-binding rate limiter, body-hash
	// dedup (Redis when available, in-memory fallback), and a per-agent
	// in-flight queue. The queue's handler execs into the agent pod via
	// kyber-job-dispatch --stdin, so it needs a back-reference to publicAPI;
	// we declare publicAPI as a pointer first, build the queue with a closure
	// that captures it, then construct the Server.
	var publicAPI *internalapi.Server
	var inboundDeduper inbound.Deduper
	if redisClient != nil {
		inboundDeduper = inbound.NewRedisDeduper(redisClient)
		setupLog.Info("InboundDeduper: using Redis")
	} else {
		inboundDeduper = inbound.NewMemoryDeduper()
		setupLog.Info("InboundDeduper: using in-memory (state will not survive pod restart)")
	}
	// EnvelopeCache (kyber#208 Phase 3): same Redis/in-memory branching as
	// the deduper. Production uses Redis so envelopes survive control-plane
	// restarts and stay replayable for the full 7-day TTL.
	var inboundEnvelopeCache inbound.EnvelopeCache
	if redisClient != nil {
		inboundEnvelopeCache = inbound.NewRedisEnvelopeCache(redisClient)
		setupLog.Info("InboundEnvelopeCache: using Redis")
	} else {
		inboundEnvelopeCache = inbound.NewMemoryEnvelopeCache()
		setupLog.Info("InboundEnvelopeCache: using in-memory (envelopes will not survive pod restart)")
	}
	// Runtime detection (kyber#375 PR-A of #374): poller fetches the
	// npm registry + Anthropic Models API on a configurable cadence and
	// publishes results into a Redis-backed cache so multi-replica
	// installs see a consistent /available response. Disabled when
	// KYBER_RUNTIMEDETECT_ENABLED=false; in-memory fallback when Redis
	// is unavailable (dev/test).
	var runtimeDetectCache runtimedetect.Cache
	runtimeDetectEnabled := os.Getenv("KYBER_RUNTIMEDETECT_ENABLED") != "false"
	if runtimeDetectEnabled {
		if redisClient != nil {
			runtimeDetectCache = runtimedetect.NewRedisCache(redisClient, 0)
			setupLog.Info("runtimedetect: cache using Redis")
		} else {
			runtimeDetectCache = runtimedetect.NewMemoryCache()
			setupLog.Info("runtimedetect: cache using in-memory (single-replica only)")
		}
		// Wire the detection snapshot into the Claude Code adapter so
		// EnvVars() can size KYBER_MODEL_CONTEXT_WINDOW from an auto-detected
		// window (kyber#492) — the layer between the override map and the
		// tokenreport.LimitFor floor. Bounded + memoized so the
		// pod-construction hot path never stalls on a slow cache. Same guarded
		// cast as the ContextWindows wiring above; left nil when detection is
		// disabled, so the adapter falls back exactly as before.
		if cc, ok := adapterRegistry["claude-code"].(*claudecode.ClaudeCodeAdapter); ok {
			cc.Snapshots = &runtimedetect.SnapshotResolver{
				Cache:   runtimeDetectCache,
				TTL:     30 * time.Second,
				Timeout: 2 * time.Second,
			}
			setupLog.Info("runtimedetect: snapshot wired into claude-code adapter for KYBER_MODEL_CONTEXT_WINDOW")
		}
		runtimeDetectCadence := time.Hour
		if v := os.Getenv("KYBER_RUNTIMEDETECT_CADENCE_SECONDS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				runtimeDetectCadence = time.Duration(n) * time.Second
			} else {
				setupLog.Info("ignoring invalid KYBER_RUNTIMEDETECT_CADENCE_SECONDS; using default 1h", "value", v)
			}
		}
		runtimeDetectVersionLimit := runtimedetect.DefaultVersionLimit
		if v := os.Getenv("KYBER_RUNTIMEDETECT_VERSION_LIMIT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				runtimeDetectVersionLimit = n
			}
		}
		runtimeDetectPoller := &runtimedetect.Poller{
			Cache:          runtimeDetectCache,
			Npm:            runtimedetect.NewNpmClient("", 15*time.Second),
			CodexNpm:       runtimedetect.NewNpmClient(runtimedetect.DefaultCodexNpmRegistryURL, 15*time.Second),
			ContextWindows: contextWindowResolver,
			Cadence:        runtimeDetectCadence,
			VersionLimit:   runtimeDetectVersionLimit,
		}
		internalSrv.SetRuntimeDetectCache(runtimeDetectCache)
		if err := mgr.Add(manager.RunnableFunc(runtimeDetectPoller.Start)); err != nil {
			setupLog.Error(err, "unable to register runtimedetect poller")
			os.Exit(1)
		}
		setupLog.Info("runtimedetect: poller registered",
			"cadenceSeconds", int(runtimeDetectCadence.Seconds()),
			"versionLimit", runtimeDetectVersionLimit)
	} else {
		setupLog.Info("runtimedetect: disabled (KYBER_RUNTIMEDETECT_ENABLED=false); /api/v1/available will serve empty contract")
	}

	// Update checking, and — where the chart has enabled it — installing.
	// The checker only ever reads; the applier creates a Job that does the
	// upgrade, because the control plane cannot supervise its own replacement.
	// See dave-agent spec 2026-08-10-kyber-owns-its-deployment.md.
	var updateChecker *updates.Checker
	var updateStore *updates.Store
	var updateApplier *updates.Applier
	if os.Getenv("KYBER_UPDATES_ENABLED") == "true" {
		policyCM := os.Getenv("KYBER_UPDATE_POLICY_CONFIGMAP")
		if policyCM == "" {
			policyCM = updates.DefaultConfigMapName
		}
		updateStore = &updates.Store{
			Client:        mgr.GetClient(),
			Namespace:     kyberNamespace,
			ConfigMapName: policyCM,
		}
		cadence := updates.DefaultCadence
		if raw := os.Getenv("KYBER_UPDATES_CADENCE_SECONDS"); raw != "" {
			if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
				cadence = time.Duration(secs) * time.Second
			} else {
				setupLog.Info("updates: ignoring unparseable KYBER_UPDATES_CADENCE_SECONDS", "value", raw)
			}
		}
		// Self-upgrade. Constructing the Applier does not grant anything: the
		// Job it creates runs as a ServiceAccount the chart only provisions
		// when selfUpgrade.enabled is true, so an install that has not opted
		// in cannot start one even if these env vars were set by hand.
		//
		// Applier.Configured() is the single source of truth for whether the
		// apply route and the applySupported flag are live, which is why every
		// field is read here rather than defaulted deeper down — a half-set
		// configuration must read as "off", not as a button that 500s.
		updateApplier = &updates.Applier{
			Client:                 mgr.GetClient(),
			Namespace:              kyberNamespace,
			ControlPlaneDeployment: os.Getenv("KYBER_CONTROL_PLANE_DEPLOYMENT"),
			ReleaseName:            os.Getenv("KYBER_SELF_UPGRADE_RELEASE"),
			ChartRef:               os.Getenv("KYBER_SELF_UPGRADE_CHART_REF"),
			ServiceAccount:         os.Getenv("KYBER_SELF_UPGRADE_SERVICE_ACCOUNT"),
			HealthURL:              os.Getenv("KYBER_SELF_UPGRADE_HEALTH_URL"),
			LogLevel:               os.Getenv("KYBER_SELF_UPGRADE_LOG_LEVEL"),
		}
		if !updateApplier.Configured() {
			setupLog.Info("updates: self-upgrade not configured; /api/v1/updates/apply will return 503 and applySupported will be false")
			updateApplier = nil
		} else {
			setupLog.Info("updates: self-upgrade enabled",
				"release", updateApplier.ReleaseName,
				"chart", updateApplier.ChartRef,
				"serviceAccount", updateApplier.ServiceAccount)
		}

		// The main channel reads the chart registry rather than GitHub
		// releases: on that channel a commit is only an update once its chart
		// exists to pull. A bad reference is logged and disables the main
		// channel rather than failing startup — a cluster on stable should not
		// be taken down by a value it never uses.
		chartFeed, chartFeedErr := updates.ChartFeedFromRef(
			os.Getenv("KYBER_UPDATES_CHART_REF"), os.Getenv("KYBER_UPDATES_TOKEN"))
		if chartFeedErr != nil {
			setupLog.Error(chartFeedErr, "updates: chart reference unusable; the main channel will report a failed check")
			chartFeed = nil
		}

		updateChecker = &updates.Checker{
			Feed: &updates.FeedClient{
				Repo:  os.Getenv("KYBER_UPDATES_REPO"),
				Token: os.Getenv("KYBER_UPDATES_TOKEN"),
			},
			ChartFeed:              chartFeed,
			Applier:                updateApplier,
			Store:                  updateStore,
			K8sClient:              mgr.GetClient(),
			Namespace:              kyberNamespace,
			ControlPlaneDeployment: os.Getenv("KYBER_CONTROL_PLANE_DEPLOYMENT"),
			CurrentVersion:         resolveDisplayVersion(),
			Cadence:                cadence,
		}
		if err := mgr.Add(manager.RunnableFunc(updateChecker.Start)); err != nil {
			setupLog.Error(err, "unable to register update checker")
			os.Exit(1)
		}
		setupLog.Info("updates: checker registered",
			"repo", updateChecker.Feed.Repo,
			"cadenceSeconds", int(cadence.Seconds()),
			"policyConfigMap", policyCM,
			"currentVersion", updateChecker.CurrentVersion,
			// Empty here means ownership detection cannot run, so the cluster
			// reports managedBy=unknown and refuses self-upgrade. Logged
			// because that is a silent capability loss otherwise.
			"controlPlaneDeployment", updateChecker.ControlPlaneDeployment)
	} else {
		setupLog.Info("updates: disabled (KYBER_UPDATES_ENABLED not \"true\"); /api/v1/updates will return 503")
	}

	// kyber#431/#437: durable archive log reader. Provider-agnostic — the backend
	// is selected by KYBER_LOG_ARCHIVE_BACKEND (default "gcs", backward-compatible):
	//   gcs (or unset) → GCS via node ADC (no static key).
	//   s3             → any S3-compatible store (MinIO/AWS) via Secret creds.
	// When the selected backend's required config is absent, source=archive
	// returns 503 (with a reason naming the missing keys) and the live kubelet
	// tail is unaffected.
	// buildArchiveReader constructs a reader for one object-storage surface root
	// from the shared KYBER_LOG_ARCHIVE_* config. rootPrefix selects the lane
	// ("agents/" for source=archive, "transcripts/" for source=transcript,
	// kyber#446); both lanes live in the SAME bucket and use the SAME backend +
	// credentials — only the root prefix differs. surface labels the log lines.
	// Returns (nil, reason) when the selected backend's config is absent so the
	// caller can surface a self-diagnosing 503.
	archiveBackend := strings.ToLower(strings.TrimSpace(os.Getenv("KYBER_LOG_ARCHIVE_BACKEND")))
	if archiveBackend == "" {
		archiveBackend = "gcs"
	}
	archiveBucket := os.Getenv("KYBER_LOG_ARCHIVE_BUCKET")
	buildArchiveReader := func(rootPrefix, surface string) (internalapi.ArchiveReader, string) {
		switch archiveBackend {
		case "gcs":
			if archiveBucket != "" {
				gcsReader, err := internalapi.NewGCSArchiveReader(context.Background(), archiveBucket, rootPrefix)
				if err != nil {
					setupLog.Error(err, "unable to create GCS log reader; surface will be unavailable",
						"surface", surface, "bucket", archiveBucket)
					return nil, "KYBER_LOG_ARCHIVE_BACKEND=gcs but the GCS client failed to initialize"
				}
				setupLog.Info("log reader: GCS configured", "surface", surface, "bucket", archiveBucket, "rootPrefix", rootPrefix)
				return gcsReader, ""
			}
			setupLog.Info("log reader: disabled (KYBER_LOG_ARCHIVE_BUCKET unset); returns 503", "surface", surface)
			return nil, "KYBER_LOG_ARCHIVE_BUCKET unset"
		case "s3":
			endpoint := os.Getenv("KYBER_LOG_ARCHIVE_ENDPOINT")
			region := os.Getenv("KYBER_LOG_ARCHIVE_REGION")
			accessKey := os.Getenv("KYBER_LOG_ARCHIVE_ACCESS_KEY")
			secretKey := os.Getenv("KYBER_LOG_ARCHIVE_SECRET_KEY")
			// TLS defaults ON; opt out explicitly (e.g. for plaintext cluster-internal MinIO).
			useTLS := !strings.EqualFold(os.Getenv("KYBER_LOG_ARCHIVE_USE_TLS"), "false")
			// Name every missing required key (never values) so the 503 is self-diagnosing.
			var missing []string
			if endpoint == "" {
				missing = append(missing, "KYBER_LOG_ARCHIVE_ENDPOINT")
			}
			if archiveBucket == "" {
				missing = append(missing, "KYBER_LOG_ARCHIVE_BUCKET")
			}
			if accessKey == "" {
				missing = append(missing, "KYBER_LOG_ARCHIVE_ACCESS_KEY")
			}
			if secretKey == "" {
				missing = append(missing, "KYBER_LOG_ARCHIVE_SECRET_KEY")
			}
			if len(missing) > 0 {
				setupLog.Info("log reader: disabled (S3 backend missing config); returns 503",
					"surface", surface, "missing", strings.Join(missing, ","))
				return nil, "KYBER_LOG_ARCHIVE_BACKEND=s3 requires " + strings.Join(missing, ", ")
			}
			s3Reader, err := internalapi.NewS3ArchiveReader(context.Background(), endpoint, archiveBucket, region, accessKey, secretKey, useTLS, rootPrefix)
			if err != nil {
				setupLog.Error(err, "unable to create S3 log reader; surface will be unavailable",
					"surface", surface, "endpoint", endpoint, "bucket", archiveBucket)
				return nil, "KYBER_LOG_ARCHIVE_BACKEND=s3 but the S3 client failed to initialize"
			}
			setupLog.Info("log reader: S3 configured", "surface", surface, "endpoint", endpoint, "bucket", archiveBucket, "useTLS", useTLS, "rootPrefix", rootPrefix)
			return s3Reader, ""
		default:
			setupLog.Info("log reader: disabled (unrecognized KYBER_LOG_ARCHIVE_BACKEND); returns 503",
				"surface", surface, "backend", archiveBackend)
			return nil, "KYBER_LOG_ARCHIVE_BACKEND=" + archiveBackend + " is not a recognized backend (want gcs|s3)"
		}
	}
	// source=archive lane (agent boot-stdout, kyber#437) and source=transcript
	// lane (Claude Code session JSONL, kyber#446) — same bucket, distinct roots.
	archiveReader, archiveDisabledReason := buildArchiveReader("agents/", "archive")
	transcriptReader, transcriptDisabledReason := buildArchiveReader("transcripts/", "transcript")

	inboundRateLimiter := inbound.NewRateLimiter()
	inboundQueue := inbound.NewQueue(func(ctx context.Context, job inbound.Job) {
		if publicAPI == nil {
			setupLog.Info("inbound queue: publicAPI not yet initialised; dropping job",
				"agent", job.Agent, "binding", job.Binding, "request_id", job.RequestID)
			return
		}
		podName := "agent-" + job.Agent
		jobName := "inbound-" + job.RequestID
		// Hold the job while the agent is mid-restart (set-model, secret
		// roll, operator restart) instead of delivering into the
		// terminating pod — the dying session would answer the prompt and
		// the reply dies with the pod. Per-agent worker goroutines mean
		// this waits only this agent's queue. On timeout, fall through
		// and attempt delivery anyway (the exec fails cleanly when
		// there's no pod, matching pre-gate behavior).
		waitCtx, cancelWait := context.WithTimeout(ctx, 3*time.Minute)
		if err := publicAPI.WaitAgentRunning(waitCtx, job.Agent, 3*time.Minute); err != nil {
			setupLog.Info("inbound dispatch: agent not Running after wait; attempting delivery anyway",
				"agent", job.Agent, "binding", job.Binding,
				"request_id", job.RequestID, "reason", err.Error())
		}
		cancelWait()
		// Bound the exec; the in-pod dispatcher is fast (send-keys + a POST)
		// but a stuck SPDY connection would otherwise hold a queue slot
		// indefinitely.
		execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, stderr, err := publicAPI.ExecRunJobStdin(execCtx, podName, jobName, strings.NewReader(job.Envelope))
		if err != nil {
			setupLog.Error(err, "inbound dispatch exec failed",
				"agent", job.Agent, "binding", job.Binding,
				"request_id", job.RequestID, "stderr", stderr)
		}
	})

	// KYBER_MAX_CONCURRENT_READS bounds simultaneous in-flight windowed log reads
	// (source=archive|transcript) so concurrent large reads can't collectively
	// crash the control-plane (kyber#463). Unset/invalid/non-positive → the
	// package default (a non-positive value can never disable the gate).
	maxConcurrentReads := 0
	if v := os.Getenv("KYBER_MAX_CONCURRENT_READS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxConcurrentReads = n
		} else {
			setupLog.Info("ignoring invalid KYBER_MAX_CONCURRENT_READS; using default", "value", v)
		}
	}

	loggingComponentLevels := map[string]string{}
	if raw := os.Getenv("KYBER_LOG_COMPONENT_OVERRIDES"); raw != "" {
		var configured map[string]struct {
			Level string `json:"level"`
		}
		if err := json.Unmarshal([]byte(raw), &configured); err != nil {
			setupLog.Error(err, "invalid KYBER_LOG_COMPONENT_OVERRIDES")
			os.Exit(1)
		}
		for component, config := range configured {
			if _, err := logging.ParseLevel(config.Level); err != nil {
				setupLog.Error(err, "invalid component logging level", "component", component)
				os.Exit(1)
			}
			if config.Level != "" {
				loggingComponentLevels[component] = strings.ToLower(config.Level)
			}
		}
	}
	loggingArchiveRetention := 0
	if raw := os.Getenv("KYBER_LOG_ARCHIVE_RETENTION_DAYS"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			setupLog.Error(err, "invalid KYBER_LOG_ARCHIVE_RETENTION_DAYS", "value", raw)
			os.Exit(1)
		}
		loggingArchiveRetention = value
	}

	// Caller-level authorization (kyber#474). Scoped API keys are an optional
	// `callers` JSON document on the api-key Secret; KYBER_AUTHZ_ENFORCE gates
	// whether an under-scoped caller is blocked (default off = permissive/audit).
	// Fail-closed on a malformed callers doc — log and reject all scoped keys
	// (the legacy full-scope key still works) rather than silently granting.
	authzEnforce := os.Getenv("KYBER_AUTHZ_ENFORCE") == "true"
	var scopedCallers []internalapi.ScopedCaller
	if raw := os.Getenv("KYBER_API_CALLERS"); raw != "" {
		parsed, err := internalapi.ParseScopedCallers(raw)
		if err != nil {
			setupLog.Error(err, "invalid KYBER_API_CALLERS — ignoring scoped callers (legacy key still works)")
		} else {
			scopedCallers = parsed
			// keyFrom resolution (kyber#557): fill Secret-referenced keys before
			// the authenticator is built. Same direct-client shape as the GitHub
			// App Secret load above — the manager's cached client isn't started
			// yet, and Background+timeout keeps a fast SIGTERM from silently
			// degrading the config. Fail-closed per caller: an unresolvable
			// reference drops exactly that caller, loudly; inline entries and
			// the legacy key are unaffected.
			needsResolve := false
			for _, c := range parsed {
				if c.KeyFrom != nil {
					needsResolve = true
					break
				}
			}
			rejectedCount := 0
			if needsResolve {
				directClient, cerr := client.New(restCfg, client.Options{Scheme: scheme})
				if cerr != nil {
					setupLog.Error(cerr, "creating direct client for keyFrom caller resolution — rejecting keyFrom callers (inline callers and legacy key still work)")
					var inlineOnly []internalapi.ScopedCaller
					for _, c := range parsed {
						if c.KeyFrom == nil {
							inlineOnly = append(inlineOnly, c)
						}
					}
					rejectedCount = len(parsed) - len(inlineOnly)
					scopedCallers = inlineOnly
				} else {
					loadCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					resolved, rejected := internalapi.ResolveScopedCallers(loadCtx, directClient, kyberNamespace, parsed)
					cancel()
					for _, rej := range rejected {
						// Reference names only — never a Secret value.
						setupLog.Error(rej.Err, "keyFrom caller not resolved — rejecting that caller (kyber#557)",
							"caller", rej.Caller, "secret", rej.Secret, "key", rej.Key)
					}
					rejectedCount = len(rejected)
					scopedCallers = resolved
				}
			}
			setupLog.Info("loaded scoped API callers (kyber#474)",
				"count", len(scopedCallers), "rejected", rejectedCount, "enforce", authzEnforce)
		}
	}

	publicAPI = &internalapi.Server{
		K8sClient:                mgr.GetClient(),
		TokenStore:               tokenStore,
		TokenAccumulator:         tokenAccumulator,
		APIKey:                   os.Getenv("KYBER_API_KEY"),
		APIKeySecretName:         os.Getenv("KYBER_API_KEY_SECRET_NAME"),
		Callers:                  scopedCallers,
		AuthzEnforce:             authzEnforce,
		PublicURL:                os.Getenv("KYBER_PUBLIC_URL"),
		AnthropicTokenURL:        os.Getenv("ANTHROPIC_TOKEN_URL"),
		Addr:                     internalapi.DefaultPublicPort,
		Namespace:                kyberNamespace,
		LoggingGlobalLevel:       os.Getenv("KYBER_LOG_GLOBAL_LEVEL"),
		LoggingComponentLevels:   loggingComponentLevels,
		LoggingArchiveRetention:  loggingArchiveRetention,
		ValidRuntimes:            validRuntimes,
		RuntimeImages:            runtimeImages,
		RestartSessionCommands:   restartSessionCommands,
		CompactSessionCommands:   compactSessionCommands,
		Clientset:                clientset,
		ArchiveReader:            archiveReader,
		ArchiveDisabledReason:    archiveDisabledReason,
		TranscriptReader:         transcriptReader,
		TranscriptDisabledReason: transcriptDisabledReason,
		MaxConcurrentReads:       maxConcurrentReads,
		RestConfig:               restCfg,
		InformerCache:            mgr.GetCache(),
		ComputeProvider:          os.Getenv("KYBER_COMPUTE_PROVIDER"),
		CapacityProvider: func() adapters.CapacityProvider {
			provider, _ := computeAdapter.(adapters.CapacityProvider)
			return provider
		}(),
		GCEVMTypeCatalog:       gceVMTypeCatalog,
		Recorder:               mgr.GetEventRecorderFor("kyber-api"),
		BuildSHA:               BuildSHA,
		BuildDate:              BuildDate,
		ChartVersion:           resolveDisplayVersion(),         // build-injected version wins; chart file is the local/dev fallback (kyber#482)
		ClusterName:            os.Getenv("KYBER_CLUSTER_NAME"), // "" is valid; PWA renders blank header
		AllowedOrigins:         parseCORSAllowedOrigins(os.Getenv("KYBER_CORS_ALLOWED_ORIGINS")),
		Substrate:              kyberNamespace,
		InboundDeduper:         inboundDeduper,
		InboundRateLimiter:     inboundRateLimiter,
		InboundQueue:           inboundQueue,
		InboundEnvelopeCache:   inboundEnvelopeCache,
		AnthropicKeySecretName: os.Getenv("KYBER_ANTHROPIC_KEY_SECRET_NAME"),
		RuntimeDetectCache:     runtimeDetectCache,
		// #500: the serve-time token-budget gauge resolves the context window
		// through the SAME detection snapshot /available + the pod adapter use,
		// via a bounded SnapshotResolver (30s TTL + 2s timeout). nil-safe — a
		// nil cache (detection disabled) falls through to the ConfigMap→floor
		// path, so the gauge never blocks or 500s.
		Snapshots:                  &runtimedetect.SnapshotResolver{Cache: runtimeDetectCache, TTL: 30 * time.Second, Timeout: 2 * time.Second},
		GithubAppClient:            githubAppClient,
		IdentityRepoOwner:          identityRepoOwner,
		MetricsStore:               metricsStore,
		NodeStore:                  nodeStore,
		StateChangeAccumulator:     stateChangeAccum,
		FleetDefaultsConfigMapName: fleetDefaultsCM,
		UpdateChecker:              updateChecker,
		UpdateStore:                updateStore,
		UpdateApplier:              updateApplier,
		ConfigExporter:             &configexport.Reader{Client: mgr.GetClient(), Namespace: kyberNamespace},
		FleetDefaultsInvalidator:   fleetDefaultsResolver,
		// #396: serve-time context-window resolution for the token-budget %.
		// Same resolver the detection poller uses (built at ~:405) — one source
		// of truth. May be nil when the override ConfigMap isn't configured;
		// LookupNormalized floors safely in that case.
		ContextWindows: contextWindowResolver,
		MetricsConfig: metrics.Config{
			PrometheusURL:                os.Getenv("KYBER_METRICS_PROMETHEUS_URL"),
			TokenRatesPath:               os.Getenv("KYBER_METRICS_TOKEN_RATES_PATH"),
			PrometheusInsecureSkipVerify: os.Getenv("KYBER_METRICS_PROMETHEUS_INSECURE") == "true",
		},
	}
	if os.Getenv("KYBER_DEV_COMPUTE_CONTROL") == "true" {
		if computeSimulation == nil {
			computeSimulation, _ = computeAdapter.(adapters.SimulationController)
		}
		if computeSimulation == nil {
			setupLog.Error(nil, "KYBER_DEV_COMPUTE_CONTROL requires a simulated compute provider", "provider", provider)
			os.Exit(1)
		}
		publicAPI.ComputeSimulation = computeSimulation
		setupLog.Info("development compute scenario control enabled")
	}
	if publicAPI.APIKey == "" {
		setupLog.Info("warning: KYBER_API_KEY not set — public API will reject all requests")
	}
	// Inbound event aggregator (kyber#208 audit slice): batches high-
	// volume Events (rate-limited, queue-full) so a flood produces 1
	// Event per minute, not N. Wired AFTER publicAPI is constructed so it
	// can hold the same Recorder and namespace. Per-occurrence Events
	// (sig-mismatch, unmatched, filter-rejected, dispatched, config-
	// error) are emitted directly from the receiver and bypass this.
	inboundEventAgg := internalapi.NewInboundEventAggregator(
		publicAPI.Recorder, publicAPI.K8sClient, publicAPI.Namespace)
	publicAPI.InboundEventAggregator = inboundEventAgg

	if err := mgr.Add(manager.RunnableFunc(publicAPI.Start)); err != nil {
		setupLog.Error(err, "unable to add public API to manager")
		os.Exit(1)
	}
	// Drain in-flight inbound jobs on shutdown so we don't leave half-sent
	// prompts on the wire when the pod is recycled.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		inboundQueue.Stop()
		return nil
	})); err != nil {
		setupLog.Error(err, "unable to register inbound queue shutdown hook")
		os.Exit(1)
	}
	// Drain pending aggregated Event counters on shutdown so a clean
	// pod-recycle still surfaces "what we saw in the last partial
	// minute" to the Events API.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		inboundEventAgg.Stop()
		return nil
	})); err != nil {
		setupLog.Error(err, "unable to register inbound event aggregator shutdown hook")
		os.Exit(1)
	}

	// Register health/ready probes so the controller-runtime health endpoint at
	// HealthProbeBindAddress (:8081) returns 200. The Helm deployment's liveness and
	// readiness probes hit this port. Without registered checks, the endpoint returns 404.
	if err := mgr.AddHealthzCheck("healthz", func(_ *http.Request) error { return nil }); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", func(_ *http.Request) error { return nil }); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// parseCORSAllowedOrigins splits the raw KYBER_CORS_ALLOWED_ORIGINS env var
// (comma-separated) into a trimmed slice. Empty string returns nil so that
// an unset env var cleanly disables CORS (same-origin only).
func parseCORSAllowedOrigins(raw string) []string {
	if raw == "" {
		return nil
	}
	var origins []string
	for _, o := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(o); trimmed != "" {
			origins = append(origins, trimmed)
		}
	}
	return origins
}

// maskDSN returns a log-safe representation of a PostgreSQL DSN with the
// password redacted. If parsing fails, returns a generic redacted placeholder
// so we never leak credentials to logs.
func maskDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	if u, err := neturl.Parse(dsn); err == nil && u.User != nil {
		u.User = neturl.User(u.User.Username())
		return u.String()
	}
	return "<postgres DSN redacted>"
}

// nodeAgentTokenSecretName is the singleton Secret holding the node-agent's
// control-plane-signed pod-token (kyber#566). The node-agent DaemonSet mounts
// it; the internal API admits this identity to the machine/node routes only.
const nodeAgentTokenSecretName = "kyber-node-agent-token"

// ensureNodeAgentToken mints (or rotation-updates) the node-agent's pod-token
// Secret. Unlike per-agent tokens (minted by the reconciler), the node-agent is
// a DaemonSet with no Agent CR, so the control plane — the sole holder of the
// signing key — ensures its token directly. Idempotent: a re-run is a no-op
// unless the signing key rotated, in which case the value is updated in place.
// No owner reference (there is no owning CR); the Secret is a control-plane
// singleton in the kyber namespace.
func ensureNodeAgentToken(ctx context.Context, c client.Client, namespace string, key []byte) error {
	token := podtoken.Sign(podtoken.NodeAgentIdentity, key)
	nn := apitypes.NamespacedName{Name: nodeAgentTokenSecretName, Namespace: namespace}

	existing := &corev1.Secret{}
	err := c.Get(ctx, nn, existing)
	if err == nil {
		if string(existing.Data[agentcontroller.PodTokenSecretKey]) == token {
			return nil
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data[agentcontroller.PodTokenSecretKey] = []byte(token)
		return c.Update(ctx, existing)
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeAgentTokenSecretName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kyber-controller",
				"kyber.io/secret-kind":         "node-agent-pod-token",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{agentcontroller.PodTokenSecretKey: []byte(token)},
	}
	if err := c.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
