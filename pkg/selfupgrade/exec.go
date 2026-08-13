package selfupgrade

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
)

// ExecCommander runs real commands. The Job's log is the upgrade record, so
// everything a command prints goes there verbatim rather than being summarised.
type ExecCommander struct {
	// Stdout and Stderr receive streamed output. Nil discards, which is only
	// useful in tests — the Job wires both to the process's own streams.
	Stdout io.Writer
	Stderr io.Writer
	Log    *slog.Logger
}

func (e *ExecCommander) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

func (e *ExecCommander) stdout() io.Writer {
	if e.Stdout != nil {
		return e.Stdout
	}
	return io.Discard
}

func (e *ExecCommander) stderr() io.Writer {
	if e.Stderr != nil {
		return e.Stderr
	}
	return io.Discard
}

// Stream runs the command with its output forwarded live.
func (e *ExecCommander) Stream(ctx context.Context, name string, args ...string) error {
	e.log().Info("running", "command", name, "args", args)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = e.stdout()
	cmd.Stderr = e.stderr()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// Output runs the command and returns its stdout.
//
// stderr is captured rather than streamed and folded into the error: helm
// writes its diagnostics there, and an error that says only "exit status 1"
// forces whoever is reading the Job log to guess.
func (e *ExecCommander) Output(ctx context.Context, name string, args ...string) (string, error) {
	e.log().Info("running", "command", name, "args", args)
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errBuf.String())
		if detail == "" {
			detail = "no stderr output"
		}
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, detail)
	}
	return out.String(), nil
}
