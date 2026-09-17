package agent

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestTranscriptSidecarsUseRuntimeSpecificRootFSRoots(t *testing.T) {
	tests := []struct {
		name    string
		runtime string
		suffix  string
	}{
		{name: "Claude Code", runtime: "claude-code", suffix: ".claude/projects"},
		{name: "Codex", runtime: "codex", suffix: ".codex/sessions"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}

			AppendSessionSaver(spec, SessionSaverConfig{
				AgentName: "alice", RuntimeImage: "runtime:test", Runtime: tt.runtime,
			})
			AppendTranscriptTailer(spec, TranscriptTailerConfig{
				AgentName: "alice", RuntimeImage: "runtime:test", Runtime: tt.runtime,
			})
			AppendTranscriptPruner(spec, TranscriptPrunerConfig{
				AgentName: "alice", RuntimeImage: "runtime:test", Runtime: tt.runtime,
				Enabled: true, MaxAgeDays: 7,
			})

			assertContainerEnv(t, mustInitContainerByName(t, spec, SessionSaverContainerName),
				"SAVER_ROOTFS_ROOT", "/persist/agentroot/home/kyber/"+tt.suffix)
			assertContainerEnv(t, mustInitContainerByName(t, spec, TranscriptTailerContainerName),
				"TRANSCRIPT_ROOTFS_ROOT", "/agent-home/agentroot/home/kyber/"+tt.suffix)
			assertContainerEnv(t, mustInitContainerByName(t, spec, TranscriptPrunerContainerName),
				"PRUNE_ROOTFS_ROOT", "/agent-home/agentroot/home/kyber/"+tt.suffix)
		})
	}
}

func assertContainerEnv(t *testing.T, container corev1.Container, name, want string) {
	t.Helper()
	for _, env := range container.Env {
		if env.Name == name {
			if env.Value != want {
				t.Errorf("%s = %q; want %q", name, env.Value, want)
			}
			return
		}
	}
	t.Errorf("%s is not set", name)
}
