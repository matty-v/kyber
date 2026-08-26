// Package main is the entrypoint for the Kyber node agent.
//
// The node agent is a thin DaemonSet binary that runs on each k8s node.
// Its responsibilities are:
//   - Export machine metrics (CPU, memory, disk, uptime) to the OTEL pipeline
//   - Execute machine-level actions (reboot, stop) when instructed by the control plane
//   - Report node health status
//
// CLI usage:
//
//	node-agent              — default mode, starts metrics loop
//	node-agent action reboot — execute a reboot action on this node
//	node-agent action stop   — execute a stop action on this node
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/matty-v/kyber/pkg/logging"
	"github.com/matty-v/kyber/pkg/nodeagent"
)

func main() {
	logger, err := logging.New(logging.Config{
		Component: "node-agent",
		Level:     os.Getenv("KYBER_LOG_LEVEL"),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	slog.SetDefault(logger)

	args := os.Args[1:]

	if len(args) > 0 && args[0] == "action" {
		// Action mode: one-shot, executes a machine-level action and exits.
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: node-agent action <reboot|stop>")
			os.Exit(1)
		}
		executor := nodeagent.NewActionExecutor()
		ctx := context.Background()
		switch args[1] {
		case "reboot":
			if err := executor.Reboot(ctx); err != nil {
				logger.Error("reboot failed", "error", err)
				os.Exit(1)
			}
		case "stop":
			if err := executor.Stop(ctx); err != nil {
				logger.Error("stop failed", "error", err)
				os.Exit(1)
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown action: %s\n", args[1])
			os.Exit(1)
		}
		return
	}

	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "node-agent: unknown arguments %v\n", args)
		os.Exit(1)
	}

	// Default mode: metrics loop. Runs until SIGTERM or SIGINT.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	collector := nodeagent.NewCollector("/proc")

	exporter, shutdownOTEL, err := nodeagent.NewOTELExporter(ctx)
	if err != nil {
		logger.Error("initializing OTEL", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx := context.Background()
		if err := shutdownOTEL(shutdownCtx); err != nil {
			logger.Warn("OTEL shutdown failed", "error", err)
		}
	}()

	// Preemption watcher — calls control plane when GCE signals preemption
	machineName := os.Getenv("KYBER_MACHINE_NAME")
	controlPlaneURL := os.Getenv("KYBER_CONTROL_PLANE_URL")
	if machineName != "" && controlPlaneURL != "" {
		var interruptionSource nodeagent.InterruptionSource = nodeagent.GCEInterruptionSource{}
		if os.Getenv("KYBER_CLOUD_PROVIDER") == "eks" {
			interruptionSource = nodeagent.EC2InterruptionSource{}
		}
		pw := &nodeagent.PreemptionWatcher{
			Source: interruptionSource,
			OnPreemption: func() {
				logger.Info("preemption detected; notifying control plane", "machine", machineName)
				notifyControlPlane(controlPlaneURL, machineName)
			},
		}
		go pw.Run(ctx)
	}

	reporter := nodeagent.NewResourceReporter()

	logger.Info("starting metrics loop", "interval", "30s", "machine", machineName)
	nodeagent.RunMetricsLoop(ctx, collector, exporter, 30*time.Second, reporter)
	logger.Info("shutting down")
}

func notifyControlPlane(baseURL, machineName string) {
	url := fmt.Sprintf("%s/internal/machines/%s/preemption-notice", baseURL, machineName)
	body := fmt.Sprintf(`{"timestamp":"%s","instanceId":"%s"}`,
		time.Now().UTC().Format(time.RFC3339), os.Getenv("HOSTNAME"))
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body)) //nolint:noctx
	if err != nil {
		slog.Error("building preemption notice request", "machine", machineName, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// Authenticate as the node-agent (kyber#566) so the internal API admits the
	// preemption notice. Empty during the migration before the token is mounted;
	// grace mode tolerates the missing Bearer until enforcement is on.
	if token := nodeagent.LoadPodToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("notifying control plane of preemption", "machine", machineName, "error", err)
		return
	}
	resp.Body.Close()
	slog.Info("control plane notified of preemption", "machine", machineName, "status_code", resp.StatusCode)
}
