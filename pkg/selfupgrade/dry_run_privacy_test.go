package selfupgrade

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

type privateRenderCommander struct {
	output   string
	failure  error
	streamed bool
	args     []string
}

func (c *privateRenderCommander) Output(_ context.Context, _ string, args ...string) (string, error) {
	c.args = append([]string{}, args...)
	return c.output, c.failure
}
func (c *privateRenderCommander) Stream(_ context.Context, _ string, _ ...string) error {
	c.streamed = true
	return errors.New("rendered output would have been streamed")
}

func TestDryRunNeverPublishesRenderedManifestsOrDiagnostics(t *testing.T) {
	const private = "private-credential-canary"
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			cmd := &privateRenderCommander{output: "kind: ConfigMap\ndata:\n  caller-key: " + private}
			if fail {
				cmd.failure = errors.New("render error near " + private)
			}
			var logs bytes.Buffer
			r := &Runner{Cfg: Config{Release: "kyber", Namespace: "kyber-system", HelmBin: "helm", DryRun: true}, Cmd: cmd, Log: slog.New(slog.NewTextHandler(&logs, nil))}
			err := r.upgrade(context.Background(), "/chart", "/private-values.yaml")
			if (err != nil) != fail {
				t.Fatal("incorrect dry-run result")
			}
			if cmd.streamed || strings.Contains(logs.String(), private) || err != nil && strings.Contains(err.Error(), private) {
				t.Fatal("rendered material escaped")
			}
			if !strings.Contains(strings.Join(cmd.args, " "), "--dry-run --hide-secret") {
				t.Fatal("Secret suppression missing")
			}
		})
	}
}

func TestDryRunKeepsSafeCancellationCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &Runner{Cfg: Config{DryRun: true}, Cmd: &privateRenderCommander{failure: errors.New("private render diagnostic")}}
	if err := r.upgrade(ctx, "/chart", "/values"); !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "private render diagnostic") {
		t.Fatal("cancellation lost or diagnostics leaked", err)
	}
}
