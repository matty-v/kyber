// Package hermes provides the Nous Research Hermes Agent runtime.
package hermes

import "github.com/matty-v/kyber/pkg/runtimes"

const Type = "hermes"

type runtime struct{}

func (*runtime) Type() string              { return Type }
func (*runtime) Adapter() runtimes.Adapter { return NewAdapter() }
func (*runtime) Probe() runtimes.Probe     { return probe{} }

type probe struct{}

func (probe) Type() string { return Type }

func init() { runtimes.Register(&runtime{}) }
