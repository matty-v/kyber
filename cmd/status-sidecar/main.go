// kyber-status-sidecar (kyber#248). Phase A foundation: POSTs a
// heartbeat event every 5 seconds to the control plane's
// /internal/agents/{name}/status-event endpoint. No runtime-specific
// signal yet; that's kyber#249, which adds a per-runtime Probe and
// pushes "activity" events through the same endpoint.
//
// The sidecar is intentionally tiny: env-driven config, simple loop,
// per-event POST (HTTP keepalive on the underlying transport amortises
// the TCP handshake at 5s cadence). Healthcheck on :8090 reports
// whether the LAST event was successfully delivered. Crashes are
// tolerated by kubelet (RestartPolicy: Always inherited from the Pod).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/matty-v/kyber/pkg/logging"
)

const (
	// metricsSnapshotInterval is the cadence for metrics-snapshot POSTs to
	// /internal/agents/{name}/status (kyber#343). The heartbeat-of-record
	// (CRD status update via status-event) fires every other tick (30s).
	metricsSnapshotInterval = 15 * time.Second
	heartbeatInterval       = metricsSnapshotInterval
	// livenessStaleThreshold (kyber#575) bounds how long the heartbeat loop may
	// go without advancing before /healthz reports unhealthy. Sized at 3× the
	// loop interval so a transient control-plane outage (POSTs failing, loop
	// still ticking) stays HEALTHY — only a genuinely stalled/deadlocked loop
	// is reported dead. Conflating "control plane reachable" with "process
	// alive" is what let a control-plane blip get every sidecar SIGTERM'd by
	// the kubelet liveness probe and, under restartPolicy: Never, stay dead.
	livenessStaleThreshold = 3 * heartbeatInterval
	healthAddr             = ":8090"
	// localhostAddr is where the sidecar accepts in-pod status events
	// from runtime-specific binaries (kyber#249). Per-runtime reporters
	// (e.g. kyber-token-reporter for claude-code) POST events to
	// http://localhost:8091/event; the sidecar forwards each event to
	// the control plane's /internal/agents/{name}/status-event with
	// agent name + auth applied. Runtime binaries don't need to know
	// the control-plane URL or pod-token.
	localhostAddr = "127.0.0.1:8091"
	postTimeout   = 5 * time.Second
	// podTokenPath matches kyber-token-reporter's convention
	// (cmd/token-reporter/main.go:51). Optional — absent on dev setups,
	// the internal :8082 port doesn't currently require auth, but we send
	// the bearer when it's there for forward-compat.
	podTokenPath = "/var/run/secrets/kyber/pod-token"
	persistPath  = "/persist"
)

// lastPostOK mirrors whether the most recent heartbeat POST succeeded.
// Retained for observability (logged/metric'd); it no longer drives liveness
// (kyber#575) — a failed POST means the control plane is unreachable, not that
// this sidecar is unhealthy.
var lastPostOK atomic.Bool

// lastLoopTick holds the unix-nano timestamp of the most recent heartbeat-loop
// iteration. It IS the liveness signal: the loop advances it every tick
// regardless of POST outcome, so /healthz reports "the sidecar process is alive
// and looping" rather than "the control plane is reachable" (kyber#575).
var lastLoopTick atomic.Int64

func main() {
	// KYBER_SIDECAR_LOG_LEVEL gates the snapshot/forwarder debug lines added
	// in kyber#360. Default is info; debug enables a single-grep view of
	// every /event POST + every metrics-snapshot tick (empty or posting).
	// The shared logger also validates info, warn, and error for the unified
	// component-level settings added by kyber#105. Read here (not in loadConfig)
	// so it shapes the logger before any other startup work runs.
	logger, err := logging.New(logging.Config{
		Component: "status-sidecar",
		Level:     os.Getenv("KYBER_SIDECAR_LOG_LEVEL"),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("invalid config", "err", err)
		os.Exit(1)
	}
	logger.Info("starting kyber-status-sidecar",
		"agent", cfg.AgentName,
		"controlPlaneURL", cfg.ControlPlaneURL,
		"otelEndpoint", cfg.OtelEndpoint,
		"runtime", cfg.Runtime,
		"cgroupMemoryEventsPath", cfg.CgroupMemoryEventsPath,
	)
	if cfg.cgroupResolutionErr != nil {
		// OOM detection (#285) is dormant — log the reason once at startup
		// so an operator can see WHY rather than just observing the silent
		// no-op behavior in oomDetector.tick.
		logger.Warn("OOM detection disabled — pod cgroup path could not be derived",
			"err", cfg.cgroupResolutionErr,
			"hint", "set KYBER_CGROUP_MEMORY_EVENTS_PATH to override (cgroup namespacing or exotic CRI)",
		)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// OTel metrics (#256, #360 Cause E). initOTel always returns a usable
	// *Metrics — backed by an OTLP exporter when KYBER_OTEL_ENDPOINT is set
	// and reachable, by a noop MeterProvider otherwise. The activity-state
	// machine in activity_metrics.go drives the per-15s snapshot POSTs to
	// /internal/agents/{name}/status independently of whether anything is
	// being exported, so the dashboards keep populating in dev installs
	// without a collector. A non-nil err here means the operator configured
	// an endpoint but the OTLP wiring failed — log it and carry on with the
	// noop fallback (the alternative is silently dropping activity tracking,
	// which is exactly what #360 Cause E was).
	metrics, otelDown, err := initOTel(ctx, cfg.OtelEndpoint)
	if err != nil {
		logger.Warn("otel metrics export disabled (state machine still active)", "err", err)
	}
	logger.Info("otel metrics initialised",
		"endpoint", cfg.OtelEndpoint,
		"exportConfigured", cfg.OtelEndpoint != "" && err == nil,
	)
	defer func() {
		// Give the SDK a brief window to flush after shutdown.
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		_ = otelDown(shutdownCtx)
	}()

	// Seed the liveness watchdog before the probe can fire (initialDelay 10s),
	// so /healthz is healthy from startup until the first loop tick (kyber#575).
	lastLoopTick.Store(time.Now().UnixNano())
	go runHealth(ctx, logger)
	go runForwarder(ctx, cfg, logger, metrics)
	runHeartbeats(ctx, cfg, logger, metrics)
}

// runForwarder serves the localhost listener that in-pod runtime
// binaries POST status events to. The sidecar is the sole egress path
// from the agent pod to the control plane (kyber#249/#257); runtime
// binaries get a single localhost address and don't need to know the
// control-plane URL, pod token, or even the agent name.
//
// Path → control-plane forward target:
//
//	POST /event          → /internal/agents/{name}/status-event
//	POST /token-usage    → /internal/agents/{name}/token-usage
//	POST /runtime-version → /internal/agents/{name}/runtime-version
//	POST /runtime-catalog → /internal/agents/{name}/runtime-catalog
//	POST /skills          → /internal/agents/{name}/skills
//	POST /refresh-token  → /internal/agents/{name}/refresh-token
//	POST /codex-auth     → /internal/agents/{name}/codex-auth
//	POST /mcp            → MCP kyber-request-reply.respond tool
//	POST /a2a/mcp        → MCP outbound A2A client tools
//
// Each handler reads the body and forwards verbatim via postToCP. The
// sidecar applies agent identity + auth to the outbound request.
func runForwarder(ctx context.Context, cfg config, logger *slog.Logger, metrics *Metrics) {
	client := &http.Client{Timeout: postTimeout}
	mux := http.NewServeMux()
	// The /event handler additionally feeds the activity-state metrics
	// pipeline (kyber#256). Token-usage and refresh-token forwards bump
	// the simpler success/error counters so dashboards can still split
	// them by `type`.
	mux.HandleFunc("/event", forwardHandler(client, cfg, metrics, logger, "status-event", true))
	mux.HandleFunc("/token-usage", forwardHandler(client, cfg, metrics, logger, "token-usage", false))
	mux.HandleFunc("/runtime-version", forwardHandler(client, cfg, metrics, logger, "runtime-version", false))
	mux.HandleFunc("/runtime-catalog", forwardHandler(client, cfg, metrics, logger, "runtime-catalog", false))
	// The agent's skill inventory, scanned from its own filesystem by
	// kyber-skills at boot and on every identity sync.
	mux.HandleFunc("/skills", forwardHandler(client, cfg, metrics, logger, "skills", false))
	mux.HandleFunc("/refresh-token", forwardHandler(client, cfg, metrics, logger, "refresh-token", false))
	// Codex's equivalent of /refresh-token. Separate endpoint because the
	// payload is Codex's opaque auth.json document, not the Anthropic
	// access/refresh/expires trio (kyber#681).
	mux.HandleFunc("/codex-auth", forwardHandler(client, cfg, metrics, logger, "codex-auth", false))
	mux.HandleFunc("/mcp", (&requestMCPServer{client: client, cfg: cfg}).handle)
	mux.HandleFunc("/a2a/mcp", (&outboundA2AMCPServer{peers: cfg.A2APeers}).handle)
	mux.HandleFunc("/task-receipts", taskReceiptForwarder(client, cfg))
	mux.HandleFunc("/task-receipts/", taskReceiptForwarder(client, cfg))
	srv := &http.Server{
		Addr:              localhostAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	// Bind first, then log — so the "forwarder listening" line is a true
	// signal that the socket is accepting (kyber#270). The previous
	// pattern emitted the log immediately before ListenAndServe, which
	// races the actual bind by however long it takes net.Listen to
	// succeed and also routed bind failures (port collision) through
	// the generic "forwarder server died" branch instead of surfacing
	// them at the call site. Splitting Listen + Serve makes both
	// problems vanish.
	ln, err := net.Listen("tcp", localhostAddr)
	if err != nil {
		logger.Error("forwarder listen failed", "addr", localhostAddr, "err", err)
		return
	}
	logger.Info("forwarder listening", "addr", localhostAddr)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("forwarder server died", "err", err)
	}
}

// forwardHandler builds an HTTP handler that forwards the request body to
// the control plane's /internal/agents/{name}/{cpEndpoint} URL. When
// withActivityMetrics is true, the body is also parsed best-effort and fed
// into the activity-state accumulator (kyber#256) — only the /event path
// uses that. Other paths just bump the labeled success/error counters.
//
// The logger is used only for the kyber#360 debug-receipt line gated by
// KYBER_SIDECAR_LOG_LEVEL=debug; success/error counters are still the
// production observability path.
func forwardHandler(client *http.Client, cfg config, metrics *Metrics, logger *slog.Logger, cpEndpoint string, withActivityMetrics bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// kyber#360: surface forwarder receipts under debug so the next
		// "panels stay dark" diagnosis is a single grep, not a forensic
		// investigation. The forwarder's silence on success is what hid
		// Cause C through #356 and #358.
		logger.Debug("forwarder: received event", "agent", cfg.AgentName, "endpoint", cpEndpoint)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			metrics.RecordEventForwardError(r.Context(), "bad_request")
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Forward verbatim — the wire shape is identical to what runtime
		// binaries already produce. The sidecar just adds auth + the
		// per-agent URL prefix.
		status, err := postToCP(r.Context(), client, cfg, cpEndpoint, body)
		if err != nil {
			metrics.RecordEventForwardError(r.Context(), classifyForwardError(status))
			http.Error(w, "forward: "+err.Error(), http.StatusBadGateway)
			return
		}
		if withActivityMetrics {
			// Best-effort parse so the success counter is labeled by event
			// type and activity-state events feed the dwell-time accumulator.
			recordEventMetrics(r.Context(), metrics, body, time.Now())
		} else {
			metrics.RecordEventForwarded(r.Context(), cpEndpoint)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// postToCP sends a pre-marshalled body to /internal/agents/{name}/{cpEndpoint}.
// Used by every forwarder path AND the self-generated heartbeat loop. Returns
// the HTTP status code (0 on transport failures) so callers can classify
// failures into the labeled buckets the metrics pipeline cares about
// (cp_unreachable / cp_4xx / cp_5xx).
func postToCP(ctx context.Context, client *http.Client, cfg config, cpEndpoint string, body []byte) (int, error) {
	url := fmt.Sprintf("%s/internal/agents/%s/%s",
		cfg.ControlPlaneURL, cfg.AgentName, cpEndpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token, err := readPodToken(podTokenPath); err == nil && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("control plane returned %s", resp.Status)
	}
	return resp.StatusCode, nil
}

func postToCPJSON(ctx context.Context, client *http.Client, cfg config, cpEndpoint string, body []byte, dst any) (int, error) {
	url := fmt.Sprintf("%s/internal/agents/%s/%s", cfg.ControlPlaneURL, cfg.AgentName, cpEndpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token, err := readPodToken(podTokenPath); err == nil && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("control plane returned %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(dst); err != nil {
		return resp.StatusCode, fmt.Errorf("decode response: %w", err)
	}
	return resp.StatusCode, nil
}

func getFromCP(ctx context.Context, client *http.Client, cfg config, cpEndpoint string, dst any) (int, error) {
	url := fmt.Sprintf("%s/internal/agents/%s/%s", cfg.ControlPlaneURL, cfg.AgentName, cpEndpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	if token, err := readPodToken(podTokenPath); err == nil && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("control plane returned %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(dst); err != nil {
		return resp.StatusCode, fmt.Errorf("decode response: %w", err)
	}
	return resp.StatusCode, nil
}

func taskReceiptForwarder(client *http.Client, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		suffix := strings.TrimPrefix(r.URL.Path, "/task-receipts")
		target := fmt.Sprintf("%s/internal/agents/%s/task-receipts%s", cfg.ControlPlaneURL, cfg.AgentName, suffix)
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, http.MaxBytesReader(w, r.Body, 4096))
		if err != nil {
			http.Error(w, "invalid receipt request", http.StatusBadRequest)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if token, err := readPodToken(podTokenPath); err == nil && token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "task receipt service unavailable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 8192))
	}
}

// classifyForwardError maps an HTTP status code (or 0 for "no response") to
// the small enum of reasons the eventForwardErrorsTotal counter labels.
// Kept in the file the metrics pipeline lives in so dashboards and code
// stay in lockstep.
func classifyForwardError(status int) string {
	switch {
	case status == 0:
		return "cp_unreachable"
	case status >= 500 && status < 600:
		return "cp_5xx"
	case status >= 400 && status < 500:
		return "cp_4xx"
	default:
		return "unknown"
	}
}

// recordEventMetrics best-effort parses the forwarded body to label the
// success counter and update the activity-state accumulator. A parse failure
// still records type=unknown rather than dropping the count.
func recordEventMetrics(ctx context.Context, metrics *Metrics, body []byte, at time.Time) {
	if metrics == nil {
		return
	}
	var env struct {
		Type  string `json:"type"`
		State string `json:"state"`
	}
	_ = json.Unmarshal(body, &env)
	metrics.RecordEventForwarded(ctx, env.Type)
	if env.Type == "activity" && env.State != "" {
		metrics.BumpActivityCounter(ctx, at, env.State)
	}
}

// config carries the env-derived runtime config for the sidecar.
type config struct {
	AgentName       string
	ControlPlaneURL string
	AgentDiskBytes  int64
	TaskResultsRoot string
	A2APeers        map[string]outboundA2APeer
	// OtelEndpoint is the OTLP HTTP endpoint the metrics SDK posts to
	// (kyber#256). Empty disables metrics — the rest of the sidecar
	// keeps working.
	OtelEndpoint string
	// Runtime is the agent's runtime type (e.g. "claude-code"), surfaced
	// as a metric label so dashboards can split by runtime. Empty when the
	// pod-spec injector didn't set it (older deployments / dev installs).
	Runtime string
	// CgroupMemoryEventsPath is the absolute path to the pod-level cgroup
	// memory.events file (recursive — see cgroup_oom.go) the sidecar
	// reads to detect kernel OOM kills
	// (kyber#285). Auto-derived at startup from /proc/self/cgroup — see
	// resolvePodCgroupEventsPath for the derivation logic + cgroup-v2
	// caveats. Override via KYBER_CGROUP_MEMORY_EVENTS_PATH for tests /
	// clusters where path derivation doesn't apply (cgroup-v1 or an
	// exotic CRI). Empty disables OOM detection (sidecar logs
	// once and continues).
	CgroupMemoryEventsPath string
	// cgroupResolutionErr captures any error from auto-deriving the path
	// at startup. Surfaced once via the main-loop logger so an operator
	// can see WHY OOM detection is dormant. Not relevant when the env
	// override is set.
	cgroupResolutionErr error
}

// loadConfig pulls + validates env vars. Mirrors kyber-token-reporter's
// convention for AGENT_NAME and KYBER_CONTROL_PLANE_INTERNAL_URL; the
// pod-spec injector in pkg/controllers/agent/status_sidecar.go sets both
// before the container starts.
func loadConfig() (config, error) {
	c := config{
		AgentName:              os.Getenv("AGENT_NAME"),
		ControlPlaneURL:        os.Getenv("KYBER_CONTROL_PLANE_INTERNAL_URL"),
		OtelEndpoint:           os.Getenv("KYBER_OTEL_ENDPOINT"),
		Runtime:                os.Getenv("KYBER_RUNTIME_TYPE"),
		CgroupMemoryEventsPath: os.Getenv("KYBER_CGROUP_MEMORY_EVENTS_PATH"),
		TaskResultsRoot:        os.Getenv("KYBER_TASK_RESULTS_ROOT"),
	}
	if c.AgentName == "" {
		return c, fmt.Errorf("AGENT_NAME unset")
	}
	if c.ControlPlaneURL == "" {
		return c, fmt.Errorf("KYBER_CONTROL_PLANE_INTERNAL_URL unset")
	}
	if c.TaskResultsRoot == "" {
		c.TaskResultsRoot = "/persist/task-results"
	}
	c.TaskResultsRoot = filepath.Clean(c.TaskResultsRoot)
	if c.TaskResultsRoot != "/persist" && !strings.HasPrefix(c.TaskResultsRoot, "/persist/") {
		return c, fmt.Errorf("KYBER_TASK_RESULTS_ROOT must be beneath /persist")
	}
	peers, err := loadOutboundA2APeers(os.Getenv("KYBER_A2A_PEERS_JSON"))
	if err != nil {
		return c, err
	}
	c.A2APeers = peers
	diskBytes, err := strconv.ParseInt(os.Getenv("KYBER_AGENT_DISK_BYTES"), 10, 64)
	if err != nil || diskBytes <= 0 {
		return c, fmt.Errorf("KYBER_AGENT_DISK_BYTES must be a positive integer")
	}
	c.AgentDiskBytes = diskBytes
	if c.CgroupMemoryEventsPath == "" {
		// Derive the pod-level cgroup memory.events path from
		// /proc/self/cgroup. On clusters where derivation fails (cgroup
		// is unavailable (cgroup-v1, exotic CRI), the path stays empty and
		// the OOM detector runs in disabled mode — the sidecar logs the
		// reason once and continues. Operators can set
		// KYBER_CGROUP_MEMORY_EVENTS_PATH to override.
		if path, err := resolvePodCgroupEventsPath("/proc/self/cgroup", "/sys/fs/cgroup"); err == nil {
			c.CgroupMemoryEventsPath = path
		} else {
			// Surfaced via the sidecar's main-loop logger after Init —
			// loadConfig is too early for slog.
			c.cgroupResolutionErr = err
		}
	}
	return c, nil
}

// runHeartbeats ticks every metricsSnapshotInterval (15s).
//
// On every tick: POST a metrics snapshot to /internal/agents/{name}/status
// with the accumulated activity-state seconds and any pending state transitions.
//
// On every other tick (30s): also POST a heartbeat-of-record to
// /internal/agents/{name}/status-event to update the CRD.
//
// Each tick also runs the cgroup OOM detector (kyber#285).
func runHeartbeats(ctx context.Context, cfg config, logger *slog.Logger, metrics *Metrics) {
	client := &http.Client{
		Timeout: postTimeout,
	}

	oom := newOOMDetector(cfg.CgroupMemoryEventsPath)
	// Seed from the pause marker so a restarted sidecar does not resume a
	// runtime the control plane still considers exhausted.
	resources := newResourceSampler(persistPath, cfg.CgroupMemoryEventsPath, cfg.AgentDiskBytes, logger.Warn,
		diskExhaustedMarkerPresent(diskExhaustedMarker))
	resourceReadErrLogged := false
	resourcePostErrLogged := false
	logfn := func(format string, args ...any) { logger.Info(fmt.Sprintf(format, args...)) }
	warnfn := func(format string, args ...any) { logger.Warn(fmt.Sprintf(format, args...)) }

	tickCount := 0

	tickOnce := func() {
		now := time.Now()
		// kyber#575 liveness watchdog: advance the tick BEFORE the POST so a
		// slow/failing control-plane call never makes the loop look stalled.
		lastLoopTick.Store(now.UnixNano())
		tickCount++

		// Every other tick is the heartbeat-of-record (CRD update).
		if tickCount%2 == 0 {
			status, err := postHeartbeat(ctx, client, cfg)
			if err != nil {
				logger.Warn("heartbeat post failed", "err", err)
				lastPostOK.Store(false)
				metrics.RecordEventForwardError(ctx, classifyForwardError(status))
			} else {
				lastPostOK.Store(true)
				metrics.MarkHeartbeat(now)
				metrics.RecordEventForwarded(ctx, "heartbeat")
			}
		} else {
			// Mark health OK if we could post last time, or on first tick.
			if tickCount == 1 {
				lastPostOK.Store(true)
			}
		}

		// Every tick: send metrics snapshot to control plane.
		postMetricsSnapshot(ctx, client, cfg, metrics, logger)

		// kyber#285: check for kernel OOM kills.
		if oom.tick(ctx, client, cfg, now, logfn, warnfn) {
			metrics.RecordEventForwarded(ctx, "memory_oom")
		}

		usage, err := resources.sample(now)
		if err != nil {
			if !resourceReadErrLogged {
				logger.Warn("resource usage sampling unavailable", "err", err)
				resourceReadErrLogged = true
			}
		} else {
			resourceReadErrLogged = false
			// A pending/failed directory walk must not clear a marker left by a
			// previously full volume. Only a completed disk sample can safely
			// change the maintenance signal.
			if err := syncDiskExhaustedMarkerForSample(diskExhaustedMarker, usage.DiskUsageState, usage.DiskReserveReached); err != nil {
				logger.Warn("disk maintenance marker update failed", "err", err)
			}
			metrics.RecordResourceUsage(ctx, usage)
			if err := postResourceUsage(ctx, client, cfg, now, usage); err != nil {
				if !resourcePostErrLogged {
					logger.Warn("resource usage post failed", "err", err)
					resourcePostErrLogged = true
				}
			} else {
				resourcePostErrLogged = false
				metrics.RecordEventForwarded(ctx, "resource_usage")
			}
		}
	}

	// Fire immediately so the control plane gets the first snapshot quickly.
	tickOnce()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tickOnce()
		}
	}
}

func postResourceUsage(ctx context.Context, client *http.Client, cfg config, now time.Time, usage resourceUsage) error {
	body, err := json.Marshal(map[string]any{
		"type":      "resource_usage",
		"at":        now.UTC().Format(time.RFC3339),
		"resources": usage,
	})
	if err != nil {
		return fmt.Errorf("marshal resource usage: %w", err)
	}
	if _, err := postToCP(ctx, client, cfg, "status-event", body); err != nil {
		return err
	}
	return nil
}

// postMetricsSnapshot POSTs accumulated activity-state seconds and pending
// state transitions to /internal/agents/{name}/status (kyber#343).
// Errors are logged and swallowed — metrics collection must never crash the loop.
func postMetricsSnapshot(ctx context.Context, client *http.Client, cfg config, m *Metrics, logger *slog.Logger) {
	if m == nil {
		return
	}
	stateSecs, transitions := m.DrainSnapshot()
	if len(stateSecs) == 0 && len(transitions) == 0 {
		// kyber#360: the silent early-return is exactly what made Cause C
		// invisible on the live cluster — every snapshot tick looked
		// identical to "code path never fires." Under debug, surface it.
		logger.Debug("metrics snapshot: empty, skipping")
		return
	}
	logger.Debug("metrics snapshot: posting",
		"stateSecs", len(stateSecs),
		"transitions", len(transitions),
	)

	type transition struct {
		From string `json:"from"`
		To   string `json:"to"`
		At   string `json:"at"`
	}
	type snapshot struct {
		ActivityStateSeconds map[string]float64 `json:"activity_state_seconds,omitempty"`
		StateTransitions     []transition       `json:"state_transitions,omitempty"`
		At                   string             `json:"at"`
	}
	snap := snapshot{
		ActivityStateSeconds: stateSecs,
		At:                   time.Now().UTC().Format(time.RFC3339),
	}
	for _, tr := range transitions {
		snap.StateTransitions = append(snap.StateTransitions, transition{
			From: tr.From,
			To:   tr.To,
			At:   tr.At.UTC().Format(time.RFC3339),
		})
	}
	body, err := json.Marshal(snap)
	if err != nil {
		logger.Warn("metrics snapshot marshal failed", "err", err)
		return
	}
	if _, err := postToCP(ctx, client, cfg, "status", body); err != nil {
		logger.Warn("metrics snapshot post failed", "err", err)
	}
}

// postHeartbeat sends a single heartbeat event to the control plane via
// the shared postToCP helper. Returns the HTTP status code (0 on transport
// failures) alongside the error so heartbeat failures can be classified
// into the same labeled buckets as forwarder failures.
func postHeartbeat(ctx context.Context, client *http.Client, cfg config) (int, error) {
	body, err := json.Marshal(map[string]string{
		"type": "heartbeat",
		"at":   time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return 0, fmt.Errorf("marshal: %w", err)
	}
	return postToCP(ctx, client, cfg, "status-event", body)
}

// readPodToken reads the pod-token Secret mounted at path. Trailing
// whitespace (typical of how k8s renders Secret data) is trimmed. Re-read
// on each post so a rotated token is picked up without a pod restart.
func readPodToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// runHealth serves /healthz on :8090. Returns 200 when the most recent
// heartbeat post succeeded; 503 otherwise. kubelet's LivenessProbe
// (set by the controller's pod injector) hits this and will restart
// the container if 503 persists.
// healthzHandler is the kubelet liveness endpoint. It reports OK while the
// heartbeat loop is still advancing (kyber#575): liveness reflects the sidecar
// PROCESS being alive, not whether the control plane is reachable. A transient
// control-plane outage (failing POSTs, loop still ticking) stays healthy;
// unhealthy is returned ONLY when the loop has stalled past
// livenessStaleThreshold (deadlock/hang). Extracted from runHealth so the
// liveness semantics are unit-testable.
func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	last := lastLoopTick.Load()
	if last != 0 && time.Since(time.Unix(0, last)) < livenessStaleThreshold {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("heartbeat loop stalled"))
}

func runHealth(ctx context.Context, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)
	srv := &http.Server{
		Addr:              healthAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	// Bind first, then log — same rationale as runForwarder (kyber#270).
	ln, err := net.Listen("tcp", healthAddr)
	if err != nil {
		logger.Error("healthz listen failed", "addr", healthAddr, "err", err)
		return
	}
	logger.Info("healthz listening", "addr", healthAddr)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("healthz server died", "err", err)
	}
}
