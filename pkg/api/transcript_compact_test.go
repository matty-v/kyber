package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
)

// fakeCompactionStore is an in-memory CompactionStore for compaction tests.
type fakeCompactionStore struct {
	objects map[string][]byte
	puts    []string // keys written, in order
	removed []string // keys deleted, in order
}

func newFakeStore() *fakeCompactionStore {
	return &fakeCompactionStore{objects: map[string][]byte{}}
}

func (f *fakeCompactionStore) ListKeys(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (f *fakeCompactionStore) GetObject(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("not found: %s", key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeCompactionStore) PutObject(_ context.Context, key string, data []byte) error {
	f.objects[key] = append([]byte(nil), data...)
	f.puts = append(f.puts, key)
	return nil
}

func (f *fakeCompactionStore) RemoveObject(_ context.Context, key string) error {
	if _, ok := f.objects[key]; !ok {
		return fmt.Errorf("not found: %s", key)
	}
	delete(f.objects, key)
	f.removed = append(f.removed, key)
	return nil
}

// line builds a SHIPPED transcript NDJSON line: Vector wraps the raw Claude
// JSONL record (which carries the stable `uuid` + a `text` we use as a content
// marker) inside the outer `message` field, with an outer `timestamp` for
// windowing — exactly the shape parseArchiveLine + extractStableID consume.
func line(uuid, ts, marker string) string {
	inner := fmt.Sprintf(`{"uuid":%q,"text":%q}`, uuid, marker)
	return fmt.Sprintf(`{"timestamp":%q,"message":%q}`, ts, inner)
}

func ndjson(lines ...string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

// markersIn parses a merged NDJSON blob and returns the ordered list of content
// markers (the inner `text`) — used to assert completeness + ordering.
func markersIn(t *testing.T, data []byte) []string {
	t.Helper()
	var out []string
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if ln == "" {
			continue
		}
		outer, ok := parseArchiveLine(ln)
		if !ok {
			t.Fatalf("unparseable merged line: %q", ln)
		}
		var inner struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(outer.Text), &inner); err != nil {
			t.Fatalf("inner message not JSON: %q", outer.Text)
		}
		out = append(out, inner.Text)
	}
	return out
}

const dayPrefix = "transcripts/dave/2026-05-24/"

// TestCompactTranscripts_DryRunReportsNoMutation is AC#7: the default dry-run
// reports reclaimable bytes but writes/deletes NOTHING.
func TestCompactTranscripts_DryRunReportsNoMutation(t *testing.T) {
	store := newFakeStore()
	// Two cumulative re-ships: B re-ships A's lines and adds u4.
	store.objects[dayPrefix+"0001-a.ndjson"] = ndjson(
		line("u1", "2026-05-24T10:00:00Z", "one"),
		line("u2", "2026-05-24T10:01:00Z", "two"),
		line("u3", "2026-05-24T10:02:00Z", "three"),
	)
	store.objects[dayPrefix+"0002-b.ndjson"] = ndjson(
		line("u1", "2026-05-24T10:00:00Z", "one"),
		line("u2", "2026-05-24T10:01:00Z", "two"),
		line("u3", "2026-05-24T10:02:00Z", "three"),
		line("u4", "2026-05-24T10:03:00Z", "four"),
	)

	rep, err := CompactTranscripts(context.Background(), store, CompactOptions{}) // Apply=false
	if err != nil {
		t.Fatalf("CompactTranscripts: %v", err)
	}
	if !rep.DryRun {
		t.Error("report must be marked DryRun")
	}
	if len(rep.Prefixes) != 1 {
		t.Fatalf("want 1 prefix report, got %d", len(rep.Prefixes))
	}
	pr := rep.Prefixes[0]
	if pr.ReclaimBytes <= 0 || rep.TotalReclaim != pr.ReclaimBytes {
		t.Errorf("want positive reclaimable bytes, got %d (total %d)", pr.ReclaimBytes, rep.TotalReclaim)
	}
	if pr.MergedLines != 4 {
		t.Errorf("merged superset should be 4 distinct lines, got %d", pr.MergedLines)
	}
	if len(store.puts) != 0 || len(store.removed) != 0 {
		t.Errorf("dry-run must not mutate: puts=%v removed=%v", store.puts, store.removed)
	}
	if pr.Applied {
		t.Error("dry-run must not mark Applied")
	}
}

// TestCompactTranscripts_ApplyMergesAndDeletes is AC#3: --apply merges to the
// superset and deletes the redundant copies (write-then-delete).
func TestCompactTranscripts_ApplyMergesAndDeletes(t *testing.T) {
	store := newFakeStore()
	store.objects[dayPrefix+"0001-a.ndjson"] = ndjson(
		line("u1", "2026-05-24T10:00:00Z", "one"),
		line("u2", "2026-05-24T10:01:00Z", "two"),
	)
	store.objects[dayPrefix+"0002-b.ndjson"] = ndjson(
		line("u1", "2026-05-24T10:00:00Z", "one"),
		line("u2", "2026-05-24T10:01:00Z", "two"),
		line("u3", "2026-05-24T10:02:00Z", "three"),
	)

	rep, err := CompactTranscripts(context.Background(), store, CompactOptions{Apply: true})
	if err != nil {
		t.Fatalf("CompactTranscripts: %v", err)
	}
	if rep.DryRun {
		t.Error("apply run must not be DryRun")
	}
	pr := rep.Prefixes[0]
	if !pr.Applied || pr.Skipped {
		t.Fatalf("expected Applied (not skipped): %+v", pr)
	}
	// Both source objects deleted; exactly the merged object remains.
	mergedKey := dayPrefix + compactedObjectName
	if _, ok := store.objects[mergedKey]; !ok {
		t.Fatalf("merged object %s must exist after apply", mergedKey)
	}
	remaining := 0
	for k := range store.objects {
		remaining++
		if k != mergedKey {
			t.Errorf("unexpected leftover source object %s", k)
		}
	}
	if remaining != 1 {
		t.Errorf("want exactly the merged object remaining, got %d objects", remaining)
	}
	// write happened before deletes (write-then-delete).
	if len(store.puts) != 1 || store.puts[0] != mergedKey {
		t.Errorf("want one put of the merged key, got %v", store.puts)
	}
	if len(store.removed) != 2 {
		t.Errorf("want both sources removed, got %v", store.removed)
	}
}

// TestCompactTranscripts_PreservesCompletenessAndOrdering is AC#4: after
// compaction the merged content is the deduped superset, in timestamp order,
// with no message lost.
func TestCompactTranscripts_PreservesCompletenessAndOrdering(t *testing.T) {
	store := newFakeStore()
	// Out-of-order across objects to exercise the merge sort.
	store.objects[dayPrefix+"0001-a.ndjson"] = ndjson(
		line("u3", "2026-05-24T10:02:00Z", "three"),
		line("u1", "2026-05-24T10:00:00Z", "one"),
	)
	store.objects[dayPrefix+"0002-b.ndjson"] = ndjson(
		line("u1", "2026-05-24T10:00:00Z", "one"), // dup of A
		line("u2", "2026-05-24T10:01:00Z", "two"),
		line("u4", "2026-05-24T10:03:00Z", "four"),
	)

	if _, err := CompactTranscripts(context.Background(), store, CompactOptions{Apply: true}); err != nil {
		t.Fatalf("CompactTranscripts: %v", err)
	}
	merged := store.objects[dayPrefix+compactedObjectName]
	got := markersIn(t, merged)
	want := []string{"one", "two", "three", "four"} // deduped, timestamp-ordered
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("merged content = %v, want %v (deduped + ordered, no loss)", got, want)
	}
}

// TestCompactTranscripts_StaysWithinCaps is AC#5: a prefix whose distinct content
// exceeds the line cap is SKIPPED (never written lossy), and nothing is mutated.
func TestCompactTranscripts_StaysWithinCaps(t *testing.T) {
	store := newFakeStore()
	var a, b []string
	for i := 0; i < 50; i++ {
		l := line(fmt.Sprintf("u%03d", i), fmt.Sprintf("2026-05-24T10:%02d:00Z", i), fmt.Sprintf("m%d", i))
		a = append(a, l)
		b = append(b, l) // b re-ships a entirely
	}
	store.objects[dayPrefix+"0001-a.ndjson"] = ndjson(a...)
	store.objects[dayPrefix+"0002-b.ndjson"] = ndjson(b...)

	rep, err := CompactTranscripts(context.Background(), store, CompactOptions{Apply: true, MaxLines: 10})
	if err != nil {
		t.Fatalf("CompactTranscripts: %v", err)
	}
	pr := rep.Prefixes[0]
	if !pr.Skipped {
		t.Errorf("prefix exceeding the cap must be Skipped, got %+v", pr)
	}
	if pr.Applied {
		t.Error("a skipped prefix must not be Applied")
	}
	if len(store.puts) != 0 || len(store.removed) != 0 {
		t.Errorf("a capped prefix must not mutate: puts=%v removed=%v", store.puts, store.removed)
	}
}

// TestCompactTranscripts_SingleObjectNoOp: a prefix with one object has no
// cross-object redundancy, so it's omitted from the report and never mutated.
func TestCompactTranscripts_SingleObjectNoOp(t *testing.T) {
	store := newFakeStore()
	store.objects[dayPrefix+"0001-a.ndjson"] = ndjson(
		line("u1", "2026-05-24T10:00:00Z", "one"),
	)
	rep, err := CompactTranscripts(context.Background(), store, CompactOptions{Apply: true})
	if err != nil {
		t.Fatalf("CompactTranscripts: %v", err)
	}
	if len(rep.Prefixes) != 0 {
		t.Errorf("single-object prefix should not be reported, got %+v", rep.Prefixes)
	}
	if len(store.puts) != 0 || len(store.removed) != 0 {
		t.Errorf("single-object prefix must not mutate")
	}
}

// TestCompactTranscripts_PreservesIdlessLines: a line carrying no stable id can
// never be a known duplicate, so it is always kept (completeness).
func TestCompactTranscripts_PreservesIdlessLines(t *testing.T) {
	store := newFakeStore()
	// An inner record with a content marker but NO stable id (no uuid/message.id).
	idless := fmt.Sprintf(`{"timestamp":%q,"message":%q}`, "2026-05-24T10:05:00Z", `{"text":"noid"}`)
	store.objects[dayPrefix+"0001-a.ndjson"] = ndjson(
		line("u1", "2026-05-24T10:00:00Z", "one"),
		idless,
	)
	store.objects[dayPrefix+"0002-b.ndjson"] = ndjson(
		line("u1", "2026-05-24T10:00:00Z", "one"), // dup
		idless, // id-less: kept again (can't dedup)
		line("u2", "2026-05-24T10:06:00Z", "two"),
	)
	if _, err := CompactTranscripts(context.Background(), store, CompactOptions{Apply: true}); err != nil {
		t.Fatalf("CompactTranscripts: %v", err)
	}
	got := markersIn(t, store.objects[dayPrefix+compactedObjectName])
	// u1 deduped to one; both id-less "noid" lines preserved; u2 kept.
	noid := 0
	for _, x := range got {
		if x == "noid" {
			noid++
		}
	}
	if noid != 2 {
		t.Errorf("id-less lines must be preserved (never collapsed); got %d 'noid' lines in %v", noid, got)
	}
}

// TestDayPrefixOf pins the <agent>/<date>/ grouping.
func TestDayPrefixOf(t *testing.T) {
	dp, ok := dayPrefixOf("transcripts/", "transcripts/dave/2026-05-24/0001-a.ndjson")
	if !ok || dp != "transcripts/dave/2026-05-24/" {
		t.Errorf("dayPrefixOf = %q, %v; want transcripts/dave/2026-05-24/, true", dp, ok)
	}
	if _, ok := dayPrefixOf("transcripts/", "transcripts/dave/onlyfile"); ok {
		t.Error("a key without <agent>/<date>/<file> depth must not group")
	}
}
