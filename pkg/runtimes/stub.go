package runtimes

import (
	corev1 "k8s.io/api/core/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// stubAdapter is a minimal no-op Adapter used in tests where the real
// adapter is not needed. Use NewStubAdapter to construct one.
type stubAdapter struct {
	image           string
	entrypointArgs  []string
	envVars         []corev1.EnvVar
	secretMounts    []SecretMount
	livenessProbe   *corev1.Probe
	readinessProbe  *corev1.Probe
	gracefulSeconds int32
	briefPath       string
	statePath       string
	modelEnvVar     string
}

// NewStubAdapter creates an Adapter with configurable fields for unit
// tests. Test code that previously consumed agentcontroller.NewStubAdapter
// should switch to this; signature is identical.
func NewStubAdapter(
	image string,
	entrypointArgs []string,
	envVars []corev1.EnvVar,
	secretMounts []SecretMount,
	livenessProbe *corev1.Probe,
	readinessProbe *corev1.Probe,
	gracefulSeconds int32,
	briefPath string,
	statePath string,
	modelEnvVar string,
) Adapter {
	return &stubAdapter{
		image:           image,
		entrypointArgs:  entrypointArgs,
		envVars:         envVars,
		secretMounts:    secretMounts,
		livenessProbe:   livenessProbe,
		readinessProbe:  readinessProbe,
		gracefulSeconds: gracefulSeconds,
		briefPath:       briefPath,
		statePath:       statePath,
		modelEnvVar:     modelEnvVar,
	}
}

// CredentialSecretName returns "" — the stub has no credential Secret, so
// tests using it see the "runtime has no credential" path.
func (s *stubAdapter) CredentialSecretName(_ *kyberv1.Agent) string { return "" }

func (s *stubAdapter) Type() string                                { return "stub" }
func (s *stubAdapter) Image() string                               { return s.image }
func (s *stubAdapter) EntrypointArgs(_ *kyberv1.Agent) []string    { return s.entrypointArgs }
func (s *stubAdapter) EnvVars(_ *kyberv1.Agent) []corev1.EnvVar    { return s.envVars }
func (s *stubAdapter) SecretMounts(_ *kyberv1.Agent) []SecretMount { return s.secretMounts }
func (s *stubAdapter) LivenessProbe() *corev1.Probe                { return s.livenessProbe }
func (s *stubAdapter) ReadinessProbe() *corev1.Probe               { return s.readinessProbe }
func (s *stubAdapter) GracefulShutdownSeconds() int32              { return s.gracefulSeconds }
func (s *stubAdapter) SessionBriefPath() string                    { return s.briefPath }
func (s *stubAdapter) SessionStatePath() string                    { return s.statePath }
func (s *stubAdapter) ModelEnvVar() string                         { return s.modelEnvVar }
func (s *stubAdapter) RestartSessionCommand() []string             { return nil }
func (s *stubAdapter) CompactSessionCommand() []string             { return nil }
func (s *stubAdapter) PreStopCommand() []string                    { return nil }
