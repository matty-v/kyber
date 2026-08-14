package api

import (
	"context"
	"errors"
	"testing"

	k8sexec "k8s.io/client-go/util/exec"
)

// The compact-session handler decides what to tell the operator from the
// in-pod script's EXIT STATUS, not from message text. These cover that
// mapping, because getting it wrong turns a transient, retry-able condition
// into a 500 ("something is broken") or — worse — tells an operator to roll
// an agent onto a new image over a problem the image would not fix.

func TestExecExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"script refused: session locked", k8sexec.CodeExitError{Err: errors.New("x"), Code: 3}, 3},
		{"script refused: no tmux session", k8sexec.CodeExitError{Err: errors.New("x"), Code: 4}, 4},
		{"wrapper could not launch it", k8sexec.CodeExitError{Err: errors.New("x"), Code: 1}, 1},
		// Not an exit status at all — a broken stream or a cancelled request.
		// Must not be mistaken for exit code 0 or for any script code.
		{"stream failure", errors.New("error dialing backend"), -1},
		{"context cancelled", context.Canceled, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := execExitCode(tc.err); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestIsScriptExitCode: only the codes kyber-compact-session defines for
// itself count as "the script ran". This is the gate that stops a wrapper
// failure from being reported as a script refusal and vice versa — and, in
// particular, stops the "your image is too old" answer from firing for an
// agent whose script plainly executed.
func TestIsScriptExitCode(t *testing.T) {
	for _, code := range []int{2, 3, 4, 5} {
		if !isScriptExitCode(code) {
			t.Errorf("exit %d should be recognized as the script's own", code)
		}
	}
	// 1 is runuser failing to exec; 127 is a shell not finding it; -1 is no
	// exit status at all. None of them mean the script ran.
	for _, code := range []int{-1, 0, 1, 6, 126, 127} {
		if isScriptExitCode(code) {
			t.Errorf("exit %d must not be treated as the script's own", code)
		}
	}
}
