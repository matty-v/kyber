// Package claudecode is the Claude Code runtime implementation. It
// self-registers with pkg/runtimes via init() so binaries that blank-
// import this package gain Claude Code support automatically:
//
//	import _ "github.com/matty-v/kyber/pkg/runtimes/claudecode"
//
// Currently provides only Adapter (pod-spec assembly). Probe (sidecar-side
// activity detection) lands with kyber#249.
package claudecode

import (
	"github.com/matty-v/kyber/pkg/runtimes"
)

// runtime wraps the Claude Code Adapter into a runtimes.Runtime so it can
// be registered. Probe is a no-op until kyber#249 ships the JSONL-tail
// activity detector.
type runtime struct{}

func (r *runtime) Type() string              { return "claude-code" }
func (r *runtime) Adapter() runtimes.Adapter { return NewClaudeCodeAdapter() }
func (r *runtime) Probe() runtimes.Probe     { return &probeStub{} }

// probeStub is the no-op Probe. Claude Code activity detection ships
// via the in-pod kyber-token-reporter binary (kyber#249) which reads
// the JSONL transcript inside the agent's chroot — out of reach for
// the platform sidecar. Probe is reserved for future cross-runtime
// signals that don't need filesystem access; see
// docs/architecture/status-pipeline.md.
type probeStub struct{}

func (p *probeStub) Type() string { return "claude-code" }

func init() {
	runtimes.Register(&runtime{})
}
