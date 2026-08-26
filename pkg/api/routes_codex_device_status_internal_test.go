package api

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/codexauth"
)

// The probe runs under `sudo -iu kyber`, which prints the pod's debug banner
// before anything we asked for. Every case here carries that banner, because
// stripping it in the test would test a stdout the handler never receives.
const probeBanner = `[Kyber agent shell — debug helpers]

  agent          attach to the agent runtime's tmux session (read/write)
  restart-agent  kill the tmux session — pod will restart cleanly

`

func TestParseDeviceAuthProbe_ReadyWithExpiryAnchoredToSessionStart(t *testing.T) {
	start := time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)
	stdout := probeBanner +
		deviceAuthStartPrefix + "1787774400\n" +
		deviceAuthPaneMarker + "\n" +
		probeBanner + `Welcome to Codex [v0.146.0]

Follow these steps to sign in with ChatGPT using device code authorization:

1. Open this link in your browser and sign in to your account
   https://auth.openai.com/codex/device

2. Enter this one-time code (expires in 15 minutes)
   E7OV-KG840
`
	got := parseDeviceAuthProbe(stdout, start.Add(time.Minute))
	if got.State != codexauth.StateReady {
		t.Fatalf("state=%q, want ready", got.State)
	}
	if got.UserCode != "E7OV-KG840" {
		t.Errorf("userCode=%q", got.UserCode)
	}
	if want := start.Add(15 * time.Minute); !got.ExpiresAt.UTC().Equal(want) {
		t.Errorf("expiresAt=%v, want %v — the countdown must anchor to session_created, not to now",
			got.ExpiresAt.UTC(), want)
	}
}

// No login running. Distinct from `starting`: nothing is going to appear until
// somebody clicks, so the panel offers the button rather than a spinner.
func TestParseDeviceAuthProbe_NoSessionIsAbsent(t *testing.T) {
	got := parseDeviceAuthProbe(probeBanner+deviceAuthNoSession+"\n", time.Now())
	if got.State != codexauth.StateAbsent {
		t.Fatalf("state=%q, want absent", got.State)
	}
}

// The session exists but the prompt has not drawn yet — the first second or
// two of every login.
func TestParseDeviceAuthProbe_SessionWithoutPromptIsStarting(t *testing.T) {
	stdout := probeBanner + deviceAuthStartPrefix + "1787774400\n" + deviceAuthPaneMarker + "\n" + probeBanner
	if got := parseDeviceAuthProbe(stdout, time.Now()); got.State != codexauth.StateStarting {
		t.Fatalf("state=%q, want starting", got.State)
	}
}

// tmux can answer has-session and still fail to report session_created. The
// code is the useful half and is still served; the countdown is dropped rather
// than anchored to a number we do not have.
func TestParseDeviceAuthProbe_MissingStartStillServesTheCode(t *testing.T) {
	stdout := probeBanner + deviceAuthStartPrefix + "\n" + deviceAuthPaneMarker + `
1. Open this link
   https://auth.openai.com/codex/device
2. Enter this one-time code (expires in 15 minutes)
   E7OV-KG840
`
	got := parseDeviceAuthProbe(stdout, time.Now())
	if got.State != codexauth.StateReady {
		t.Fatalf("state=%q, want ready", got.State)
	}
	if !got.ExpiresAt.IsZero() {
		t.Errorf("expiresAt=%v, want zero when session_created is unreadable", got.ExpiresAt)
	}
}

// Pins the banner-stripping: the marker is what separates our echoes from the
// pane, so a banner containing something code-shaped must not leak through.
func TestParseDeviceAuthProbe_OnlyLooksAfterThePaneMarker(t *testing.T) {
	stdout := "SOME-BANNER https://auth.openai.com/x\n" +
		deviceAuthStartPrefix + "1787774400\n" +
		deviceAuthPaneMarker + "\nnothing useful here\n"
	if got := parseDeviceAuthProbe(stdout, time.Now()); got.State != codexauth.StateStarting {
		t.Fatalf("state=%q, want starting — text before the marker is not the pane", got.State)
	}
}

// The bug this file's mocks could not catch (kyber-canary, 2026-08-26).
//
// Every test above feeds parseDeviceAuthProbe a hand-written stdout, so the
// probe could be — and was — a command that never ran. `sudo -iu kyber` starts
// a LOGIN shell and re-parses the string it is handed, which mangled the
// script's `{ … }` group into
//
//	bash: -c: line 2: syntax error: unexpected end of file from `{' command on line 1
//
// on every single poll. The handler logged a warning, answered `starting`, and
// the panel spun forever over a code that was sitting in the pane the whole
// time.
//
// A mocked-stdout test cannot see that, and neither can `bash -n` on the script
// — the script is valid bash; sudo broke it in transit. What distinguishes the
// broken form from the working one is the argv, so that is what this pins.
func TestDeviceAuthProbeArgv_RunsAsAgentWithoutALoginShell(t *testing.T) {
	argv := deviceAuthProbeArgv()

	if argv[0] != "/usr/sbin/runuser" {
		t.Fatalf("argv[0]=%q, want /usr/sbin/runuser", argv[0])
	}
	for i, a := range argv {
		// -i / -l anywhere here means a login shell re-parsing the script.
		if a == "-i" || a == "-l" || a == "-iu" || a == "-lc" {
			t.Fatalf("argv[%d]=%q — a login shell re-parses the script and breaks its shell syntax", i, a)
		}
		if a == "sudo" {
			t.Fatalf("argv[%d]=sudo — use runuser, as every other run-as-the-agent path does", i)
		}
	}

	// tmux resolves its socket per-uid: as root it reports no session against a
	// perfectly healthy pod.
	joinedUser := false
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-u" && argv[i+1] == "kyber" {
			joinedUser = true
		}
	}
	if !joinedUser {
		t.Errorf("argv must become the kyber user; got %v", argv)
	}

	if argv[len(argv)-1] != deviceAuthProbeScript {
		t.Errorf("the script must be the final argument, passed whole; got %v", argv)
	}
}

// Cheap insurance for future edits to the script itself. It would not have
// caught the bug above, and says so, but a genuine syntax error introduced
// later is worth catching before a cluster does.
func TestDeviceAuthProbeScript_IsValidShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(deviceAuthProbeScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe script is not valid bash: %v\n%s", err, out)
	}
}
