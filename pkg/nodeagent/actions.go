package nodeagent

import (
	"context"
	"fmt"
	"os/exec"
)

// ActionExecutor executes machine-level actions (reboot, stop).
// In dryRun mode it prints what would be executed without running the command —
// used for development and tests to avoid actually rebooting the machine.
type ActionExecutor struct {
	// shutdownCmd is the path to the shutdown binary (default: "/sbin/shutdown").
	shutdownCmd string
	// dryRun disables actual command execution. Set via NewDryRunActionExecutor.
	dryRun bool
}

// NewActionExecutor returns an ActionExecutor that runs real shutdown commands.
// The DaemonSet must run privileged for these to succeed.
func NewActionExecutor() *ActionExecutor {
	return &ActionExecutor{shutdownCmd: "/sbin/shutdown"}
}

// NewDryRunActionExecutor returns an ActionExecutor that logs actions without
// executing them. Used in dev and tests.
func NewDryRunActionExecutor() *ActionExecutor {
	return &ActionExecutor{shutdownCmd: "/sbin/shutdown", dryRun: true}
}

// Reboot initiates an immediate reboot of the host: `shutdown -r now`.
func (e *ActionExecutor) Reboot(ctx context.Context) error {
	return e.run(ctx, e.shutdownCmd, "-r", "now")
}

// Stop initiates an immediate halt of the host: `shutdown -h now`.
func (e *ActionExecutor) Stop(ctx context.Context) error {
	return e.run(ctx, e.shutdownCmd, "-h", "now")
}

// run executes or dry-runs the given command with args.
func (e *ActionExecutor) run(ctx context.Context, name string, args ...string) error {
	if e.dryRun {
		fmt.Printf("[dry-run] would execute: %s %v\n", name, args)
		return nil
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("executing %q %v: %w (output: %s)", name, args, err, string(out))
	}
	return nil
}
