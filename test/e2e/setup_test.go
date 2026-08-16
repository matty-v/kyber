//go:build e2e

package e2e

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var (
	clusterName = flag.String("cluster-name", "kyber-e2e", "k3d cluster name")
	skipSetup   = flag.Bool("skip-setup", false, "assume cluster and kyber already installed")
	skipCleanup = flag.Bool("skip-cleanup", false, "don't delete cluster after tests")
	skipBuild   = flag.Bool("skip-build", false, "skip docker build + k3d image import steps")
	helmRelease = flag.String("helm-release", "kyber", "Helm release name")
	helmChart   = flag.String("helm-chart", "deploy/helm/kyber", "path to the Helm chart")
	valuesFile  = flag.String("values-file", "test/e2e/values-test.yaml", "Helm values override file")
	portForward = flag.Bool("port-forward", true, "start kubectl port-forward for the API")
	apiPort     = flag.Int("api-port", 18080, "local port for the port-forwarded API")
)

// portForwardCmd holds the running port-forward process so we can stop it on teardown.
var portForwardCmd *exec.Cmd

func TestMain(m *testing.M) {
	flag.Parse()

	// Change working directory to the repo root so that relative paths in
	// commands (images/, deploy/helm/, test/e2e/) resolve correctly regardless
	// of where `go test` was invoked.
	if err := cdToRepoRoot(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: cannot find repo root: %v\n", err)
		os.Exit(1)
	}

	if !*skipSetup {
		if err := setupCluster(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e setup failed: %v\n", err)
			os.Exit(1)
		}
	}

	// Start port-forward in background so tests can reach the API.
	if *portForward {
		pf, err := startPortForward()
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e port-forward failed: %v\n", err)
			os.Exit(1)
		}
		portForwardCmd = pf
	}

	// Wait for the API to respond.
	if err := waitForAPI(); err != nil {
		stopPortForward()
		fmt.Fprintf(os.Stderr, "e2e API not ready: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	stopPortForward()

	if !*skipCleanup {
		if err := teardownCluster(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e teardown failed (ignored): %v\n", err)
		}
	}

	os.Exit(code)
}

// cdToRepoRoot changes the working directory to the repository root.
// It navigates from the location of this source file (test/e2e/) up two levels.
func cdToRepoRoot() error {
	// runtime.Caller(0) returns the path to this source file at compile time.
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("runtime.Caller failed")
	}
	// filename is .../kyber/test/e2e/setup_test.go
	// Go up two directories: test/e2e -> test -> repo root
	repoRoot := filepath.Join(filepath.Dir(filename), "..", "..")
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return err
	}
	fmt.Printf("=== e2e: chdir to repo root: %s ===\n", abs)
	return os.Chdir(abs)
}

// setupCluster creates a k3d cluster, builds and imports the control-plane image,
// then installs Kyber via Helm.
func setupCluster() error {
	fmt.Printf("=== e2e: creating k3d cluster %q ===\n", *clusterName)
	if err := run("k3d", "cluster", "create", *clusterName, "--no-lb", "--wait"); err != nil {
		return fmt.Errorf("k3d cluster create: %w", err)
	}

	if !*skipBuild {
		fmt.Println("=== e2e: building control-plane image ===")
		if err := run("docker", "build",
			"-t", "kyber/control-plane:local",
			"-f", "images/control-plane/Dockerfile",
			".",
		); err != nil {
			return fmt.Errorf("docker build: %w", err)
		}

		fmt.Printf("=== e2e: importing image into k3d cluster %q ===\n", *clusterName)
		if err := run("k3d", "image", "import",
			"-c", *clusterName,
			"kyber/control-plane:local",
		); err != nil {
			return fmt.Errorf("k3d image import: %w", err)
		}
	}

	fmt.Println("=== e2e: installing Kyber via Helm ===")
	// Use --create-namespace so Helm creates the namespace if it doesn't exist.
	// Disable namespace.create in the chart to avoid a double-create conflict
	// (the chart's namespace.yaml template would otherwise also attempt to create it).
	if err := run("helm", "upgrade", "--install",
		*helmRelease, *helmChart,
		"-f", *valuesFile,
		"--namespace", "kyber-system",
		"--create-namespace",
		"--set", "namespace.create=false",
		"--wait",
		"--timeout", "3m",
	); err != nil {
		return fmt.Errorf("helm install: %w", err)
	}

	fmt.Println("=== e2e: cluster ready ===")
	return nil
}

// teardownCluster deletes the k3d cluster.
func teardownCluster() error {
	fmt.Printf("=== e2e: deleting k3d cluster %q ===\n", *clusterName)
	return run("k3d", "cluster", "delete", *clusterName)
}

// startPortForward starts `kubectl port-forward` for the Kyber API in the background.
// Returns the exec.Cmd so the caller can stop it later.
func startPortForward() (*exec.Cmd, error) {
	addr := fmt.Sprintf("%d:8080", *apiPort)
	cmd := exec.Command("kubectl", "port-forward",
		"-n", "kyber-system",
		"svc/kyber-control-plane",
		addr,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("kubectl port-forward: %w", err)
	}
	// Give the port-forward a moment to establish the tunnel.
	time.Sleep(2 * time.Second)
	return cmd, nil
}

// stopPortForward kills the port-forward process if running.
func stopPortForward() {
	if portForwardCmd != nil && portForwardCmd.Process != nil {
		_ = portForwardCmd.Process.Kill()
		_ = portForwardCmd.Wait()
	}
}

// waitForAPI polls /healthz until the server responds or a timeout elapses.
func waitForAPI() error {
	// Honour KYBER_E2E_BASE_URL when it is already set. The API client reads
	// it, so without this the readiness gate polls localhost while the tests
	// themselves would talk to a completely different cluster — the suite then
	// fails at startup against any pre-existing install (a dev cluster or a
	// canary), which is exactly where the sandbox suites need to run.
	baseURL := os.Getenv("KYBER_E2E_BASE_URL")
	if baseURL == "" {
		baseURL = fmt.Sprintf("http://localhost:%d", *apiPort)
	}
	url := baseURL + "/healthz"
	deadline := time.Now().Add(60 * time.Second)
	client := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				fmt.Printf("=== e2e: API ready at %s ===\n", baseURL)
				if os.Getenv("KYBER_E2E_BASE_URL") == "" {
					_ = os.Setenv("KYBER_E2E_BASE_URL", baseURL)
				}
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("API did not respond within 60s at %s", url)
}

// run executes an external command with its output forwarded to stdout/stderr.
func run(name string, args ...string) error {
	fmt.Printf("+ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runWithContext executes an external command with context cancellation.
func runWithContext(ctx context.Context, name string, args ...string) error {
	fmt.Printf("+ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
