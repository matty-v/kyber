package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// --- spec tests: AppendSessionSaver container shape ---

func TestAppendSessionSaver_NoOpWithoutImage(t *testing.T) {
	spec := &corev1.PodSpec{}
	AppendSessionSaver(spec, SessionSaverConfig{AgentName: "dave", RuntimeImage: ""})
	if len(spec.InitContainers) != 0 {
		t.Fatalf("empty RuntimeImage should be a no-op; got %d init containers", len(spec.InitContainers))
	}
}

func TestAppendSessionSaver_AppendsNativeSidecar(t *testing.T) {
	spec := &corev1.PodSpec{
		Volumes: []corev1.Volume{{Name: "persist"}},
	}
	volsBefore := len(spec.Volumes)

	AppendSessionSaver(spec, SessionSaverConfig{AgentName: "dave", RuntimeImage: "runtime:tag"})

	if len(spec.InitContainers) != 1 {
		t.Fatalf("want 1 init container, got %d", len(spec.InitContainers))
	}
	c := spec.InitContainers[0]
	if c.Name != SessionSaverContainerName {
		t.Errorf("container name = %q, want %q", c.Name, SessionSaverContainerName)
	}
	if c.Image != "runtime:tag" {
		t.Errorf("image = %q, want the runtime image %q", c.Image, "runtime:tag")
	}
	// Native sidecar so teardown runs after the agent exits (final-turn flush).
	if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("RestartPolicy = %v, want Always (native sidecar)", c.RestartPolicy)
	}
	// Root uid to read the root-owned JSONL (kyber#451 lesson).
	if c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0 {
		t.Errorf("RunAsUser = %v, want 0", c.SecurityContext)
	}
	// The whole point: a READ-WRITE persist mount at /persist (unlike the tailer's
	// read-only mount) so the saver can write session-state.json.
	var pm *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == "persist" {
			pm = &c.VolumeMounts[i]
		}
	}
	if pm == nil {
		t.Fatalf("no persist volume mount on the saver container")
	}
	if pm.MountPath != saverMountPath {
		t.Errorf("persist mountPath = %q, want %q", pm.MountPath, saverMountPath)
	}
	if pm.ReadOnly {
		t.Errorf("persist mount is ReadOnly; the saver must mount it read-write to write session-state.json")
	}
	// Reuses the existing persist PVC — no new volume added.
	if len(spec.Volumes) != volsBefore {
		t.Errorf("saver added %d volume(s); it should reuse the existing persist PVC and add none",
			len(spec.Volumes)-volsBefore)
	}
}

// --- behavior tests: execute sessionSaverScript over a fixture ---

// sessionState mirrors the JSON the saver writes to session-state.json.
type sessionState struct {
	Version         int    `json:"version"`
	AgentName       string `json:"agent_name"`
	UpdatedAt       string `json:"updated_at"`
	LastActivity    string `json:"last_activity"`
	RecentExchanges []struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		Timestamp string `json:"timestamp"`
	} `json:"recent_exchanges"`
}

func requireSaverTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("session-saver script is a Linux sidecar; skipping on %s", runtime.GOOS)
	}
	for _, tool := range []string{"bash", "jq", "find", "sort", "cut", "mktemp", "cmp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("required tool %q not on PATH; skipping executable script test", tool)
		}
	}
}

// runSaver writes fixtureLines as a single session file, runs the embedded script
// for one bounded poll, and returns the parsed session-state.json. extraEnv is
// appended last so a test can override a default (e.g. SAVER_MAX_BYTES).
func runSaver(t *testing.T, turns string, fixture string, extraEnv ...string) sessionState {
	t.Helper()
	requireSaverTools(t)
	dir := t.TempDir()
	overlay := filepath.Join(dir, "projects")
	if err := os.MkdirAll(overlay, 0o755); err != nil {
		t.Fatalf("mkdir overlay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(overlay, "sess.jsonl"), []byte(fixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	out := filepath.Join(dir, "session-state.json")

	cmd := exec.Command("bash", "-c", sessionSaverScript)
	cmd.Env = append(os.Environ(),
		"AGENT_NAME=k-2so",
		"SAVER_OVERLAY_ROOT="+overlay,
		"SAVER_BIND_ROOT="+filepath.Join(dir, "nonexistent-bind"),
		"SAVER_OUT="+out,
		"SAVER_TURNS="+turns,
		"SAVER_POLL_SECONDS=0",
		"SAVER_POLL_LIMIT=1",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("saver script failed: %v\noutput: %s", err, string(b))
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read session-state.json: %v", err)
	}
	var st sessionState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("session-state.json is not valid JSON: %v\ncontent: %s", err, string(data))
	}
	return st
}

// A realistic Claude Code transcript: a user string turn, an assistant with a
// thinking + text block, a tool_use-only assistant, a tool_result-only user, a
// SIDECHAIN (subagent) text turn, a final assistant text, and a half-written
// trailing line. Only the main-thread text turns should survive.
const saverFixture = `{"type":"user","isSidechain":false,"timestamp":"2026-07-06T18:00:00Z","message":{"role":"user","content":"hello there"}}
{"type":"assistant","isSidechain":false,"timestamp":"2026-07-06T18:00:01Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Hi! First line.\nSecond line."}]}}
{"type":"assistant","isSidechain":false,"timestamp":"2026-07-06T18:00:02Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{}}]}}
{"type":"user","isSidechain":false,"timestamp":"2026-07-06T18:00:03Z","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}
{"type":"assistant","isSidechain":true,"timestamp":"2026-07-06T18:00:04Z","message":{"role":"assistant","content":[{"type":"text","text":"SUBAGENT text must be skipped"}]}}
{"type":"assistant","isSidechain":false,"timestamp":"2026-07-06T18:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"Final assistant answer."}]}}
{"type":"user","isSidechain":false,"timestamp":"2026-07-06T18:00:0`

func TestSessionSaverScript_ExtractsRecallSnapshot(t *testing.T) {
	st := runSaver(t, "12", saverFixture)

	if st.Version != 1 {
		t.Errorf("version = %d, want 1", st.Version)
	}
	if st.AgentName != "k-2so" {
		t.Errorf("agent_name = %q, want k-2so", st.AgentName)
	}
	// last_activity is the first line of the most recent assistant text.
	if st.LastActivity != "Final assistant answer." {
		t.Errorf("last_activity = %q, want %q", st.LastActivity, "Final assistant answer.")
	}
	if st.UpdatedAt != "2026-07-06T18:00:05Z" {
		t.Errorf("updated_at = %q, want the last exchange's timestamp", st.UpdatedAt)
	}
	// Exactly the three main-thread text turns, in order — tool-only, sidechain,
	// and the torn trailing line all excluded.
	wantContent := []string{"hello there", "Hi! First line.\nSecond line.", "Final assistant answer."}
	if len(st.RecentExchanges) != len(wantContent) {
		t.Fatalf("got %d exchanges, want %d: %+v", len(st.RecentExchanges), len(wantContent), st.RecentExchanges)
	}
	for i, want := range wantContent {
		if st.RecentExchanges[i].Content != want {
			t.Errorf("exchange[%d].content = %q, want %q", i, st.RecentExchanges[i].Content, want)
		}
	}
	if st.RecentExchanges[0].Role != "user" || st.RecentExchanges[2].Role != "assistant" {
		t.Errorf("roles not preserved: %+v", st.RecentExchanges)
	}
}

const codexSaverFixture = `{"timestamp":"2026-08-06T14:00:00Z","type":"session_meta","payload":{"id":"session-id"}}
{"timestamp":"2026-08-06T14:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"remember the blue launch plan"}}
{"timestamp":"2026-08-06T14:00:02Z","type":"response_item","payload":{"type":"reasoning","summary":[]}}
{"timestamp":"2026-08-06T14:00:03Z","type":"event_msg","payload":{"type":"agent_message","message":"The blue launch plan is saved.\nNext step: verify it."}}
{"timestamp":"2026-08-06T14:00:04Z","type":"event_msg","payload":{"type":"token_count","info":{}}}
`

func TestSessionSaverScript_ExtractsCodexRecallSnapshot(t *testing.T) {
	st := runSaver(t, "12", codexSaverFixture)
	if len(st.RecentExchanges) != 2 {
		t.Fatalf("got %d Codex exchanges, want 2: %+v", len(st.RecentExchanges), st.RecentExchanges)
	}
	if st.RecentExchanges[0].Role != "user" || st.RecentExchanges[0].Content != "remember the blue launch plan" {
		t.Errorf("user exchange = %+v", st.RecentExchanges[0])
	}
	if st.RecentExchanges[1].Role != "assistant" || st.RecentExchanges[1].Content != "The blue launch plan is saved.\nNext step: verify it." {
		t.Errorf("assistant exchange = %+v", st.RecentExchanges[1])
	}
	if st.LastActivity != "The blue launch plan is saved." {
		t.Errorf("last_activity = %q", st.LastActivity)
	}
	if st.UpdatedAt != "2026-08-06T14:00:03Z" {
		t.Errorf("updated_at = %q", st.UpdatedAt)
	}
}

func TestSessionSaverScript_TurnsWindow(t *testing.T) {
	st := runSaver(t, "2", saverFixture)
	if len(st.RecentExchanges) != 2 {
		t.Fatalf("SAVER_TURNS=2 kept %d exchanges, want the last 2", len(st.RecentExchanges))
	}
	// The last two of the three surviving turns.
	if st.RecentExchanges[0].Content != "Hi! First line.\nSecond line." ||
		st.RecentExchanges[1].Content != "Final assistant answer." {
		t.Errorf("turns window kept the wrong tail: %+v", st.RecentExchanges)
	}
	// last_activity still reflects the most recent assistant turn.
	if st.LastActivity != "Final assistant answer." {
		t.Errorf("last_activity = %q, want %q", st.LastActivity, "Final assistant answer.")
	}
}

func TestSessionSaverScript_EmptyWhenNoTextTurns(t *testing.T) {
	// A transcript with only tool/sidechain noise yields a valid, empty snapshot.
	fixture := `{"type":"assistant","isSidechain":false,"timestamp":"2026-07-06T18:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{}}]}}
{"type":"assistant","isSidechain":true,"timestamp":"2026-07-06T18:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"subagent"}]}}
`
	st := runSaver(t, "12", fixture)
	if len(st.RecentExchanges) != 0 {
		t.Errorf("want 0 exchanges from tool/sidechain-only transcript, got %+v", st.RecentExchanges)
	}
	if st.LastActivity != "" {
		t.Errorf("last_activity = %q, want empty", st.LastActivity)
	}
	if st.Version != 1 || st.AgentName != "k-2so" {
		t.Errorf("snapshot metadata missing on empty transcript: %+v", st)
	}
}

// TestSessionSaverScript_ByteBoundDropsOldContent verifies the SAVER_MAX_BYTES
// tail bound: only the last N bytes of the (potentially huge) newest file are
// parsed, so an old turn beyond the window is dropped and the partial leading
// line the byte cut produces is tolerated — while the recent turn still extracts.
func TestSessionSaverScript_ByteBoundDropsOldContent(t *testing.T) {
	// A big old assistant turn (long content) followed by a small recent one.
	big := strings.Repeat("x", 4000)
	fixture := `{"type":"assistant","isSidechain":false,"timestamp":"2026-07-06T17:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"` + big + `"}]}}
{"type":"assistant","isSidechain":false,"timestamp":"2026-07-06T18:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"RECENT-TURN"}]}}
`
	// Window smaller than the big line so tail -c cuts it; the recent line survives.
	st := runSaver(t, "12", fixture, "SAVER_MAX_BYTES=500")
	if len(st.RecentExchanges) != 1 {
		t.Fatalf("byte bound: want exactly the 1 recent turn, got %d: %+v", len(st.RecentExchanges), st.RecentExchanges)
	}
	if st.RecentExchanges[0].Content != "RECENT-TURN" {
		t.Errorf("byte bound kept the wrong turn: %q", st.RecentExchanges[0].Content)
	}
	if st.LastActivity != "RECENT-TURN" {
		t.Errorf("last_activity = %q, want RECENT-TURN", st.LastActivity)
	}
}
