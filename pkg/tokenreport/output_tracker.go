package tokenreport

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
)

// outputSeenCap bounds the tracker's message-id dedup set. FIFO eviction;
// 4096 ids is hours of traffic at any realistic message rate, and a dedup
// miss after eviction can only double-count a message that reappears far
// later — which does not happen in an append-only transcript.
const outputSeenCap = 4096

// outputTracker accumulates the output-token spend of finalized assistant
// messages across reporter ticks (kyber output-cost fix). The Claude Code
// transcript has no cumulative output counter (unlike Codex's
// total_token_usage), and reading only the newest finalized message loses
// every intermediate message written between ticks — so the reporter process
// maintains this running total itself and publishes it as
// Snapshot.Tokens.Output.
//
// Mechanics:
//   - Counting starts at the END of the first-observed session file
//     ("output since reporter start"): historical content is skipped so a
//     reporter restart cannot re-count spend an earlier incarnation already
//     reported. The restart gap itself is undercounted — acceptable and
//     honest (the control plane's safeDelta treats the post-restart smaller
//     total as a fresh increment).
//   - Each tick reads ONLY newly-appended complete lines via a per-file byte
//     offset, so messages that scroll out of ParseLatest's 50-line tail
//     window between ticks are still counted. A trailing line without '\n'
//     is left for the next tick (never mis-parse a mid-write half-line).
//   - When the newest session file rotates (Claude Code rotates on /clear),
//     the previous file's tail is drained first so messages written between
//     the last read and the rotation aren't lost. Files first seen after
//     start are read from offset 0 (they began life empty).
//   - Lines are deduped by message.id → uuid → leafUuid: multi-block
//     messages repeat the same message id and usage on several lines.
//
// Not safe for concurrent use; owned by a single Reporter goroutine.
type outputTracker struct {
	total       int64
	offsets     map[string]int64 // path → consumed byte offset
	current     string           // last path passed to advance
	initialized bool

	seen      map[string]struct{}
	seenOrder []string // FIFO eviction order for seen
}

func newOutputTracker() *outputTracker {
	return &outputTracker{
		offsets: map[string]int64{},
		seen:    map[string]struct{}{},
	}
}

// advance accounts newly-appended finalized assistant output in path (the
// current newest session file) and returns the running total. On the first
// call it only records the file's current end — counting starts with lines
// appended after reporter start.
func (t *outputTracker) advance(path string) int64 {
	if !t.initialized {
		t.offsets[path] = initialOffset(path)
		t.current = path
		t.initialized = true
		return t.total
	}
	if path != t.current {
		// Session file rotated: drain the previous file's tail first so
		// messages written between our last read and the rotation count.
		if t.current != "" {
			t.consume(t.current)
		}
		t.current = path
	}
	t.consume(path)
	return t.total
}

// consume reads complete lines from the stored offset of path to EOF,
// accounting each, and advances the offset. Errors are swallowed (transient
// read problems retry on the next tick; the reporter must never crash).
func (t *outputTracker) consume(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	off := t.offsets[path]
	if info, serr := f.Stat(); serr == nil && info.Size() < off {
		off = 0 // file replaced/truncated — re-read from the start
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, rerr := r.ReadBytes('\n')
		if rerr != nil {
			// EOF with a partial (un-terminated) line: leave it for the next
			// tick so a concurrent append is never parsed half-written.
			break
		}
		off += int64(len(line))
		t.account(line)
	}
	t.offsets[path] = off
}

// account parses one transcript line and adds its output tokens to the total
// when it is a newly-seen finalized assistant message.
func (t *outputTracker) account(line []byte) {
	var e rawEntry
	if json.Unmarshal(line, &e) != nil || e.Type != "assistant" {
		return
	}
	// Same finalized signal ParseLatest uses: streaming entries carry "?".
	if speed := e.Message.Usage.Speed; speed == "?" || speed == "" {
		return
	}
	// Guard against absent or malformed counts; never accumulate negatives.
	if e.Message.Usage.OutputTokens <= 0 {
		return
	}
	if id := dedupID(e); id != "" {
		if _, dup := t.seen[id]; dup {
			return
		}
		t.remember(id)
	}
	t.total += e.Message.Usage.OutputTokens
}

// dedupID returns the strongest available identity for a transcript line:
// message.id (stable across a message's multiple content-block lines) →
// line uuid → leafUuid. Empty means "no identity" — the line is counted
// without dedup.
func dedupID(e rawEntry) string {
	if e.Message.ID != "" {
		return e.Message.ID
	}
	if e.UUID != "" {
		return e.UUID
	}
	return e.LeafUUID
}

// remember inserts id into the bounded seen-set, evicting FIFO at capacity.
func (t *outputTracker) remember(id string) {
	if len(t.seenOrder) >= outputSeenCap {
		oldest := t.seenOrder[0]
		t.seenOrder = t.seenOrder[1:]
		delete(t.seen, oldest)
	}
	t.seen[id] = struct{}{}
	t.seenOrder = append(t.seenOrder, id)
}

// initialOffset returns the offset just past the last complete ('\n'-
// terminated) line of path, so the tracker starts cleanly on a line boundary
// even when the file is mid-append. 0 on any error (the file will simply be
// read from the start, which for an unreadable-then-created file is correct).
func initialOffset(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var off, lineEnd int64
	buf := make([]byte, 64*1024)
	for {
		n, rerr := f.Read(buf)
		for i := 0; i < n; i++ {
			if buf[i] == '\n' {
				lineEnd = off + int64(i) + 1
			}
		}
		off += int64(n)
		if rerr != nil {
			break
		}
	}
	return lineEnd
}
