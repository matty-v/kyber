// Package agent_base_test — this file deliberately carries NO build tag, so it
// runs in the ordinary `go test ./...` suite. It needs neither docker nor a
// network: it is a pure text check on entrypoint.sh.
package agent_base_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The chroot boot runs inside `dbus-run-session -- bash -c '...'`, a
// SINGLE-QUOTED shell string. One unescaped apostrophe anywhere in that body
// silently terminates the string and the boot dies at "Entering chroot" with no
// diagnostic — every agent, every runtime.
//
// This is not hypothetical. It happened while fixing kyber#684: a comment
// reading "the keyring's own subtrees" plus a nested `su -c 'test -w ...'` broke
// all four agent-base docker tests, including two the change never touched.
// `bash -n` on entrypoint.sh does NOT catch it — the block is just a string to
// the outer parser — and the docker tests that do catch it need a docker daemon
// and ~40s per boot, so they are gated behind a build tag and did not run for
// weeks (the job is skipped unless the runtime-base paths change).
//
// The only correct way to embed an apostrophe in such a block is the
// '"'"' idiom, which closes, quotes, and reopens. That form is allowed here.
func TestEntrypoint_NoUnescapedQuotesInSingleQuotedBashBlocks(t *testing.T) {
	path := filepath.Join("entrypoint.sh")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(string(raw), "\n")

	// A block opens on a line ending in `bash -c '` and closes on the first
	// later line that is nothing but whitespace and a lone single quote.
	closer := regexp.MustCompile(`^\s*'$`)
	const escapeIdiom = `'"'"'`

	var blocks int
	for i, l := range lines {
		if !strings.HasSuffix(strings.TrimRight(l, " \t"), "bash -c '") {
			continue
		}
		blocks++
		end := -1
		for j := i + 1; j < len(lines); j++ {
			if closer.MatchString(lines[j]) {
				end = j
				break
			}
		}
		if end == -1 {
			t.Errorf("block opened at line %d has no closing quote line — entrypoint.sh is malformed", i+1)
			continue
		}
		for k, body := range lines[i+1 : end] {
			if strings.Contains(strings.ReplaceAll(body, escapeIdiom, ""), "'") {
				t.Errorf("entrypoint.sh:%d has an unescaped single quote inside a single-quoted `bash -c` block "+
					"— this ends the string early and kills the boot at \"Entering chroot\". "+
					"Use the '\"'\"' idiom or reword.\n    %s",
					i+2+k, strings.TrimSpace(body))
			}
		}
	}

	// Guard the guard: if the blocks stop being findable (refactor, reindent),
	// this test would pass vacuously and stop protecting anything.
	if blocks < 2 {
		t.Errorf("expected at least 2 single-quoted `bash -c` blocks in entrypoint.sh, found %d — "+
			"the detector has drifted from the file and is no longer checking anything", blocks)
	}
}
