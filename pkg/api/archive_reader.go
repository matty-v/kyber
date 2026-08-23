package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"google.golang.org/api/iterator"
)

// LogLine is a single archived log line: the timestamp the line was emitted
// and its text. The archive read path returns these in ascending timestamp
// order so an operator reconstructing a feature reads events in the order they
// happened.
type LogLine struct {
	// Timestamp is when the line was emitted (parsed from the shipped NDJSON
	// `timestamp` field, RFC3339). Used for absolute-window filtering.
	Timestamp time.Time
	// Text is the raw log line as written by the agent container.
	Text string
}

// ArchiveReader reads durable, off-cluster agent log lines for an absolute time
// window. It backs the `source=archive` branch of GET /agents/{name}/logs.
//
// Implementations MUST enforce per-agent isolation: a read for agent X must
// never return another agent's lines. The GCS implementation does this by
// listing strictly under the `agents/<agent>/` object prefix; see objectPrefix.
type ArchiveReader interface {
	// ReadAgentLines returns the named agent's lines emitted within the
	// inclusive window [since, until], in ascending timestamp order. A window
	// that matches no lines returns an empty result and a nil error (not an
	// error). The read is memory-bounded: rather than buffering the whole window
	// (kyber#455) it retains a capped set, and if it cannot return everything it
	// returns the NEWEST lines that fit with ReadResult.Truncated=true
	// (kyber#669).
	ReadAgentLines(ctx context.Context, agent string, since, until time.Time) (ReadResult, error)
}

// GenericArchiveSelection identifies one discovered pod/container archive
// prefix. Every field is resolved server-side from a live managed Pod before it
// reaches the reader; the reader validates segments again as defense in depth.
type GenericArchiveSelection struct {
	Component string
	Workload  string
	PodUID    string
	Container string
}

// PlatformArchiveReader reads the normalized logs/ archive lane.
type PlatformArchiveReader interface {
	ListContainerSelections(ctx context.Context, limit int) ([]GenericArchiveSelection, error)
	ReadContainerLines(ctx context.Context, selection GenericArchiveSelection, since, until time.Time) (ReadResult, error)
	StreamContainerRecords(ctx context.Context, selection GenericArchiveSelection, since, until time.Time, emit func(raw string, line LogLine) error) error
}

func parseGenericArchiveSelection(key string) (GenericArchiveSelection, bool) {
	parts := strings.Split(key, "/")
	if len(parts) < 7 || parts[0] != "logs" {
		return GenericArchiveSelection{}, false
	}
	selection := GenericArchiveSelection{Component: parts[1], Workload: parts[2], PodUID: parts[3], Container: parts[4]}
	for _, value := range []string{selection.Component, selection.Workload, selection.PodUID, selection.Container} {
		if !validArchiveSegment(value) {
			return GenericArchiveSelection{}, false
		}
	}
	return selection, true
}

func uniqueGenericArchiveSelections(keys []string, limit int) []GenericArchiveSelection {
	seen := map[GenericArchiveSelection]struct{}{}
	result := make([]GenericArchiveSelection, 0)
	for _, key := range keys {
		selection, ok := parseGenericArchiveSelection(key)
		if !ok {
			continue
		}
		if _, ok := seen[selection]; ok {
			continue
		}
		seen[selection] = struct{}{}
		result = append(result, selection)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result
}

type ArchiveReaderWithPlatform interface {
	ArchiveReader
	PlatformArchiveReader
}

// archiveLine is the on-the-wire NDJSON shape the log-shipper (Vector) writes,
// one JSON object per line. The reader only needs the emit timestamp and the
// raw message; any other fields Vector adds are ignored.
type archiveLine struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
}

// defaultRootPrefix is the object-key root for the durable agent-stdout archive
// surface (source=archive). The transcript surface (source=transcript, kyber#446)
// reuses this same reader with rootPrefix "transcripts/" instead — see
// effectiveRootPrefix. A reader constructed with an empty rootPrefix behaves
// exactly as before this change, so source=archive stays byte-for-byte unchanged.
const defaultRootPrefix = "agents/"

// transcriptRootPrefix is the object-key root for the Claude Code session
// transcript surface (source=transcript, kyber#446). A reader whose effective
// root is this prefix enables read-side dedup (kyber#454): the transcript-tailer
// ships each session JSONL from line 1 on every (re)start (`tail -n +1 -F`, a
// read-only PVC mount forbids a durable offset checkpoint), so cumulative
// re-ship objects return each message 2-3x — silently inflating any token/cost
// tally. Dedup collapses exact-id re-ships on read. The archive root ("agents/")
// stays dedup-off so source=archive is byte-for-byte unchanged.
const transcriptRootPrefix = "transcripts/"

// extractStableID extracts a transcript line's stable identity for dedup: the
// first non-empty of the agent-authored JSON's `message.id` (the Claude Code
// assistant-record id), then `uuid`, then `leafUuid`. It returns ok=false for a
// line that carries none of them (or is malformed / not a JSON object) so the
// caller PRESERVES that line rather than collapsing it (kyber#454 AC#2) — an
// id-less line can never be a known duplicate. The input is untrusted
// agent-authored JSON: it is parsed into a fixed minimal struct (extra/nested
// fields ignored) and bounded upstream by the scanner's 4 MiB per-line cap, so
// there is no parse-cost blowup and the id is only ever used as a map key.
func extractStableID(text string) (string, bool) {
	var probe struct {
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		UUID     string `json:"uuid"`
		LeafUUID string `json:"leafUuid"`
	}
	if err := json.Unmarshal([]byte(text), &probe); err != nil {
		return "", false
	}
	switch {
	case probe.Message.ID != "":
		return probe.Message.ID, true
	case probe.UUID != "":
		return probe.UUID, true
	case probe.LeafUUID != "":
		return probe.LeafUUID, true
	default:
		return "", false
	}
}

// effectiveRootPrefix resolves a reader's configured root prefix, defaulting an
// empty value to defaultRootPrefix ("agents/"). Centralizing the default here
// lets both the constructors and the struct-literal readers used in tests share
// one behavior without each having to normalize.
func effectiveRootPrefix(rootPrefix string) string {
	if rootPrefix == "" {
		return defaultRootPrefix
	}
	return rootPrefix
}

// objectPrefix is the object-key prefix under which a single agent's shipped log
// objects live for a given surface root: "<root><agent>/" (e.g. "agents/dave/"
// or "transcripts/dave/"). Per-agent isolation is structural — a read lists
// strictly under this prefix, so a query for agent X can never enumerate agent
// Y's objects, and the archive and transcript surfaces never intermix because
// their roots differ. The trailing slash matters: it stops agent "dave" from
// matching "dave2"'s keys. Agent names reaching here are already constrained by
// isValidName (^[a-z][a-z0-9-]{0,62}$), so the prefix cannot contain "/" or
// ".." path segments; the constraint is reasserted at the reader boundary as
// defense-in-depth (see validArchiveAgentName).
func objectPrefix(rootPrefix, agent string) string {
	return effectiveRootPrefix(rootPrefix) + agent + "/"
}

// validArchiveAgentName is the reader-boundary defense-in-depth check beyond the
// upstream isValidName: it rejects any name that could escape the per-agent
// prefix (path separators, traversal, empty). isValidName already guarantees
// this for live traffic; this keeps the reader safe even if called directly.
func validArchiveAgentName(agent string) bool {
	if agent == "" {
		return false
	}
	if strings.ContainsAny(agent, "/\\") || strings.Contains(agent, "..") {
		return false
	}
	return true
}

// Bounding caps for a single windowed read (kyber#455, kyber#669). They keep the
// control plane's peak memory AND the response size bounded regardless of how
// wide the window is or how many/large the shipped objects are: a read streams
// one object at a time, retains a capped newest-wins set, and stops scanning once
// the work cap is hit — marking the result truncated in either case. A
// whole-window read used to buffer every object's bytes at once and OOM-kill the
// control plane (CrashLoopBackOff, 502s on every endpoint); these caps plus
// per-object streaming (see windowScanner) bound it.
const (
	// defaultMaxReturnedLines caps the in-window lines a single read retains and
	// returns. The retained slice is a dominant memory cost of a read, so this
	// bounds it directly.
	defaultMaxReturnedLines = 50000
	// defaultMaxReturnedBytes caps the total TEXT bytes a single read retains and
	// returns (kyber#669). A line count alone does not bound a response: transcript
	// lines are agent-authored JSON whose size spans three orders of magnitude
	// (measured in production: p50 850 B, p99 15 KB, max 877 KB), so 50k lines
	// was observed to be an 84.7 MB body — enough to kill the reader's browser tab
	// before a single turn rendered. This cap is what makes a response size
	// predictable regardless of how fat the underlying lines are.
	defaultMaxReturnedBytes = 8 << 20 // 8 MiB
	// defaultMaxScannedBytes caps the total raw object bytes a single read scans
	// before stopping. It bounds work (and time) for a pathologically wide window
	// whose lines mostly fall out-of-window, complementing the per-object
	// streaming that bounds instantaneous memory.
	defaultMaxScannedBytes = 128 << 20 // 128 MiB
)

// scannerCaps bundles the three independent bounds on a single windowed read.
// They are distinct concerns and were previously conflated under one "maxBytes":
// maxScanBytes bounds WORK (how much raw object data we are willing to read),
// while maxLines/maxReturnBytes bound the RESULT (what we hand back to the
// caller). Grouping them in one struct keeps the call sites readable now that
// there are three.
type scannerCaps struct {
	// maxLines caps the retained line count. Zero means defaultMaxReturnedLines.
	maxLines int
	// maxReturnBytes caps the retained text bytes. Zero means defaultMaxReturnedBytes.
	maxReturnBytes int64
	// maxScanBytes caps raw bytes scanned before the read stops. Zero means
	// defaultMaxScannedBytes.
	maxScanBytes int64
}

// withDefaults resolves any zero-valued cap to its package default.
func (c scannerCaps) withDefaults() scannerCaps {
	if c.maxLines <= 0 {
		c.maxLines = defaultMaxReturnedLines
	}
	if c.maxReturnBytes <= 0 {
		c.maxReturnBytes = defaultMaxReturnedBytes
	}
	if c.maxScanBytes <= 0 {
		c.maxScanBytes = defaultMaxScannedBytes
	}
	return c
}

// ReadResult is the outcome of a windowed archive/transcript read: the in-window
// lines in ascending timestamp order, plus whether the read hit a cap and could
// not return the whole window. Truncated lets the HTTP layer emit an explicit,
// caller-visible truncation signal (kyber#455) instead of returning a silent
// partial — the caller can always tell the result was capped.
//
// When Truncated is true the lines are the NEWEST part of the window, not the
// oldest (kyber#669). The retention caps evict oldest-first, so a capped read
// still answers "what has this agent been doing lately" — the question every
// caller of this surface is actually asking.
type ReadResult struct {
	Lines     []LogLine
	Truncated bool
}

// parseArchiveLine parses one shipped NDJSON log line. A line is a JSON object
// carrying at least `timestamp` (RFC3339) and `message`. It returns ok=false for
// a blank, malformed, or timestamp-less line so the caller skips it — such a
// line can't be placed in an absolute window. This is the single per-line parse
// used by both the streaming windowScanner and parseNDJSONLines.
func parseArchiveLine(raw string) (LogLine, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return LogLine{}, false
	}
	var al archiveLine
	if err := json.Unmarshal([]byte(raw), &al); err != nil {
		return LogLine{}, false
	}
	if al.Timestamp == "" {
		return LogLine{}, false
	}
	ts, err := time.Parse(time.RFC3339, al.Timestamp)
	if err != nil {
		return LogLine{}, false
	}
	return LogLine{Timestamp: ts, Text: al.Message}, true
}

func streamArchiveRecords(reader io.Reader, since, until time.Time, emit func(string, LogLine) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		raw := scanner.Text()
		line, ok := parseArchiveLine(raw)
		if !ok || line.Timestamp.Before(since) || line.Timestamp.After(until) {
			continue
		}
		if err := emit(raw, line); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// heldLine is a retained in-window line plus its cached dedup identity. Caching
// the id here (rather than re-deriving it) matters because compaction has to
// rebuild the `seen` set from the survivors, and extractStableID is a JSON
// unmarshal — re-deriving would roughly double the parse cost of a large read on
// an endpoint that is already CPU-gated (kyber#463). Keeping id beside the line
// also means the two can never drift out of alignment across a sort.
type heldLine struct {
	LogLine
	// id is the line's stable dedup identity, or "" when it carries none.
	id string
}

// windowScanner is the streaming, memory-bounded core of a windowed read. The
// caller feeds it one object stream at a time (via scan); for each it reads
// line-by-line — never holding a whole object in memory — parses, and keeps only
// in-window lines.
//
// Retention is newest-wins (kyber#669): rather than accumulating a prefix and
// stopping at the first cap, the scanner keeps accumulating and periodically
// evicts the OLDEST retained lines until both retention caps (maxLines,
// maxReturnBytes) hold again. This is what makes a capped read return recent
// activity instead of a week-old prefix. Peak memory stays bounded because
// eviction triggers at twice the caps, so the retained set never exceeds
// 2*maxLines lines / 2*maxReturnBytes bytes between compactions.
//
// The scan-byte cap is the one bound that still halts the read outright: it
// bounds work, not the result, and there is nothing to evict in response to it.
// The caller MUST stop feeding objects once done() reports true.
type windowScanner struct {
	since, until time.Time
	caps         scannerCaps

	// dedup, when true (the transcript lane, kyber#454), drops exact-id re-ships:
	// a line whose stable id (extractStableID) has already been kept is skipped,
	// first-seen kept, timestamp order preserved. seen holds one entry per RETAINED
	// line only — compaction rebuilds it from the survivors — so
	// |seen| ≤ |lines| ≤ 2*maxLines and it adds no unbounded-memory vector (the
	// #455/#456 OOM bound is preserved).
	dedup bool
	seen  map[string]struct{}

	lines     []heldLine
	heldBytes int64
	scanned   int64
	// truncated records that the result is incomplete — either lines were evicted
	// or the scan cap halted the read. Reported to the caller via ReadResult.
	truncated bool
	// stopped records that the scan-byte cap halted the read. Distinct from
	// truncated because eviction must NOT stop the caller from feeding further
	// objects — the whole point is to reach the newest ones.
	stopped bool
}

// newWindowScanner builds a scanner for the inclusive window [since, until].
// Zero-valued caps fall back to the package defaults. dedup enables read-side
// stable-id dedup for the transcript lane (kyber#454); the archive lane passes
// false.
func newWindowScanner(since, until time.Time, caps scannerCaps, dedup bool) *windowScanner {
	ws := &windowScanner{since: since, until: until, caps: caps.withDefaults(), dedup: dedup}
	if dedup {
		ws.seen = make(map[string]struct{})
	}
	return ws
}

// done reports whether the read must stop feeding objects — i.e. the scan-byte
// cap was hit. Retention-cap eviction deliberately does NOT set this.
func (ws *windowScanner) done() bool { return ws.stopped }

// scan reads one object's NDJSON from r, retaining only in-window lines and
// compacting (evicting oldest) whenever the retention caps are exceeded; it stops
// early only when the scan-byte cap halts the read. It never materializes the
// whole object: it scans line-by-line via
// bufio.Scanner directly over the stream. The caller retains ownership of r and
// must Close it. An over-long line (beyond the 4MiB token cap) stops this
// object's scan silently — matching the prior parse behavior — while a genuine
// read (I/O) error is propagated so it surfaces as a 502 rather than a silent
// partial.
func (ws *windowScanner) scan(r io.Reader) error {
	if ws.stopped {
		return nil
	}
	sc := bufio.NewScanner(r)
	// Agent log lines can be long; raise the scanner's max token size well above
	// the 64KB default so a wide line isn't silently truncated/dropped.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		b := sc.Bytes()
		ws.scanned += int64(len(b)) + 1 // +1 approximates the consumed newline
		if ws.scanned > ws.caps.maxScanBytes {
			ws.stopped = true
			ws.truncated = true
			return nil
		}
		line, ok := parseArchiveLine(string(b))
		if !ok {
			continue
		}
		if line.Timestamp.Before(ws.since) || line.Timestamp.After(ws.until) {
			continue // out of window
		}
		// Transcript dedup (kyber#454): drop an exact-id re-ship before it counts
		// against the retention caps. The bytes were already scanned (and counted
		// toward maxScanBytes above), so the scan-work bound is unaffected; only the
		// retained set shrinks. A line with no stable id is always kept.
		var id string
		if ws.dedup {
			var haveID bool
			if id, haveID = extractStableID(line.Text); haveID {
				if _, dup := ws.seen[id]; dup {
					continue
				}
				ws.seen[id] = struct{}{} // recorded only on a RETAINED line
			}
		}
		ws.lines = append(ws.lines, heldLine{LogLine: line, id: id})
		ws.heldBytes += retainedSize(line)
		// Compact at twice the caps so eviction is amortized (each line is sorted
		// O(log n) times overall, not once per append) while peak retention stays
		// within a constant factor of the caps.
		if len(ws.lines) > 2*ws.caps.maxLines || ws.heldBytes > 2*ws.caps.maxReturnBytes {
			ws.compact()
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return err
	}
	return nil
}

// retainedSize is a line's contribution to the returned-byte budget: its text
// plus the newline the HTTP layer writes after it (see serveWindowedLines), so
// the cap bounds the response body rather than merely the text it carries.
func retainedSize(l LogLine) int64 { return int64(len(l.Text)) + 1 }

// compact evicts the OLDEST retained lines until both retention caps hold. It is
// the mechanism behind newest-wins retention (kyber#669): rather than refusing
// new lines once full, the scanner drops the least interesting ones it already
// has. When anything is evicted the result is marked truncated and — for the
// dedup lane — `seen` is rebuilt from the survivors so it stays bounded by the
// retained set rather than growing with every line ever scanned.
//
// Dropping an evicted line's id from `seen` is safe. A later re-ship of that
// line is re-admitted once, but it is older than everything retained (it was
// evicted as the oldest), so the next compaction evicts it again. It can never
// appear twice in the retained set, because the moment it is re-admitted its id
// is back in `seen`.
func (ws *windowScanner) compact() {
	// Newest first, so the survivors are a prefix. Stable so equal timestamps keep
	// their scan order and truncation is deterministic.
	sort.SliceStable(ws.lines, func(i, j int) bool {
		return ws.lines[j].Timestamp.Before(ws.lines[i].Timestamp)
	})

	keep := len(ws.lines)
	if keep > ws.caps.maxLines {
		keep = ws.caps.maxLines
	}
	var held int64
	for i := 0; i < keep; i++ {
		size := retainedSize(ws.lines[i].LogLine)
		// i > 0 keeps a single over-cap line rather than returning nothing. The
		// scanner's 4 MiB per-line token cap sits below maxReturnBytes, so in
		// practice this only guards a caller-supplied cap smaller than one line.
		if held+size > ws.caps.maxReturnBytes && i > 0 {
			keep = i
			break
		}
		held += size
	}

	ws.heldBytes = held
	if keep == len(ws.lines) {
		return // nothing evicted
	}
	ws.lines = ws.lines[:keep]
	ws.truncated = true
	if ws.dedup {
		ws.seen = make(map[string]struct{}, len(ws.lines))
		for i := range ws.lines {
			if ws.lines[i].id != "" {
				ws.seen[ws.lines[i].id] = struct{}{}
			}
		}
	}
}

// result applies a final compaction (so the returned set honors the caps exactly,
// not just the 2x compaction trigger), then returns the retained lines sorted
// ascending by timestamp. The retained set is bounded by the caps, so the sort is
// cheap and memory-safe.
func (ws *windowScanner) result() ReadResult {
	ws.compact()
	lines := make([]LogLine, len(ws.lines))
	for i := range ws.lines {
		lines[i] = ws.lines[i].LogLine
	}
	sort.SliceStable(lines, func(i, j int) bool {
		return lines[i].Timestamp.Before(lines[j].Timestamp)
	})
	return ReadResult{Lines: lines, Truncated: ws.truncated}
}

// parseNDJSONLines parses shipped NDJSON object bytes into LogLines, skipping
// blank/malformed/timestamp-less lines. It is the in-memory convenience over
// parseArchiveLine used by the storage-independent readFromObjects (and its
// tests); the production GCS/S3 readers stream via windowScanner instead so they
// never hold a whole object in memory.
func parseNDJSONLines(data []byte) []LogLine {
	var out []LogLine
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if l, ok := parseArchiveLine(sc.Text()); ok {
			out = append(out, l)
		}
	}
	return out
}

// readFromObjects is the storage-independent, in-memory equivalent of a windowed
// read: given a set of objects keyed by storage key, it scans only those under
// the agent's prefix (isolation) through the same bounded windowScanner the
// streaming readers use, returning the in-window lines sorted ascending. The
// streaming GCS/S3 readers are equivalent to this over their live object sets;
// keeping it lets the isolation + window logic be unit-tested without a live
// backend.
func readFromObjects(objects map[string][]byte, rootPrefix, agent string, since, until time.Time) ([]LogLine, error) {
	if !validArchiveAgentName(agent) {
		return nil, fmt.Errorf("invalid agent name %q", agent)
	}
	prefix := objectPrefix(rootPrefix, agent)
	// Deterministic key order so any truncation is stable; the windowed result is
	// sorted by timestamp regardless of key order.
	keys := make([]string, 0, len(objects))
	for key := range objects {
		if strings.HasPrefix(key, prefix) { // isolation: skip other agents' objects
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	ws := newWindowScanner(since, until, scannerCaps{}, effectiveRootPrefix(rootPrefix) == transcriptRootPrefix)
	for _, key := range keys {
		if ws.done() {
			break
		}
		if err := ws.scan(bytes.NewReader(objects[key])); err != nil {
			return nil, err
		}
	}
	return ws.result().Lines, nil
}

// GCSArchiveReader reads shipped agent logs from a GCS bucket via Application
// Default Credentials (node ADC — no static key material). It lists strictly
// under the per-agent prefix, narrowed to the day-partitions the window spans,
// so list cost is proportional to the window, not the agent's total history.
type GCSArchiveReader struct {
	client *storage.Client
	bucket string
	// rootPrefix is the object-key root this reader serves ("agents/" for
	// source=archive, "transcripts/" for source=transcript). Empty means
	// "agents/" (see effectiveRootPrefix) so an unset value is backward
	// compatible. The same bucket can back two readers that differ only here.
	rootPrefix string
	// caps bound a single read's memory and work (kyber#455, kyber#669). Zero
	// fields mean the package defaults (see scannerCaps.withDefaults).
	// Configurable mainly so tests can drive truncation with small caps.
	caps scannerCaps
}

// NewGCSArchiveReader builds a reader against the named bucket using ADC. The
// caller owns the returned reader's lifecycle and should Close it at shutdown.
// rootPrefix selects the surface root ("agents/" or "transcripts/"); an empty
// value defaults to "agents/" (the durable-archive surface).
func NewGCSArchiveReader(ctx context.Context, bucket, rootPrefix string) (*GCSArchiveReader, error) {
	if bucket == "" {
		return nil, fmt.Errorf("archive bucket name is empty")
	}
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create GCS client: %w", err)
	}
	return &GCSArchiveReader{client: client, bucket: bucket, rootPrefix: effectiveRootPrefix(rootPrefix)}, nil
}

// Close releases the underlying GCS client.
func (g *GCSArchiveReader) Close() error {
	if g.client == nil {
		return nil
	}
	return g.client.Close()
}

// dayPartitionPrefixes returns the "<root><agent>/<YYYY-MM-DD>/" object
// prefixes for each UTC day the inclusive window [since, until] touches. The
// shipper keys objects by the UTC date of the line's emit timestamp, so these
// prefixes bound the listing to exactly the relevant days.
//
// Prefixes are returned NEWEST DAY FIRST (kyber#669). Retention already evicts
// oldest-first, so scan order does not decide what a completed read returns —
// but the scan-byte cap can halt a read part-way, and visiting the oldest day
// first meant a busy agent spent that entire budget on stale days and never
// reached today. Measured in production: one agent's 7-day window is ~167 MB of
// raw objects against a 128 MiB scan cap, so the cap genuinely binds and the
// most recent ~18 hours were never read. Newest-first spends the budget where
// the caller is looking.
func dayPartitionPrefixes(rootPrefix, agent string, since, until time.Time) []string {
	base := objectPrefix(rootPrefix, agent)
	start := since.UTC().Truncate(24 * time.Hour)
	end := until.UTC().Truncate(24 * time.Hour)
	var prefixes []string
	for d := end; !d.Before(start); d = d.Add(-24 * time.Hour) {
		prefixes = append(prefixes, base+d.Format("2006-01-02")+"/")
	}
	return prefixes
}

func validArchiveSegment(value string) bool {
	return value != "" && !strings.ContainsAny(value, "/\\") && !strings.Contains(value, "..")
}

func genericDayPartitionPrefixes(selection GenericArchiveSelection, since, until time.Time) ([]string, error) {
	for name, value := range map[string]string{
		"component": selection.Component, "workload": selection.Workload,
		"pod UID": selection.PodUID, "container": selection.Container,
	} {
		if !validArchiveSegment(value) {
			return nil, fmt.Errorf("invalid archive %s %q", name, value)
		}
	}
	base := "logs/" + selection.Component + "/" + selection.Workload + "/" + selection.PodUID + "/" + selection.Container + "/"
	start := since.UTC().Truncate(24 * time.Hour)
	end := until.UTC().Truncate(24 * time.Hour)
	var prefixes []string
	for d := end; !d.Before(start); d = d.Add(-24 * time.Hour) {
		prefixes = append(prefixes, base+d.Format("2006-01-02")+"/")
	}
	return prefixes, nil
}

// ReadAgentLines implements ArchiveReader against GCS. See the interface doc for
// the contract; isolation is enforced by listing only under the agent prefix.
func (g *GCSArchiveReader) ReadAgentLines(ctx context.Context, agent string, since, until time.Time) (ReadResult, error) {
	if !validArchiveAgentName(agent) {
		return ReadResult{}, fmt.Errorf("invalid agent name %q", agent)
	}
	bkt := g.client.Bucket(g.bucket)
	ws := newWindowScanner(since, until, g.caps, effectiveRootPrefix(g.rootPrefix) == transcriptRootPrefix)
	for _, prefix := range dayPartitionPrefixes(g.rootPrefix, agent, since, until) {
		if ws.done() {
			break
		}
		it := bkt.Objects(ctx, &storage.Query{Prefix: prefix})
		for {
			if ws.done() {
				break
			}
			attrs, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return ReadResult{}, fmt.Errorf("list %q: %w", prefix, err)
			}
			// Stream this object straight into the scanner and release it before
			// fetching the next, so at most one object's bytes are resident.
			if err := g.scanObject(ctx, bkt, attrs.Name, ws); err != nil {
				return ReadResult{}, err
			}
		}
	}
	return ws.result(), nil
}

func (g *GCSArchiveReader) ReadContainerLines(ctx context.Context, selection GenericArchiveSelection, since, until time.Time) (ReadResult, error) {
	prefixes, err := genericDayPartitionPrefixes(selection, since, until)
	if err != nil {
		return ReadResult{}, err
	}
	bkt := g.client.Bucket(g.bucket)
	ws := newWindowScanner(since, until, g.caps, false)
	for _, prefix := range prefixes {
		it := bkt.Objects(ctx, &storage.Query{Prefix: prefix})
		for !ws.done() {
			attrs, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return ReadResult{}, fmt.Errorf("list %q: %w", prefix, err)
			}
			if err := g.scanObject(ctx, bkt, attrs.Name, ws); err != nil {
				return ReadResult{}, err
			}
		}
	}
	return ws.result(), nil
}

func (g *GCSArchiveReader) ListContainerSelections(ctx context.Context, limit int) ([]GenericArchiveSelection, error) {
	it := g.client.Bucket(g.bucket).Objects(ctx, &storage.Query{Prefix: "logs/"})
	keys := make([]string, 0)
	for limit <= 0 || len(keys) < limit*4 {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list archived logging targets: %w", err)
		}
		keys = append(keys, attrs.Name)
	}
	return uniqueGenericArchiveSelections(keys, limit), nil
}

func (g *GCSArchiveReader) StreamContainerRecords(ctx context.Context, selection GenericArchiveSelection, since, until time.Time, emit func(string, LogLine) error) error {
	prefixes, err := genericDayPartitionPrefixes(selection, since, until)
	if err != nil {
		return err
	}
	bkt := g.client.Bucket(g.bucket)
	for i := len(prefixes) - 1; i >= 0; i-- {
		it := bkt.Objects(ctx, &storage.Query{Prefix: prefixes[i]})
		for {
			attrs, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return fmt.Errorf("list %q: %w", prefixes[i], err)
			}
			rc, err := bkt.Object(attrs.Name).NewReader(ctx)
			if err != nil {
				return fmt.Errorf("open %q: %w", attrs.Name, err)
			}
			err = streamArchiveRecords(rc, since, until, emit)
			closeErr := rc.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return fmt.Errorf("close %q: %w", attrs.Name, closeErr)
			}
		}
	}
	return nil
}

// scanObject opens one GCS object as a stream and feeds it to the scanner,
// closing it before returning. It never reads the whole object into memory.
func (g *GCSArchiveReader) scanObject(ctx context.Context, bkt *storage.BucketHandle, name string, ws *windowScanner) error {
	rc, err := bkt.Object(name).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("open %q: %w", name, err)
	}
	defer rc.Close()
	if err := ws.scan(rc); err != nil {
		return fmt.Errorf("read %q: %w", name, err)
	}
	return nil
}

// s3ObjectStore is the minimal object-store surface the S3 reader needs: list
// the keys under a prefix, and fetch one object's bytes by key. Abstracting it
// behind this interface (defined at the consumer, per docs/contributing/code-quality.md § Naming)
// keeps the reader's list+fetch+delegate logic unit-testable without a live
// MinIO — the real implementation wraps a minio.Client; tests inject a fake.
type s3ObjectStore interface {
	listKeys(ctx context.Context, prefix string) ([]string, error)
	listKeysLimit(ctx context.Context, prefix string, limit int) ([]string, error)
	// getObject opens one object as a stream. The caller MUST Close the returned
	// reader. Returning a stream (not []byte) is what lets the reader scan an
	// object without materializing it whole — the memory bound for a single large
	// object (kyber#455).
	getObject(ctx context.Context, key string) (io.ReadCloser, error)
}

// S3ArchiveReader reads shipped agent logs from any S3-compatible object store
// (MinIO, AWS S3) via static access-key/secret credentials sourced from a
// Kubernetes Secret — unlike the GCS reader's node ADC. The read semantics are
// identical to GCS: it lists strictly under the per-agent prefix, narrowed to
// the day-partitions the window spans, and streams each object into the shared
// windowScanner — so per-agent isolation and absolute-window filtering hold by
// construction, the same as GCS. (readFromObjects is the in-memory equivalent of
// this loop, kept for unit-testing that shared core without a live backend.)
type S3ArchiveReader struct {
	store  s3ObjectStore
	bucket string
	// rootPrefix is the object-key root this reader serves ("agents/" for
	// source=archive, "transcripts/" for source=transcript). Empty defaults to
	// "agents/" (see effectiveRootPrefix), so a struct-literal reader without it
	// behaves exactly as the archive reader.
	rootPrefix string
	// caps bound a single read's memory and work (kyber#455, kyber#669). Zero
	// fields mean the package defaults (see scannerCaps.withDefaults).
	// Configurable mainly so tests can drive truncation with small caps.
	caps scannerCaps
}

// minioObjectStore is the production s3ObjectStore backed by a minio.Client.
type minioObjectStore struct {
	client *minio.Client
	bucket string
}

func (m *minioObjectStore) listKeys(ctx context.Context, prefix string) ([]string, error) {
	return m.listKeysLimit(ctx, prefix, 0)
}

func (m *minioObjectStore) listKeysLimit(ctx context.Context, prefix string, limit int) ([]string, error) {
	var keys []string
	for obj := range m.client.ListObjects(ctx, m.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("listing %q: %w", prefix, obj.Err)
		}
		keys = append(keys, obj.Key)
		if limit > 0 && len(keys) >= limit {
			break
		}
	}
	return keys, nil
}

func (m *minioObjectStore) getObject(ctx context.Context, key string) (io.ReadCloser, error) {
	// *minio.Object is a lazy io.ReadCloser: it opens on first Read and the
	// caller Closes it. Returning it (rather than io.ReadAll-ing here) keeps a
	// single large object from being buffered whole in memory.
	obj, err := m.client.GetObject(ctx, m.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("opening %q: %w", key, err)
	}
	return obj, nil
}

// NewS3ArchiveReader builds a reader against the named bucket on an
// S3-compatible endpoint. Credentials are static (access/secret key) and MUST be
// sourced from a Kubernetes Secret by the caller — never committed. useTLS
// toggles HTTPS (default on for external S3; may be plaintext for cluster-internal
// MinIO). region may be empty for MinIO. The caller owns the reader's lifecycle.
//
// endpoint may be a bare host[:port] or a full URL. The chart's shared
// logShipper.endpoint value is a URL whose scheme selects TLS (the Vector
// shipper consumes it that way), but minio-go rejects any scheme/path —
// passing "http://host:9000" yields "Endpoint url cannot have fully qualified
// paths." So we normalize here: strip a leading scheme, and let that scheme
// drive TLS (http→plaintext, https→TLS) so the reader honors the same
// "scheme selects TLS" contract as the shipper. A bare endpoint falls back to
// the explicit useTLS flag.
// rootPrefix selects the surface root ("agents/" or "transcripts/"); an empty
// value defaults to "agents/" (the durable-archive surface).
func NewS3ArchiveReader(ctx context.Context, endpoint, bucket, region, accessKey, secretKey string, useTLS bool, rootPrefix string) (*S3ArchiveReader, error) {
	if bucket == "" {
		return nil, fmt.Errorf("archive bucket name is empty")
	}
	if endpoint == "" {
		return nil, fmt.Errorf("archive S3 endpoint is empty")
	}
	endpoint, useTLS = normalizeS3Endpoint(endpoint, useTLS)
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useTLS,
		Region: region,
	})
	if err != nil {
		return nil, fmt.Errorf("create S3 client: %w", err)
	}
	return &S3ArchiveReader{store: &minioObjectStore{client: client, bucket: bucket}, bucket: bucket, rootPrefix: effectiveRootPrefix(rootPrefix)}, nil
}

// normalizeS3Endpoint turns a possibly-schemed endpoint into the bare
// host[:port] minio-go requires. If a scheme is present it overrides useTLS
// (https→true, http→false), matching the chart's "scheme selects TLS"
// convention; an unrecognized scheme leaves useTLS untouched. A trailing slash
// is trimmed (minio-go treats a non-empty path as a fully-qualified-path error).
func normalizeS3Endpoint(endpoint string, useTLS bool) (string, bool) {
	if i := strings.Index(endpoint, "://"); i >= 0 {
		switch strings.ToLower(endpoint[:i]) {
		case "https":
			useTLS = true
		case "http":
			useTLS = false
		}
		endpoint = endpoint[i+len("://"):]
	}
	return strings.TrimRight(endpoint, "/"), useTLS
}

// Close releases the reader. The minio client holds no long-lived handle that
// needs closing; Close exists for symmetry with GCSArchiveReader so callers can
// treat any ArchiveReader uniformly.
func (s *S3ArchiveReader) Close() error { return nil }

// ReadAgentLines implements ArchiveReader against S3/MinIO. See the interface doc
// for the contract. Isolation is enforced by listing only under the agent's
// day-partition prefixes; each object is streamed into the same windowScanner the
// GCS reader uses, so window filtering and retention are identical across both.
func (s *S3ArchiveReader) ReadAgentLines(ctx context.Context, agent string, since, until time.Time) (ReadResult, error) {
	if !validArchiveAgentName(agent) {
		return ReadResult{}, fmt.Errorf("invalid agent name %q", agent)
	}
	ws := newWindowScanner(since, until, s.caps, effectiveRootPrefix(s.rootPrefix) == transcriptRootPrefix)
	for _, prefix := range dayPartitionPrefixes(s.rootPrefix, agent, since, until) {
		if ws.done() {
			break
		}
		keys, err := s.store.listKeys(ctx, prefix)
		if err != nil {
			return ReadResult{}, fmt.Errorf("list %q: %w", prefix, err)
		}
		for _, key := range keys {
			if ws.done() {
				break
			}
			// Fetch+scan+release one object at a time so the whole window is never
			// resident — the OOM fix. The list above is already agent-prefix scoped,
			// so isolation holds the same way it does for GCS.
			if err := s.scanKey(ctx, key, ws); err != nil {
				return ReadResult{}, err
			}
		}
	}
	return ws.result(), nil
}

func (s *S3ArchiveReader) ReadContainerLines(ctx context.Context, selection GenericArchiveSelection, since, until time.Time) (ReadResult, error) {
	prefixes, err := genericDayPartitionPrefixes(selection, since, until)
	if err != nil {
		return ReadResult{}, err
	}
	ws := newWindowScanner(since, until, s.caps, false)
	for _, prefix := range prefixes {
		keys, err := s.store.listKeys(ctx, prefix)
		if err != nil {
			return ReadResult{}, fmt.Errorf("list %q: %w", prefix, err)
		}
		for _, key := range keys {
			if ws.done() {
				break
			}
			if err := s.scanKey(ctx, key, ws); err != nil {
				return ReadResult{}, err
			}
		}
	}
	return ws.result(), nil
}

func (s *S3ArchiveReader) ListContainerSelections(ctx context.Context, limit int) ([]GenericArchiveSelection, error) {
	keyLimit := 0
	if limit > 0 {
		keyLimit = limit * 10
	}
	keys, err := s.store.listKeysLimit(ctx, "logs/", keyLimit)
	if err != nil {
		return nil, fmt.Errorf("list archived logging targets: %w", err)
	}
	return uniqueGenericArchiveSelections(keys, limit), nil
}

func (s *S3ArchiveReader) StreamContainerRecords(ctx context.Context, selection GenericArchiveSelection, since, until time.Time, emit func(string, LogLine) error) error {
	prefixes, err := genericDayPartitionPrefixes(selection, since, until)
	if err != nil {
		return err
	}
	for i := len(prefixes) - 1; i >= 0; i-- {
		keys, err := s.store.listKeys(ctx, prefixes[i])
		if err != nil {
			return fmt.Errorf("list %q: %w", prefixes[i], err)
		}
		for _, key := range keys {
			rc, err := s.store.getObject(ctx, key)
			if err != nil {
				return fmt.Errorf("read %q: %w", key, err)
			}
			err = streamArchiveRecords(rc, since, until, emit)
			closeErr := rc.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return fmt.Errorf("close %q: %w", key, closeErr)
			}
		}
	}
	return nil
}

// scanKey opens one S3 object as a stream and feeds it to the scanner, closing
// it before returning. It never reads the whole object into memory.
func (s *S3ArchiveReader) scanKey(ctx context.Context, key string, ws *windowScanner) error {
	rc, err := s.store.getObject(ctx, key)
	if err != nil {
		return fmt.Errorf("read %q: %w", key, err)
	}
	defer rc.Close()
	if err := ws.scan(rc); err != nil {
		return fmt.Errorf("read %q: %w", key, err)
	}
	return nil
}
