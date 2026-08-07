package api

// transcript_compact.go — kyber#458 Part B: reclaim pre-fix duplicate transcript
// objects. The transcript-tailer used to re-ship a whole session on every
// sidecar restart (fixed at source in Part A), leaving multiple overlapping
// cumulative NDJSON objects under each transcripts/<agent>/<date>/ prefix. This
// merges the overlapping objects in one prefix into a single deduped superset
// and deletes the redundant copies.
//
// Safety (this MUTATES durable audit data — the highest-risk path):
//   - Dry-run is the DEFAULT (CompactOptions.Apply=false): it reports reclaimable
//     bytes/objects and writes/deletes NOTHING.
//   - Apply is write-then-verify-then-delete: the merged superset is written
//     FIRST, then read back and verified to contain every stable id from every
//     source object (read-back equivalence), and only then are the redundant
//     sources deleted. A verify failure aborts the prefix with nothing deleted.
//   - Memory is bounded the same way reads are (kyber#456): each object is
//     streamed line-by-line, and a prefix whose distinct content exceeds the
//     line/byte cap is SKIPPED (never written truncated/lossy) — the 30-day
//     lifecycle expiry remains the backstop for those.
//   - Dedup uses the SAME stable id as the read path (extractStableID, kyber#454)
//     so a post-compaction read returns the same deduped set it would have
//     before (AC#4), and per-agent prefix scoping (validArchiveAgentName) keeps a
//     merge within one agent's lane.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// compactedObjectName is the fixed key suffix the merged superset is written to
// under each <agent>/<date>/ prefix. A fixed name makes compaction idempotent: a
// re-run reads the prior compacted object back in as a source and re-merges it,
// never accumulating compacted-of-compacted copies. Reads list the whole prefix,
// so any *.ndjson here (including this one) is picked up.
const compactedObjectName = "compacted.ndjson"

// defaultTranscriptRoot is the lane this tool operates on. The archive lane
// ("agents/") has no re-ship amplification, so compaction is transcript-only.
const defaultTranscriptRoot = "transcripts/"

// CompactionStore is the object-store surface compaction needs: list + stream-read
// + write + delete. It is a SUPERSET of the read-only archive reader's surface —
// the extra write+delete is the genuinely higher privilege compaction holds, so
// it is a distinct interface (the reader must never gain delete). cmd/transcript-
// compact builds one per backend (S3/MinIO or GCS) and hands it to
// CompactTranscripts.
type CompactionStore interface {
	ListKeys(ctx context.Context, prefix string) ([]string, error)
	GetObject(ctx context.Context, key string) (io.ReadCloser, error)
	PutObject(ctx context.Context, key string, data []byte) error
	RemoveObject(ctx context.Context, key string) error
}

// CompactOptions controls a compaction run.
type CompactOptions struct {
	// RootPrefix is the lane root; empty defaults to "transcripts/".
	RootPrefix string
	// Agent, when non-empty, limits compaction to one agent's prefix. Empty
	// compacts every agent under the root.
	Agent string
	// Apply performs the destructive merge+delete. False (default) is a dry-run:
	// report only, no writes or deletes.
	Apply bool
	// MaxLines / MaxBytes bound a single prefix merge (0 = the package read
	// defaults). A prefix whose distinct content exceeds either is skipped, never
	// written lossy.
	MaxLines int
	MaxBytes int64
}

// PrefixReport is the per-<agent>/<date> outcome.
type PrefixReport struct {
	Prefix        string
	SourceObjects int
	SourceBytes   int64
	MergedLines   int
	MergedBytes   int64
	ReclaimBytes  int64 // SourceBytes - MergedBytes (freed by --apply)
	Applied       bool
	Skipped       bool
	SkipReason    string
}

// CompactionReport is the whole-run summary.
type CompactionReport struct {
	DryRun       bool
	Prefixes     []PrefixReport
	TotalReclaim int64
}

// CompactTranscripts discovers every transcripts/<agent>/<date>/ prefix (or one
// agent's), merges overlapping NDJSON objects per prefix to a deduped superset,
// and (under Apply) deletes the redundant copies. It returns a report suitable
// for the dry-run reclaimable-bytes summary. Memory stays within the per-prefix
// streaming caps; an oversized prefix is reported skipped, not compacted.
func CompactTranscripts(ctx context.Context, store CompactionStore, opts CompactOptions) (CompactionReport, error) {
	root := opts.RootPrefix
	if root == "" {
		root = defaultTranscriptRoot
	}
	if opts.Agent != "" && !validArchiveAgentName(opts.Agent) {
		return CompactionReport{}, fmt.Errorf("invalid agent name %q", opts.Agent)
	}

	listPrefix := root
	if opts.Agent != "" {
		listPrefix = root + opts.Agent + "/"
	}
	keys, err := store.ListKeys(ctx, listPrefix)
	if err != nil {
		return CompactionReport{}, fmt.Errorf("listing %q: %w", listPrefix, err)
	}

	// Group keys by their <agent>/<date>/ day prefix (the first two path segments
	// under the root). Keys not at that depth are ignored.
	groups := map[string][]string{}
	for _, k := range keys {
		if dp, ok := dayPrefixOf(root, k); ok {
			groups[dp] = append(groups[dp], k)
		}
	}

	report := CompactionReport{DryRun: !opts.Apply}
	dayPrefixes := make([]string, 0, len(groups))
	for dp := range groups {
		dayPrefixes = append(dayPrefixes, dp)
	}
	sort.Strings(dayPrefixes) // deterministic order

	for _, dp := range dayPrefixes {
		objs := groups[dp]
		sort.Strings(objs)
		pr := compactPrefix(ctx, store, dp, objs, opts)
		if pr == nil {
			continue // nothing redundant here
		}
		report.Prefixes = append(report.Prefixes, *pr)
		if !pr.Skipped {
			report.TotalReclaim += pr.ReclaimBytes
		}
	}
	return report, nil
}

// dayPrefixOf returns the "<root><agent>/<date>/" prefix for an object key, and
// false if the key isn't at least root/<agent>/<date>/<file>.
func dayPrefixOf(root, key string) (string, bool) {
	if !strings.HasPrefix(key, root) {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(key, root), "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return "", false // need <agent>/<date>/<file...>
	}
	return root + parts[0] + "/" + parts[1] + "/", true
}

// compactPrefix merges one day prefix. Returns nil when there's nothing to
// reclaim (≤1 source object, or no redundancy). A prefix exceeding the caps is
// returned Skipped (never written lossy).
func compactPrefix(ctx context.Context, store CompactionStore, prefix string, objs []string, opts CompactOptions) *PrefixReport {
	if len(objs) <= 1 {
		return nil // only cross-object re-ships create redundancy
	}

	pr := &PrefixReport{Prefix: prefix, SourceObjects: len(objs)}
	m := newMerger(opts.MaxLines, opts.MaxBytes)

	for _, key := range objs {
		rc, err := store.GetObject(ctx, key)
		if err != nil {
			pr.Skipped, pr.SkipReason = true, fmt.Sprintf("read %s: %v", key, err)
			return pr
		}
		err = m.addObject(rc)
		rc.Close()
		if err != nil {
			pr.Skipped, pr.SkipReason = true, fmt.Sprintf("read %s: %v", key, err)
			return pr
		}
		if m.over {
			pr.SourceBytes = m.scanned
			pr.Skipped = true
			pr.SkipReason = "distinct content exceeds the compaction cap; left for lifecycle expiry"
			return pr
		}
	}

	merged := m.serialize()
	pr.SourceBytes = m.scanned
	pr.MergedLines = len(m.kept)
	pr.MergedBytes = int64(len(merged))
	pr.ReclaimBytes = pr.SourceBytes - pr.MergedBytes
	if pr.ReclaimBytes <= 0 {
		return nil // sources already disjoint — not worth mutating
	}

	if !opts.Apply {
		return pr // dry-run: report only
	}

	// --- destructive path (Apply): write → read-back verify → delete ---
	mergedKey := prefix + compactedObjectName
	if err := store.PutObject(ctx, mergedKey, merged); err != nil {
		pr.Skipped, pr.SkipReason = true, fmt.Sprintf("write merged %s: %v (no deletes)", mergedKey, err)
		return pr
	}
	if err := verifyMergedContains(ctx, store, mergedKey, m.unionIDs); err != nil {
		pr.Skipped, pr.SkipReason = true, fmt.Sprintf("read-back verify failed for %s: %v (no deletes)", mergedKey, err)
		return pr
	}
	for _, key := range objs {
		if key == mergedKey {
			continue // never delete the object we just wrote
		}
		if err := store.RemoveObject(ctx, key); err != nil {
			pr.Applied = true
			pr.SkipReason = fmt.Sprintf("partial: merged written+verified but delete %s failed: %v", key, err)
			return pr
		}
	}
	pr.Applied = true
	return pr
}

// mergeLine is one kept raw line plus its parsed timestamp (for ordering).
type mergeLine struct {
	ts  time.Time
	raw string
}

// merger accumulates the deduped raw-line superset for one prefix, bounded by
// the same caps as a read (kyber#456). It keeps RAW line bytes verbatim —
// windowScanner's LogLine is lossy (timestamp + message only), which would
// corrupt the rich transcript records on write-back — but reuses parseArchiveLine
// + extractStableID so dedup matches the read path exactly.
type merger struct {
	maxLines int
	maxBytes int64
	seen     map[string]struct{} // stable ids already kept
	unionIDs map[string]struct{} // every stable id seen (read-back verify)
	kept     []mergeLine
	scanned  int64
	over     bool
}

func newMerger(maxLines int, maxBytes int64) *merger {
	if maxLines <= 0 {
		maxLines = defaultMaxReturnedLines
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxScannedBytes
	}
	return &merger{
		maxLines: maxLines,
		maxBytes: maxBytes,
		seen:     map[string]struct{}{},
		unionIDs: map[string]struct{}{},
	}
}

// addObject streams one object, appending first-seen raw lines (deduped by stable
// id; lines with no id always kept, preserving completeness) until a cap is hit
// (then over=true). Blank lines are skipped.
func (m *merger) addObject(r io.Reader) error {
	if m.over {
		return nil
	}
	sc := newLineScanner(r)
	for sc.Scan() {
		b := sc.Bytes()
		m.scanned += int64(len(b)) + 1
		if m.scanned > m.maxBytes {
			m.over = true
			return nil
		}
		raw := strings.TrimSpace(string(b))
		if raw == "" {
			continue
		}
		// The shipped line wraps the raw Claude JSONL in its `message` field; the
		// stable id lives in that inner record. Match the read path exactly
		// (windowScanner.scan → extractStableID(line.Text)) so compaction dedups
		// on the same id a read does (AC#4). A line that doesn't parse as the
		// wrapped shape is kept verbatim with no id (can't be a known dup).
		var ts time.Time
		var inner string
		if line, ok := parseArchiveLine(raw); ok {
			ts = line.Timestamp
			inner = line.Text
		}
		id, haveID := extractStableID(inner)
		if haveID {
			m.unionIDs[id] = struct{}{}
			if _, dup := m.seen[id]; dup {
				continue // redundant re-ship — drop from the superset
			}
		}
		if len(m.kept) >= m.maxLines {
			m.over = true
			return nil
		}
		m.kept = append(m.kept, mergeLine{ts: ts, raw: raw})
		if haveID {
			m.seen[id] = struct{}{}
		}
	}
	return sc.Err()
}

// serialize returns the kept superset as NDJSON, ordered chronologically (the
// read path sorts by timestamp anyway, so stored order doesn't affect read
// correctness — this just keeps the merged object tidy).
func (m *merger) serialize() []byte {
	sort.SliceStable(m.kept, func(i, j int) bool { return m.kept[i].ts.Before(m.kept[j].ts) })
	var buf bytes.Buffer
	for i := range m.kept {
		buf.WriteString(m.kept[i].raw)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// verifyMergedContains re-reads the merged object and confirms its stable-id set
// is a superset of want — the read-back equivalence check that gates deletion.
func verifyMergedContains(ctx context.Context, store CompactionStore, key string, want map[string]struct{}) error {
	rc, err := store.GetObject(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	got := map[string]struct{}{}
	sc := newLineScanner(rc)
	for sc.Scan() {
		line, ok := parseArchiveLine(strings.TrimSpace(sc.Text()))
		if !ok {
			continue
		}
		if id, ok := extractStableID(line.Text); ok {
			got[id] = struct{}{}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	for id := range want {
		if _, ok := got[id]; !ok {
			return fmt.Errorf("merged object missing id %q", id)
		}
	}
	return nil
}

// newLineScanner is the shared bufio.Scanner config used by the windowed read and
// compaction: the same 4MiB max-token buffer so a wide transcript line isn't
// silently dropped (matches windowScanner.scan, kyber#456).
func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return sc
}
