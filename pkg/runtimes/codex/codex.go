// Package codex provides the OpenAI Codex CLI runtime.
package codex

import "github.com/matty-v/kyber/pkg/runtimes"

type runtime struct{}

func (r *runtime) Type() string              { return "codex" }
func (r *runtime) Adapter() runtimes.Adapter { return NewAdapter() }
func (r *runtime) Probe() runtimes.Probe     { return &probe{} }

type probe struct{}

func (p *probe) Type() string { return "codex" }

func init() { runtimes.Register(&runtime{}) }
