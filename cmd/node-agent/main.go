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
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/matty-v/kyber/pkg/nodeagent"
)

func main() {
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
				log.Fatalf("reboot failed: %v", err)
			}
		case "stop":
			if err := executor.Stop(ctx); err != nil {
				log.Fatalf("stop failed: %v", err)
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
		log.Fatalf("initializing OTEL: %v", err)
	}
	defer func() {
		shutdownCtx := context.Background()
		if err := shutdownOTEL(shutdownCtx); err != nil {
			log.Printf("OTEL shutdown error: %v", err)
		}
	}()

	// Preemption watcher — calls control plane when GCE signals preemption
	machineName := os.Getenv("KYBER_MACHINE_NAME")
	controlPlaneURL := os.Getenv("KYBER_CONTROL_PLANE_URL")
	if machineName != "" && controlPlaneURL != "" {
		pw := &nodeagent.PreemptionWatcher{
			OnPreemption: func() {
				log.Println("preemption detected, notifying control plane")
				notifyControlPlane(controlPlaneURL, machineName)
			},
		}
		go pw.Run(ctx)
	}

	reporter := nodeagent.NewResourceReporter()

	log.Printf("node-agent: starting metrics loop (interval=30s)")
	nodeagent.RunMetricsLoop(ctx, collector, exporter, 30*time.Second, reporter)
	log.Printf("node-agent: shutting down")
}

func notifyControlPlane(baseURL, machineName string) {
	url := fmt.Sprintf("%s/internal/machines/%s/preemption-notice", baseURL, machineName)
	body := fmt.Sprintf(`{"timestamp":"%s","instanceId":"%s"}`,
		time.Now().UTC().Format(time.RFC3339), os.Getenv("HOSTNAME"))
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body)) //nolint:noctx
	if err != nil {
		log.Printf("failed to build preemption-notice request: %v", err)
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
		log.Printf("failed to notify control plane: %v", err)
		return
	}
	resp.Body.Close()
	log.Printf("control plane notified: %d", resp.StatusCode)
}
