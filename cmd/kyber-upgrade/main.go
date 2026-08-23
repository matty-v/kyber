// Command kyber-upgrade upgrades this Kyber install to a target chart version.
//
// It is the entrypoint of the upgrade Job the control plane creates — the same
// control-plane image, a different binary, exactly like transcript-compact.
// Running in a Job rather than in the control-plane process is the whole point:
// the control plane is the thing being replaced, and a process cannot supervise
// its own termination. The Job survives the rollout, and its log is the record
// of what happened.
//
// It refuses more than it acts. It will not run where Kyber is not already a
// Helm release, it will not proceed if the target chart's CRDs fail to apply,
// and it will not call an upgrade successful until the running Deployment says
// it is on the new image and /healthz answers. Any failure after the release is
// written rolls back to the revision that was live when it started.
//
// Configuration comes from the environment (the Job template sets it); flags
// override for running it by hand against a cluster. See dave-agent spec
// 2026-08-10-kyber-owns-its-deployment.md §4.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matty-v/kyber/pkg/logging"
	"github.com/matty-v/kyber/pkg/selfupgrade"
)

const (
	defaultHelmBin = "/usr/local/bin/helm"
	defaultWorkDir = "/tmp/kyber-upgrade"
)

func main() {
	var (
		release    = flag.String("release", env("KYBER_UPGRADE_RELEASE", ""), "Helm release name to upgrade")
		namespace  = flag.String("namespace", env("KYBER_UPGRADE_NAMESPACE", ""), "namespace holding the release")
		chartRef   = flag.String("chart", env("KYBER_UPGRADE_CHART_REF", ""), "OCI chart repository, e.g. oci://ghcr.io/matty-v/charts/kyber")
		target     = flag.String("target-version", env("KYBER_UPGRADE_TARGET_VERSION", ""), "exact chart version to upgrade to")
		deployment = flag.String("control-plane-deployment", env("KYBER_UPGRADE_CONTROL_PLANE_DEPLOYMENT", ""), "control-plane Deployment to verify after the upgrade")
		healthURL  = flag.String("health-url", env("KYBER_UPGRADE_HEALTH_URL", ""), "in-cluster control-plane health endpoint")
		helmBin    = flag.String("helm", env("KYBER_UPGRADE_HELM_BIN", defaultHelmBin), "path to the helm binary")
		workDir    = flag.String("work-dir", env("KYBER_UPGRADE_WORKDIR", defaultWorkDir), "scratch directory for the pulled chart and captured values")
		dryRun     = flag.Bool("dry-run", envBool("KYBER_UPGRADE_DRY_RUN", false), "run every precondition and render the upgrade without changing anything")
	)
	flag.Parse()

	logger, err := logging.New(logging.Config{
		Component: "self-upgrade",
		Level:     os.Getenv("KYBER_LOG_LEVEL"),
		Writer:    os.Stdout,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	slog.SetDefault(logger)

	cfg := selfupgrade.Config{
		Release:        *release,
		Namespace:      *namespace,
		ChartRef:       *chartRef,
		TargetVersion:  *target,
		HelmBin:        *helmBin,
		WorkDir:        *workDir,
		UpgradeTimeout: envDuration("KYBER_UPGRADE_TIMEOUT_SECONDS", selfupgrade.DefaultUpgradeTimeout),
		VerifyTimeout:  envDuration("KYBER_UPGRADE_VERIFY_TIMEOUT_SECONDS", selfupgrade.DefaultVerifyTimeout),
		DryRun:         *dryRun,
	}

	if err := run(cfg, *deployment, *healthURL, logger); err != nil {
		logger.Error("upgrade failed", "error", err.Error())
		// Exit 1 so the Job records a failure. The rollback (if one was
		// needed) has already run by this point — Runner.Run does not return
		// until the cluster is back on the previous revision or a human is
		// needed.
		os.Exit(1)
	}
}

func run(cfg selfupgrade.Config, deployment, healthURL string, logger *slog.Logger) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if deployment == "" {
		return fmt.Errorf("control-plane deployment name is required: without it there is nothing to verify against, and an unverified upgrade is not one we report as successful")
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		return fmt.Errorf("create work directory %s: %w", cfg.WorkDir, err)
	}

	// SIGTERM cancels the run. It does NOT cancel a rollback already in
	// flight — Runner.rollback detaches from this context precisely so a Job
	// eviction cannot strand the release half-upgraded.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes config: %w", err)
	}
	k8s, err := client.New(restCfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		return fmt.Errorf("build Kubernetes client: %w", err)
	}

	runner := &selfupgrade.Runner{
		Cfg: cfg,
		Cmd: &selfupgrade.ExecCommander{Stdout: os.Stdout, Stderr: os.Stderr, Log: logger},
		K8s: k8s,
		Ver: &selfupgrade.DeploymentVerifier{
			Client:     k8s,
			Namespace:  cfg.Namespace,
			Deployment: deployment,
			HealthURL:  healthURL,
			Log:        logger,
		},
		Log: logger,
	}
	return runner.Run(ctx)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}

// envDuration reads a seconds count. An unparseable or non-positive value
// falls back rather than failing: a typo in a timeout must not stop an upgrade
// an operator asked for, and the default is always a safe value.
func envDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return fallback
	}
	return time.Duration(secs) * time.Second
}
