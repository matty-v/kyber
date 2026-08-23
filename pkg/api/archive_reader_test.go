package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

// readLines runs a reader's windowed read and returns just the lines, failing
// the test on error. The truncation flag is exercised separately by the cap
// tests; the window/isolation tests only care about which lines come back.
func readLines(t *testing.T, r ArchiveReader, agent string, since, until time.Time) []LogLine {
	t.Helper()
	res, err := r.ReadAgentLines(context.Background(), agent, since, until)
	if err != nil {
		t.Fatalf("ReadAgentLines: %v", err)
	}
	return res.Lines
}

// TestObjectPrefix pins the per-agent isolation boundary: the key prefix for an
// agent is exactly "agents/<agent>/", and one agent's prefix is never a prefix
// of another's object keys.
func TestObjectPrefix(t *testing.T) {
	// Empty rootPrefix must default to the archive root "agents/" — this is
	// what keeps source=archive byte-for-byte unchanged after the rootPrefix
	// parameterization (kyber#446).
	if got := objectPrefix("", "dave"); got != "agents/dave/" {
		t.Errorf("objectPrefix(\"\", dave) = %q, want agents/dave/ (default root)", got)
	}
	// "dave" must not be able to read "dave2"'s objects: dave's prefix
	// (agents/dave/) is NOT a prefix of agents/dave2/... because of the
	// trailing slash.
	davePrefix := objectPrefix("", "dave")
	dave2Key := "agents/dave2/2026-06-03/line.ndjson"
	if len(dave2Key) >= len(davePrefix) && dave2Key[:len(davePrefix)] == davePrefix {
		t.Errorf("agent dave's prefix %q must not match dave2's key %q", davePrefix, dave2Key)
	}
}

// TestNormalizeS3Endpoint pins the endpoint normalization: minio-go wants a
// bare host[:port], but the chart shares a schemed URL (scheme selects TLS)
// with the Vector shipper. A leading scheme must be stripped and drive TLS;
// a bare endpoint must fall back to the explicit useTLS flag. This is the
// regression guard for #437: "http://host:9000" previously reached minio.New
// verbatim and failed with "Endpoint url cannot have fully qualified paths."
func TestNormalizeS3Endpoint(t *testing.T) {
	cases := []struct {
		name       string
		endpoint   string
		useTLS     bool
		wantHost   string
		wantUseTLS bool
	}{
		{"http scheme forces plaintext", "http://kyber-minio.kyber-system.svc:9000", true, "kyber-minio.kyber-system.svc:9000", false},
		{"https scheme forces TLS", "https://s3.amazonaws.com", false, "s3.amazonaws.com", true},
		{"bare host keeps useTLS=false", "kyber-minio.kyber-system.svc:9000", false, "kyber-minio.kyber-system.svc:9000", false},
		{"bare host keeps useTLS=true", "s3.amazonaws.com", true, "s3.amazonaws.com", true},
		{"trailing slash trimmed", "http://host:9000/", true, "host:9000", false},
		{"unknown scheme leaves useTLS untouched", "ftp://host:21", true, "host:21", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotHost, gotTLS := normalizeS3Endpoint(tc.endpoint, tc.useTLS)
			if gotHost != tc.wantHost {
				t.Errorf("host = %q, want %q", gotHost, tc.wantHost)
			}
			if gotTLS != tc.wantUseTLS {
				t.Errorf("useTLS = %v, want %v", gotTLS, tc.wantUseTLS)
			}
		})
	}
}

// TestParseArchiveLine verifies single-line NDJSON parsing: a good line parses,
// while blank, malformed, and timestamp-less lines are rejected (ok=false) so
// they're skipped — they can't be placed in an absolute window.
func TestParseArchiveLine(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantOK   bool
		wantText string
		wantTS   string
	}{
		{"good line", `{"timestamp":"2026-06-03T10:00:00Z","message":"first"}`, true, "first", "2026-06-03T10:00:00Z"},
		{"leading/trailing space trimmed", "  " + `{"timestamp":"2026-06-03T10:00:01Z","message":"second"}` + "  ", true, "second", "2026-06-03T10:00:01Z"},
		{"blank", "   ", false, "", ""},
		{"malformed json", "not json", false, "", ""},
		{"missing timestamp", `{"message":"no timestamp"}`, false, "", ""},
		{"non-rfc3339 timestamp", `{"timestamp":"june third","message":"x"}`, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseArchiveLine(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.Text != tc.wantText {
				t.Errorf("text = %q, want %q", got.Text, tc.wantText)
			}
			if !got.Timestamp.Equal(mustTime(t, tc.wantTS)) {
				t.Errorf("timestamp = %v, want %v", got.Timestamp, tc.wantTS)
			}
		})
	}
}

// TestWindowScanner_InclusiveWindow verifies the streaming scanner filters each
// line to the inclusive window [since, until] as it reads — the per-line
// equivalent of the old post-hoc filterToWindow, but bounded.
func TestWindowScanner_InclusiveWindow(t *testing.T) {
	since := mustTime(t, "2026-06-03T10:00:00Z")
	until := mustTime(t, "2026-06-03T11:00:00Z")
	ws := newWindowScanner(since, until, scannerCaps{}, false)
	data := `{"timestamp":"2026-06-03T09:59:59Z","message":"before"}` + "\n" +
		`{"timestamp":"2026-06-03T10:00:00Z","message":"at-since"}` + "\n" +
		`{"timestamp":"2026-06-03T10:30:00Z","message":"inside"}` + "\n" +
		`{"timestamp":"2026-06-03T11:00:00Z","message":"at-until"}` + "\n" +
		`{"timestamp":"2026-06-03T11:00:01Z","message":"after"}`
	if err := ws.scan(strings.NewReader(data)); err != nil {
		t.Fatalf("scan: %v", err)
	}
	res := ws.result()
	if res.Truncated {
		t.Errorf("did not expect truncation under the default cap")
	}
	var texts []string
	for _, l := range res.Lines {
		texts = append(texts, l.Text)
	}
	want := []string{"at-since", "inside", "at-until"}
	if strings.Join(texts, ",") != strings.Join(want, ",") {
		t.Errorf("window lines = %v, want %v", texts, want)
	}
}

// TestWindowScanner_LineCapTruncates verifies the scanner bounds the returned
// slice to the line cap regardless of how many in-window lines exist and reports
// Truncated (kyber#455 AC#4/#5), and that the lines it keeps are the NEWEST ones
// (kyber#669) — the panel's whole purpose is "what happened lately", so a capped
// read that returned a week-old prefix was worse than useless.
func TestWindowScanner_LineCapTruncates(t *testing.T) {
	since := mustTime(t, "2026-06-03T00:00:00Z")
	until := mustTime(t, "2026-06-03T23:59:59Z")
	ws := newWindowScanner(since, until, scannerCaps{maxLines: 3}, false) // cap at 3 lines
	var b strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, `{"timestamp":"2026-06-03T10:00:%02dZ","message":"line-%d"}`+"\n", i, i)
	}
	if err := ws.scan(strings.NewReader(b.String())); err != nil {
		t.Fatalf("scan: %v", err)
	}
	res := ws.result()
	if !res.Truncated {
		t.Errorf("want Truncated=true when more in-window lines than the cap")
	}
	if len(res.Lines) != 3 {
		t.Fatalf("want exactly 3 lines (the cap), got %d", len(res.Lines))
	}
	var texts []string
	for _, l := range res.Lines {
		texts = append(texts, l.Text)
	}
	// Ascending order is preserved; the survivors are the last three emitted.
	want := "line-7,line-8,line-9"
	if got := strings.Join(texts, ","); got != want {
		t.Errorf("capped read kept %q, want the newest %q", got, want)
	}
}

// TestWindowScanner_ReturnedByteCapKeepsNewest verifies the returned-byte cap
// (kyber#669) bounds the RESPONSE, not merely the line count, and evicts
// oldest-first. This is the cap that a line count alone could not provide: on
// a production cluster a 50k-line transcript read was an 84.7 MB body because individual
// lines ranged from 850 B to 877 KB.
func TestWindowScanner_ReturnedByteCapKeepsNewest(t *testing.T) {
	since := mustTime(t, "2026-06-03T00:00:00Z")
	until := mustTime(t, "2026-06-03T23:59:59Z")
	// Line cap deliberately generous so the BYTE cap is the binding constraint.
	const budget = 400
	ws := newWindowScanner(since, until, scannerCaps{maxLines: 1000, maxReturnBytes: budget}, false)

	var b strings.Builder
	fat := strings.Repeat("x", 120) // each line is well over budget/10
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, `{"timestamp":"2026-06-03T10:00:%02dZ","message":"%s-%02d"}`+"\n", i, fat, i)
	}
	if err := ws.scan(strings.NewReader(b.String())); err != nil {
		t.Fatalf("scan: %v", err)
	}
	res := ws.result()

	if !res.Truncated {
		t.Fatal("want Truncated=true when in-window bytes exceed the returned-byte cap")
	}
	var total int64
	for _, l := range res.Lines {
		total += int64(len(l.Text)) + 1
	}
	if total > budget {
		t.Errorf("returned %d bytes, want <= the %d-byte cap", total, budget)
	}
	if len(res.Lines) == 0 {
		t.Fatal("want a non-empty result under the byte cap")
	}
	// The survivors must be a suffix of the emitted order: newest kept, oldest dropped.
	if !strings.HasSuffix(res.Lines[len(res.Lines)-1].Text, "-19") {
		t.Errorf("newest retained line = %q, want the last emitted (…-19)", res.Lines[len(res.Lines)-1].Text)
	}
}

// TestWindowScanner_EvictionKeepsSeenBounded verifies the kyber#454 dedup bound
// survives kyber#669's eviction: `seen` is rebuilt from the survivors on each
// compaction rather than growing with every line ever scanned, and a re-ship of
// an already-evicted line never appears twice in the retained set.
func TestWindowScanner_EvictionKeepsSeenBounded(t *testing.T) {
	since := mustTime(t, "2026-06-03T00:00:00Z")
	until := mustTime(t, "2026-06-03T23:59:59Z")
	const maxLines = 4
	ws := newWindowScanner(since, until, scannerCaps{maxLines: maxLines}, true) // dedup ON

	// Emit far more distinct ids than the cap, then re-ship the whole run — the
	// cumulative re-ship pattern the transcript-tailer actually produces. The
	// dedup id lives INSIDE the shipped envelope's `message` payload, not on the
	// envelope itself (see parseArchiveLine / extractStableID).
	line := func(i int) string {
		return transcriptNDJSON(t,
			fmt.Sprintf("2026-06-03T10:00:%02dZ", i),
			fmt.Sprintf(`{"uuid":"id-%02d","text":"m-%02d"}`, i, i),
		) + "\n"
	}
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString(line(i))
	}
	for i := 0; i < 30; i++ {
		b.WriteString(line(i))
	}
	if err := ws.scan(strings.NewReader(b.String())); err != nil {
		t.Fatalf("scan: %v", err)
	}
	res := ws.result()

	if len(ws.seen) > maxLines {
		t.Errorf("|seen| = %d, want <= maxLines (%d) — the #454 memory bound must survive eviction", len(ws.seen), maxLines)
	}
	if len(res.Lines) != maxLines {
		t.Fatalf("want exactly %d retained lines, got %d", maxLines, len(res.Lines))
	}
	seenText := map[string]bool{}
	for _, l := range res.Lines {
		if seenText[l.Text] {
			t.Errorf("line %q appears twice in the retained set", l.Text)
		}
		seenText[l.Text] = true
	}
	if !strings.Contains(res.Lines[len(res.Lines)-1].Text, `"id-29"`) {
		t.Errorf("newest retained line = %q, want the id-29 record", res.Lines[len(res.Lines)-1].Text)
	}
}

// TestWindowScanner_EvictionDoesNotStopTheRead is the regression guard for the
// bug that would make kyber#669 pointless: eviction must not set done(). If it
// did, the reader would stop feeding objects at the first compaction and never
// reach the newest day — reproducing the exact staleness the fix removes.
func TestWindowScanner_EvictionDoesNotStopTheRead(t *testing.T) {
	since := mustTime(t, "2026-06-03T00:00:00Z")
	until := mustTime(t, "2026-06-03T23:59:59Z")
	ws := newWindowScanner(since, until, scannerCaps{maxLines: 2}, false)

	var b strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, `{"timestamp":"2026-06-03T10:00:%02dZ","message":"line-%d"}`+"\n", i, i)
	}
	if err := ws.scan(strings.NewReader(b.String())); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if ws.done() {
		t.Fatal("done() must stay false after retention-cap eviction — only the scan-byte cap halts a read")
	}
	// A second object still contributes, and its newer lines win.
	if err := ws.scan(strings.NewReader(
		`{"timestamp":"2026-06-03T11:00:00Z","message":"newest"}` + "\n")); err != nil {
		t.Fatalf("scan: %v", err)
	}
	res := ws.result()
	if len(res.Lines) == 0 || res.Lines[len(res.Lines)-1].Text != "newest" {
		t.Errorf("want the later object's newest line retained, got %+v", res.Lines)
	}
}

// TestWindowScanner_ByteCapTruncates verifies the scanner stops once the
// scanned-byte cap is exceeded and reports Truncated, bounding work for a wide
// window even when few lines fall in-window.
func TestWindowScanner_ByteCapTruncates(t *testing.T) {
	since := mustTime(t, "2026-06-03T00:00:00Z")
	until := mustTime(t, "2026-06-03T23:59:59Z")
	// A tiny byte cap (200 bytes) against ~10 lines well over that total.
	ws := newWindowScanner(since, until, scannerCaps{maxScanBytes: 200}, false)
	var b strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, `{"timestamp":"2026-06-03T10:00:%02dZ","message":"line-%d"}`+"\n", i, i)
	}
	if err := ws.scan(strings.NewReader(b.String())); err != nil {
		t.Fatalf("scan: %v", err)
	}
	res := ws.result()
	if !res.Truncated {
		t.Errorf("want Truncated=true when scanned bytes exceed the byte cap")
	}
}

// TestReadFromObjects_WindowAndOrdering verifies the storage-independent core
// excludes out-of-window lines and returns the rest sorted ascending by time,
// across multiple objects.
func TestReadFromObjects_WindowAndOrdering(t *testing.T) {
	objects := map[string][]byte{
		"agents/dave/2026-06-03/a.ndjson": []byte(
			`{"timestamp":"2026-06-03T10:30:00Z","message":"mid"}` + "\n" +
				`{"timestamp":"2026-06-03T09:00:00Z","message":"too-early"}`),
		"agents/dave/2026-06-03/b.ndjson": []byte(
			`{"timestamp":"2026-06-03T10:00:00Z","message":"start"}` + "\n" +
				`{"timestamp":"2026-06-03T12:00:00Z","message":"too-late"}`),
	}
	since := mustTime(t, "2026-06-03T10:00:00Z")
	until := mustTime(t, "2026-06-03T11:00:00Z")

	got, err := readFromObjects(objects, "", "dave", since, until)
	if err != nil {
		t.Fatalf("readFromObjects: %v", err)
	}
	want := []string{"start", "mid"} // sorted ascending, out-of-window excluded
	if len(got) != len(want) {
		t.Fatalf("want %v, got %+v", want, got)
	}
	for i := range want {
		if got[i].Text != want[i] {
			t.Errorf("line[%d] = %q, want %q", i, got[i].Text, want[i])
		}
	}
}

// fakeS3Store is an in-memory s3ObjectStore for S3ArchiveReader tests. It holds
// objects keyed exactly like the real store (full object keys) and answers
// listKeys by prefix and getObject by exact key — the same surface the real
// minio-backed store exposes, so the reader's list+fetch+delegate logic is
// exercised without a live MinIO. It records the prefixes listed so a test can
// assert the reader bounds listing to the window's day-partitions.
type fakeS3Store struct {
	objects map[string][]byte
	// listedPrefixes records every prefix passed to listKeys, in call order.
	listedPrefixes []string
	// listErr / getErr, when non-nil, make the corresponding op fail — used to
	// verify the reader propagates storage errors (it must not swallow them).
	listErr error
	getErr  error
	// openNow / maxOpen track concurrently-open object readers. The memory-bound
	// AC (kyber#455) requires the reader hold at most one object's bytes resident
	// at a time: it must fetch+scan+Close one object before opening the next, so
	// maxOpen must stay 1 across a multi-object read.
	openNow int
	maxOpen int
}

// trackedReadCloser wraps an object's bytes as a stream and decrements the
// store's open-reader count on Close, so a test can assert the reader never
// holds more than one object open at once.
type trackedReadCloser struct {
	io.Reader
	store  *fakeS3Store
	closed bool
}

func (t *trackedReadCloser) Close() error {
	if !t.closed {
		t.closed = true
		t.store.openNow--
	}
	return nil
}

func (f *fakeS3Store) listKeys(_ context.Context, prefix string) ([]string, error) {
	f.listedPrefixes = append(f.listedPrefixes, prefix)
	if f.listErr != nil {
		return nil, f.listErr
	}
	var keys []string
	for k := range f.objects {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys) // deterministic order for assertions
	return keys, nil
}

func (f *fakeS3Store) getObject(_ context.Context, key string) (io.ReadCloser, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	data, ok := f.objects[key]
	if !ok {
		return nil, errors.New("not found: " + key)
	}
	f.openNow++
	if f.openNow > f.maxOpen {
		f.maxOpen = f.openNow
	}
	return &trackedReadCloser{Reader: bytes.NewReader(data), store: f}, nil
}

// TestS3ArchiveReader_WindowAndOrdering verifies the S3 reader returns the
// agent's lines for the absolute window, sorted ascending, with out-of-window
// lines excluded — i.e. the same window contract as GCS, via the shared core.
func TestS3ArchiveReader_WindowAndOrdering(t *testing.T) {
	store := &fakeS3Store{objects: map[string][]byte{
		"agents/dave/2026-06-03/a.ndjson": []byte(
			`{"timestamp":"2026-06-03T10:30:00Z","message":"mid"}` + "\n" +
				`{"timestamp":"2026-06-03T09:00:00Z","message":"too-early"}`),
		"agents/dave/2026-06-03/b.ndjson": []byte(
			`{"timestamp":"2026-06-03T10:00:00Z","message":"start"}` + "\n" +
				`{"timestamp":"2026-06-03T12:00:00Z","message":"too-late"}`),
	}}
	r := &S3ArchiveReader{store: store, bucket: "logs"}

	since := mustTime(t, "2026-06-03T10:00:00Z")
	until := mustTime(t, "2026-06-03T11:00:00Z")
	got := readLines(t, r, "dave", since, until)
	want := []string{"start", "mid"} // ascending, out-of-window excluded
	if len(got) != len(want) {
		t.Fatalf("want %v, got %+v", want, got)
	}
	for i := range want {
		if got[i].Text != want[i] {
			t.Errorf("line[%d] = %q, want %q", i, got[i].Text, want[i])
		}
	}
}

func TestS3ArchiveReader_GenericContainerIsolation(t *testing.T) {
	store := &fakeS3Store{objects: map[string][]byte{
		"logs/agent/sol/uid-1/agent/2026-06-03/a.ndjson":                []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"wanted"}`),
		"logs/agent/sol/uid-2/agent/2026-06-03/a.ndjson":                []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"stale-pod"}`),
		"logs/agent/sol/uid-1/kyber-status-sidecar/2026-06-03/a.ndjson": []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"other-container"}`),
	}}
	r := &S3ArchiveReader{store: store, bucket: "logs"}
	result, err := r.ReadContainerLines(context.Background(), GenericArchiveSelection{
		Component: "agent", Workload: "sol", PodUID: "uid-1", Container: "agent",
	}, time.Date(2026, 6, 3, 9, 0, 0, 0, time.UTC), time.Date(2026, 6, 3, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ReadContainerLines: %v", err)
	}
	if len(result.Lines) != 1 || result.Lines[0].Text != "wanted" {
		t.Fatalf("lines = %+v, want only wanted", result.Lines)
	}
	wantPrefix := "logs/agent/sol/uid-1/agent/2026-06-03/"
	if len(store.listedPrefixes) != 1 || store.listedPrefixes[0] != wantPrefix {
		t.Errorf("listed prefixes = %v, want [%s]", store.listedPrefixes, wantPrefix)
	}
}

func TestGenericArchiveRejectsUnsafeSegments(t *testing.T) {
	_, err := genericDayPartitionPrefixes(GenericArchiveSelection{
		Component: "agent", Workload: "../other", PodUID: "uid-1", Container: "agent",
	}, time.Now(), time.Now())
	if err == nil {
		t.Fatal("expected unsafe workload to be rejected")
	}
}

func TestS3ArchiveReaderStreamsGenericRecordsOneObjectAtATime(t *testing.T) {
	store := &fakeS3Store{objects: map[string][]byte{
		"logs/agent/sol/uid-1/agent/2026-06-03/a.ndjson": []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"one"}`),
		"logs/agent/sol/uid-1/agent/2026-06-03/b.ndjson": []byte(`{"timestamp":"2026-06-03T10:01:00Z","message":"two"}`),
	}}
	r := &S3ArchiveReader{store: store, bucket: "logs"}
	var messages []string
	err := r.StreamContainerRecords(context.Background(), GenericArchiveSelection{
		Component: "agent", Workload: "sol", PodUID: "uid-1", Container: "agent",
	}, time.Date(2026, 6, 3, 9, 0, 0, 0, time.UTC), time.Date(2026, 6, 3, 11, 0, 0, 0, time.UTC), func(_ string, line LogLine) error {
		messages = append(messages, line.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamContainerRecords: %v", err)
	}
	if got := strings.Join(messages, ","); got != "one,two" {
		t.Errorf("messages = %q, want one,two", got)
	}
	if store.maxOpen != 1 {
		t.Errorf("max open objects = %d, want 1", store.maxOpen)
	}
}

// TestS3ArchiveReader_EmptyWindows is the regression for the failure mode caught
// during #431 verification: a future-dated window and a pre-shipper window each
// return EMPTY — not the same lines as a recent window.
func TestS3ArchiveReader_EmptyWindows(t *testing.T) {
	store := &fakeS3Store{objects: map[string][]byte{
		"agents/dave/2026-06-03/a.ndjson": []byte(
			`{"timestamp":"2026-06-03T10:30:00Z","message":"real-line"}`),
	}}
	r := &S3ArchiveReader{store: store, bucket: "logs"}

	cases := []struct {
		name         string
		since, until string
	}{
		{"future window", "2026-06-05T00:00:00Z", "2026-06-05T23:59:59Z"},
		{"pre-shipper window", "2026-06-01T00:00:00Z", "2026-06-01T23:59:59Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := readLines(t, r, "dave", mustTime(t, tc.since), mustTime(t, tc.until))
			if len(got) != 0 {
				t.Errorf("want empty for %s, got %+v", tc.name, got)
			}
		})
	}
}

// TestS3ArchiveReader_AgentIsolation is the multi-tenant isolation AC against
// the S3 reader: a read for agent-X over a store that also holds agent-Y (and a
// look-alike agent-X2) objects returns ZERO other-agent lines. Isolation holds
// by construction: the reader lists strictly under agents/<agent>/.
func TestS3ArchiveReader_AgentIsolation(t *testing.T) {
	store := &fakeS3Store{objects: map[string][]byte{
		"agents/dave/2026-06-03/a.ndjson":  []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"dave-line"}`),
		"agents/luke/2026-06-03/a.ndjson":  []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"luke-line"}`),
		"agents/dave2/2026-06-03/a.ndjson": []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"dave2-line"}`),
	}}
	r := &S3ArchiveReader{store: store, bucket: "logs"}

	got := readLines(t, r, "dave",
		mustTime(t, "2026-06-03T00:00:00Z"), mustTime(t, "2026-06-03T23:59:59Z"))
	if len(got) != 1 {
		t.Fatalf("want exactly 1 line (dave's), got %d: %+v", len(got), got)
	}
	if got[0].Text != "dave-line" {
		t.Errorf("isolation breach: got %q, want dave-line", got[0].Text)
	}
	// Every prefix the reader listed must be under dave's prefix only — proving
	// it never even enumerates another agent's objects.
	for _, p := range store.listedPrefixes {
		if want := "agents/dave/"; len(p) < len(want) || p[:len(want)] != want {
			t.Errorf("reader listed a prefix outside agents/dave/: %q", p)
		}
	}
}

// TestS3ArchiveReader_ListsByDayPartition verifies the reader bounds listing to
// exactly the day-partition prefixes the window spans (one per UTC day), not the
// agent's whole history — the same window-bounded list-cost invariant as GCS —
// and that it visits them NEWEST DAY FIRST (kyber#669), so a read halted by the
// scan-byte cap has spent its budget on recent days rather than stale ones.
func TestS3ArchiveReader_ListsByDayPartition(t *testing.T) {
	store := &fakeS3Store{objects: map[string][]byte{}}
	r := &S3ArchiveReader{store: store, bucket: "logs"}

	// A 2-day window (inclusive) → two day-partition prefixes.
	_, err := r.ReadAgentLines(context.Background(), "dave",
		mustTime(t, "2026-06-03T22:00:00Z"), mustTime(t, "2026-06-04T02:00:00Z"))
	if err != nil {
		t.Fatalf("ReadAgentLines: %v", err)
	}
	want := []string{"agents/dave/2026-06-04/", "agents/dave/2026-06-03/"}
	if len(store.listedPrefixes) != len(want) {
		t.Fatalf("want %d listed prefixes %v, got %v", len(want), want, store.listedPrefixes)
	}
	for i := range want {
		if store.listedPrefixes[i] != want[i] {
			t.Errorf("listed prefix[%d] = %q, want %q", i, store.listedPrefixes[i], want[i])
		}
	}
}

// TestS3ArchiveReader_PropagatesListError verifies a storage list failure is
// returned (wrapped), never swallowed into a silent empty result.
func TestS3ArchiveReader_PropagatesListError(t *testing.T) {
	sentinel := errors.New("minio list boom")
	store := &fakeS3Store{objects: map[string][]byte{}, listErr: sentinel}
	r := &S3ArchiveReader{store: store, bucket: "logs"}

	_, err := r.ReadAgentLines(context.Background(), "dave",
		mustTime(t, "2026-06-03T10:00:00Z"), mustTime(t, "2026-06-03T11:00:00Z"))
	if err == nil {
		t.Fatal("want error when list fails, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("want wrapped sentinel, got %v", err)
	}
}

// TestS3ArchiveReader_RejectsInvalidAgentName verifies the reader-boundary
// defense-in-depth holds for the S3 path too: a name that could escape the
// prefix is rejected before any store call.
func TestS3ArchiveReader_RejectsInvalidAgentName(t *testing.T) {
	store := &fakeS3Store{objects: map[string][]byte{}}
	r := &S3ArchiveReader{store: store, bucket: "logs"}

	_, err := r.ReadAgentLines(context.Background(), "../etc",
		mustTime(t, "2026-06-03T10:00:00Z"), mustTime(t, "2026-06-03T11:00:00Z"))
	if err == nil {
		t.Fatal("want error for invalid agent name, got nil")
	}
	if len(store.listedPrefixes) != 0 {
		t.Errorf("store must not be listed for invalid agent name; listed %v", store.listedPrefixes)
	}
}

// TestReadFromObjects_AgentIsolation is the multi-tenant isolation AC at the
// logic level: a read for agent-X over a store that also holds agent-Y objects
// returns ZERO agent-Y lines.
func TestReadFromObjects_AgentIsolation(t *testing.T) {
	objects := map[string][]byte{
		"agents/dave/2026-06-03/a.ndjson": []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"dave-line"}`),
		"agents/luke/2026-06-03/a.ndjson": []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"luke-line"}`),
		// A look-alike agent whose name has dave's as a substring must not leak.
		"agents/dave2/2026-06-03/a.ndjson": []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"dave2-line"}`),
	}
	since := mustTime(t, "2026-06-03T00:00:00Z")
	until := mustTime(t, "2026-06-03T23:59:59Z")

	got, err := readFromObjects(objects, "", "dave", since, until)
	if err != nil {
		t.Fatalf("readFromObjects: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly 1 line (dave's), got %d: %+v", len(got), got)
	}
	if got[0].Text != "dave-line" {
		t.Errorf("isolation breach: got %q, want dave-line", got[0].Text)
	}
}

// TestObjectPrefix_TranscriptRoot pins the transcript surface's per-agent
// prefix: with rootPrefix "transcripts/" the key root is "transcripts/<agent>/",
// isolated from the "agents/" archive root so the two object sets never
// intermix (kyber#446 AC).
func TestObjectPrefix_TranscriptRoot(t *testing.T) {
	if got := objectPrefix("transcripts/", "dave"); got != "transcripts/dave/" {
		t.Errorf("objectPrefix(transcripts/, dave) = %q, want transcripts/dave/", got)
	}
	// The transcript prefix must never be a prefix of an archive key and vice
	// versa — distinct roots guarantee the lanes don't intermix.
	if strings.HasPrefix("agents/dave/2026-06-04/x.ndjson", objectPrefix("transcripts/", "dave")) {
		t.Error("transcript prefix must not match an archive (agents/) key")
	}
}

// TestReadFromObjects_TranscriptIsolation verifies that a transcript read
// (rootPrefix "transcripts/") returns ONLY transcript-lane objects and never
// the agent's archive (agents/) objects, even for the same agent — the two
// surfaces are isolated by root prefix.
func TestReadFromObjects_TranscriptIsolation(t *testing.T) {
	objects := map[string][]byte{
		"transcripts/dave/2026-06-03/a.ndjson": []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"transcript-line"}`),
		"agents/dave/2026-06-03/a.ndjson":      []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"archive-line"}`),
	}
	since := mustTime(t, "2026-06-03T00:00:00Z")
	until := mustTime(t, "2026-06-03T23:59:59Z")

	got, err := readFromObjects(objects, "transcripts/", "dave", since, until)
	if err != nil {
		t.Fatalf("readFromObjects: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly 1 transcript line, got %d: %+v", len(got), got)
	}
	if got[0].Text != "transcript-line" {
		t.Errorf("lane breach: got %q, want transcript-line (archive object must not leak)", got[0].Text)
	}
}

// TestS3ArchiveReader_TranscriptRoot_ListsTranscriptPartitions verifies an S3
// reader configured with rootPrefix "transcripts/" lists strictly under the
// transcripts/<agent>/<day>/ partitions (not agents/), returning the
// transcript-lane lines — the same windowing machinery, a different root.
func TestS3ArchiveReader_TranscriptRoot_ListsTranscriptPartitions(t *testing.T) {
	store := &fakeS3Store{objects: map[string][]byte{
		"transcripts/dave/2026-06-03/a.ndjson": []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"t-line"}`),
		"agents/dave/2026-06-03/a.ndjson":      []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"a-line"}`),
	}}
	r := &S3ArchiveReader{store: store, bucket: "logs", rootPrefix: "transcripts/"}

	got := readLines(t, r, "dave",
		mustTime(t, "2026-06-03T00:00:00Z"), mustTime(t, "2026-06-03T23:59:59Z"))
	if len(got) != 1 || got[0].Text != "t-line" {
		t.Fatalf("want only transcript lane line t-line, got %+v", got)
	}
	for _, p := range store.listedPrefixes {
		if !strings.HasPrefix(p, "transcripts/dave/") {
			t.Errorf("transcript reader listed a non-transcript prefix: %q", p)
		}
	}
}

// TestS3ArchiveReader_EmptyRootDefaultsToAgents verifies a struct-literal reader
// with no rootPrefix still serves the archive root — the backward-compat guard
// for source=archive.
func TestS3ArchiveReader_EmptyRootDefaultsToAgents(t *testing.T) {
	store := &fakeS3Store{objects: map[string][]byte{
		"agents/dave/2026-06-03/a.ndjson": []byte(`{"timestamp":"2026-06-03T10:00:00Z","message":"a-line"}`),
	}}
	r := &S3ArchiveReader{store: store, bucket: "logs"} // rootPrefix unset

	got := readLines(t, r, "dave",
		mustTime(t, "2026-06-03T00:00:00Z"), mustTime(t, "2026-06-03T23:59:59Z"))
	if len(got) != 1 || got[0].Text != "a-line" {
		t.Fatalf("empty rootPrefix must default to agents/; got %+v", got)
	}
	for _, p := range store.listedPrefixes {
		if !strings.HasPrefix(p, "agents/dave/") {
			t.Errorf("default reader listed a non-agents prefix: %q", p)
		}
	}
}

// TestS3ArchiveReader_OneObjectResidentAtATime is the core memory-bound AC
// (kyber#455 AC#1/#5): over many objects, the reader must fetch+scan+Close one
// object before opening the next, so it never holds more than one object's bytes
// resident at once. The fake store tracks concurrently-open readers; maxOpen
// must be 1. This is the regression guard for the OOM: the old S3 path built a
// map[string][]byte holding EVERY object at once (maxOpen would equal the object
// count).
func TestS3ArchiveReader_OneObjectResidentAtATime(t *testing.T) {
	objects := map[string][]byte{}
	for i := 0; i < 25; i++ {
		key := fmt.Sprintf("agents/dave/2026-06-03/obj-%02d.ndjson", i)
		objects[key] = []byte(fmt.Sprintf(`{"timestamp":"2026-06-03T10:00:%02dZ","message":"line-%d"}`, i, i))
	}
	store := &fakeS3Store{objects: objects}
	r := &S3ArchiveReader{store: store, bucket: "logs"}

	got := readLines(t, r, "dave",
		mustTime(t, "2026-06-03T00:00:00Z"), mustTime(t, "2026-06-03T23:59:59Z"))
	if len(got) != 25 {
		t.Fatalf("want all 25 in-window lines, got %d", len(got))
	}
	if store.maxOpen != 1 {
		t.Errorf("reader held %d objects open at once; want at most 1 (the OOM regression: whole window must not be resident)", store.maxOpen)
	}
}

// TestS3ArchiveReader_LineCapTruncates verifies the cap + truncation signal
// surface through the real reader (not just the scanner): with the line cap set
// below the in-window line count, ReadAgentLines returns a bounded slice and
// ReadResult.Truncated=true — the additive bounded-read contract (AC#4). It also
// proves the reader stops early: it must not open every object once the cap is
// hit, so maxOpen stays 1 and not all objects are fetched.
func TestS3ArchiveReader_LineCapTruncates(t *testing.T) {
	objects := map[string][]byte{}
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("agents/dave/2026-06-03/obj-%02d.ndjson", i)
		objects[key] = []byte(fmt.Sprintf(`{"timestamp":"2026-06-03T10:00:%02dZ","message":"line-%d"}`, i, i))
	}
	store := &fakeS3Store{objects: objects}
	r := &S3ArchiveReader{store: store, bucket: "logs", caps: scannerCaps{maxLines: 5}}

	res, err := r.ReadAgentLines(context.Background(), "dave",
		mustTime(t, "2026-06-03T00:00:00Z"), mustTime(t, "2026-06-03T23:59:59Z"))
	if err != nil {
		t.Fatalf("ReadAgentLines: %v", err)
	}
	if !res.Truncated {
		t.Errorf("want Truncated=true when in-window lines exceed the cap")
	}
	if len(res.Lines) != 5 {
		t.Fatalf("want exactly 5 lines (the cap), got %d", len(res.Lines))
	}
	if store.maxOpen != 1 {
		t.Errorf("reader held %d objects open at once; want at most 1", store.maxOpen)
	}
}

// --- kyber#454: read-side transcript dedup -----------------------------------

// transcriptNDJSON builds one shipped NDJSON envelope line whose `message`
// carries the raw transcript JSONL `raw` verbatim — the exact shape the read
// path sees (parseArchiveLine lifts `message` into LogLine.Text, and the stable
// id lives inside that raw text). Using json.Marshal keeps the inner quoting
// correct without hand-escaping.
func transcriptNDJSON(t *testing.T, ts, raw string) string {
	t.Helper()
	b, err := json.Marshal(archiveLine{Timestamp: ts, Message: raw})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(b)
}

// assistantLine is a minimal Claude Code assistant transcript line carrying a
// top-level message.id and a usage.output_tokens — the dedup key plus the field
// a cost tally sums (kyber#454 AC#3).
func assistantLine(msgID string, outputTokens int) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":"x","message":{"id":%q,"usage":{"output_tokens":%d}}}`, msgID, outputTokens)
}

// sumOutputTokens sums message.usage.output_tokens across returned lines —
// stands in for a cost consumer (holocron#33) reading the deduped lane.
func sumOutputTokens(t *testing.T, lines []LogLine) int {
	t.Helper()
	var total int
	for _, l := range lines {
		var probe struct {
			Message struct {
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(l.Text), &probe); err != nil {
			continue // non-assistant / id-less line — no usage to sum
		}
		total += probe.Message.Usage.OutputTokens
	}
	return total
}

// TestExtractStableID pins the id-extraction precedence the dedup keys on:
// message.id first, then uuid, then leafUuid; an id-less or malformed line
// returns ok=false so it is preserved, never collapsed (kyber#454 AC#2).
func TestExtractStableID(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		wantID string
		wantOK bool
	}{
		{"message.id wins over uuid/leafUuid", `{"message":{"id":"msg_01"},"uuid":"u1","leafUuid":"l1"}`, "msg_01", true},
		{"uuid when no message.id", `{"type":"user","message":{"role":"user"},"uuid":"u2","leafUuid":"l2"}`, "u2", true},
		{"leafUuid when no message.id/uuid", `{"type":"summary","leafUuid":"l3"}`, "l3", true},
		{"no id fields", `{"type":"system","content":"boot"}`, "", false},
		{"empty message.id falls back to uuid", `{"message":{"id":""},"uuid":"u4"}`, "u4", true},
		{"malformed json", `not json`, "", false},
		{"empty object", `{}`, "", false},
		{"non-object message (string) is preserved", `{"message":"hi","uuid":"u5"}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotOK := extractStableID(tc.raw)
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v (id=%q)", gotOK, tc.wantOK, gotID)
			}
			if gotID != tc.wantID {
				t.Errorf("id = %q, want %q", gotID, tc.wantID)
			}
		})
	}
}

// TestReadFromObjects_TranscriptDedup_CumulativeReship is the core AC (#1/#6):
// object B is object A's lines plus new lines — the cumulative-reship shape the
// tailer produces on restart. After a transcript-root read each stable id
// appears exactly once, in ascending timestamp order, with nothing distinct
// dropped.
func TestReadFromObjects_TranscriptDedup_CumulativeReship(t *testing.T) {
	objects := map[string][]byte{
		// Object A: the original session prefix.
		"transcripts/dave/2026-06-03/a.ndjson": []byte(strings.Join([]string{
			transcriptNDJSON(t, "2026-06-03T10:00:00Z", assistantLine("msg_01", 10)),
			transcriptNDJSON(t, "2026-06-03T10:01:00Z", assistantLine("msg_02", 20)),
		}, "\n")),
		// Object B: A re-shipped from line 1 PLUS one new message — exactly what
		// `tail -n +1 -F` writes on a sidecar restart.
		"transcripts/dave/2026-06-03/b.ndjson": []byte(strings.Join([]string{
			transcriptNDJSON(t, "2026-06-03T10:00:00Z", assistantLine("msg_01", 10)),
			transcriptNDJSON(t, "2026-06-03T10:01:00Z", assistantLine("msg_02", 20)),
			transcriptNDJSON(t, "2026-06-03T10:02:00Z", assistantLine("msg_03", 30)),
		}, "\n")),
	}
	since := mustTime(t, "2026-06-03T00:00:00Z")
	until := mustTime(t, "2026-06-03T23:59:59Z")

	got, err := readFromObjects(objects, "transcripts/", "dave", since, until)
	if err != nil {
		t.Fatalf("readFromObjects: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 deduped lines, got %d: %+v", len(got), got)
	}
	// Each id exactly once, ascending by timestamp.
	wantIDs := []string{"msg_01", "msg_02", "msg_03"}
	for i, want := range wantIDs {
		id, ok := extractStableID(got[i].Text)
		if !ok || id != want {
			t.Errorf("line[%d] id = %q (ok=%v), want %q", i, id, ok, want)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].Timestamp.Before(got[i-1].Timestamp) {
			t.Errorf("timestamp order broken at %d: %v before %v", i, got[i].Timestamp, got[i-1].Timestamp)
		}
	}
}

// TestReadFromObjects_TranscriptDedup_CostSum is AC#3: a per-agent token sum
// over the deduped transcript lane matches the unique figure — no ~2-2.5x
// inflation from re-shipped duplicates. The reference repro shape: the same
// records shipped 2-3x must sum once.
func TestReadFromObjects_TranscriptDedup_CostSum(t *testing.T) {
	// Three unique assistant records, summing to 60 output tokens.
	uniqueSum := 10 + 20 + 30
	objects := map[string][]byte{
		// Ship-1: first two records.
		"transcripts/dave/2026-06-03/a.ndjson": []byte(strings.Join([]string{
			transcriptNDJSON(t, "2026-06-03T10:00:00Z", assistantLine("msg_01", 10)),
			transcriptNDJSON(t, "2026-06-03T10:01:00Z", assistantLine("msg_02", 20)),
		}, "\n")),
		// Ship-2: re-ship of the first two + a third (restart #1).
		"transcripts/dave/2026-06-03/b.ndjson": []byte(strings.Join([]string{
			transcriptNDJSON(t, "2026-06-03T10:00:00Z", assistantLine("msg_01", 10)),
			transcriptNDJSON(t, "2026-06-03T10:01:00Z", assistantLine("msg_02", 20)),
			transcriptNDJSON(t, "2026-06-03T10:02:00Z", assistantLine("msg_03", 30)),
		}, "\n")),
		// Ship-3: full re-ship again (restart #2) — the 2-3x repeat from the issue.
		"transcripts/dave/2026-06-03/c.ndjson": []byte(strings.Join([]string{
			transcriptNDJSON(t, "2026-06-03T10:00:00Z", assistantLine("msg_01", 10)),
			transcriptNDJSON(t, "2026-06-03T10:01:00Z", assistantLine("msg_02", 20)),
			transcriptNDJSON(t, "2026-06-03T10:02:00Z", assistantLine("msg_03", 30)),
		}, "\n")),
	}
	since := mustTime(t, "2026-06-03T00:00:00Z")
	until := mustTime(t, "2026-06-03T23:59:59Z")

	got, err := readFromObjects(objects, "transcripts/", "dave", since, until)
	if err != nil {
		t.Fatalf("readFromObjects: %v", err)
	}
	if gotSum := sumOutputTokens(t, got); gotSum != uniqueSum {
		t.Errorf("deduped token sum = %d, want %d (inflation not removed)", gotSum, uniqueSum)
	}
	if len(got) != 3 {
		t.Errorf("want 3 unique records after dedup, got %d", len(got))
	}
}

// TestReadFromObjects_ArchiveLaneNoDedup is the lane-scoping guard: with the
// archive root ("agents/") dedup is OFF, so re-shipped duplicate ids are NOT
// collapsed — source=archive stays byte-for-byte identical (kyber#454 scope).
func TestReadFromObjects_ArchiveLaneNoDedup(t *testing.T) {
	dup := transcriptNDJSON(t, "2026-06-03T10:00:00Z", assistantLine("msg_01", 10))
	objects := map[string][]byte{
		"agents/dave/2026-06-03/a.ndjson": []byte(dup + "\n" + dup),
	}
	since := mustTime(t, "2026-06-03T00:00:00Z")
	until := mustTime(t, "2026-06-03T23:59:59Z")

	got, err := readFromObjects(objects, "", "dave", since, until) // empty root -> agents/
	if err != nil {
		t.Fatalf("readFromObjects: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("archive lane must NOT dedup; want 2 lines, got %d", len(got))
	}
}

// TestReadFromObjects_TranscriptDedup_PreservesIDlessLines is AC#2: lines that
// carry no stable id are preserved as-is and never collapsed — dedup only drops
// exact-id repeats and never drops distinct content.
func TestReadFromObjects_TranscriptDedup_PreservesIDlessLines(t *testing.T) {
	objects := map[string][]byte{
		"transcripts/dave/2026-06-03/a.ndjson": []byte(strings.Join([]string{
			// Two distinct id-less system lines — must both survive.
			transcriptNDJSON(t, "2026-06-03T10:00:00Z", `{"type":"system","content":"boot-A"}`),
			transcriptNDJSON(t, "2026-06-03T10:01:00Z", `{"type":"system","content":"boot-B"}`),
			// One assistant record duplicated — must collapse to one.
			transcriptNDJSON(t, "2026-06-03T10:02:00Z", assistantLine("msg_01", 10)),
			transcriptNDJSON(t, "2026-06-03T10:02:00Z", assistantLine("msg_01", 10)),
		}, "\n")),
	}
	since := mustTime(t, "2026-06-03T00:00:00Z")
	until := mustTime(t, "2026-06-03T23:59:59Z")

	got, err := readFromObjects(objects, "transcripts/", "dave", since, until)
	if err != nil {
		t.Fatalf("readFromObjects: %v", err)
	}
	// 2 distinct id-less lines + 1 collapsed assistant = 3.
	if len(got) != 3 {
		t.Fatalf("want 3 lines (2 id-less preserved + 1 deduped), got %d: %+v", len(got), got)
	}
	var bootA, bootB int
	for _, l := range got {
		if strings.Contains(l.Text, "boot-A") {
			bootA++
		}
		if strings.Contains(l.Text, "boot-B") {
			bootB++
		}
	}
	if bootA != 1 || bootB != 1 {
		t.Errorf("distinct id-less lines must be preserved exactly once each; bootA=%d bootB=%d", bootA, bootB)
	}
}

// TestReadFromObjects_TranscriptDedup_UUIDFallbackCollapses verifies a
// re-shipped non-assistant line that lacks message.id but carries a uuid is
// collapsed via the fallback key (consistent with the proposed direction).
func TestReadFromObjects_TranscriptDedup_UUIDFallbackCollapses(t *testing.T) {
	userLine := `{"type":"user","message":{"role":"user"},"uuid":"u-42"}`
	objects := map[string][]byte{
		"transcripts/dave/2026-06-03/a.ndjson": []byte(
			transcriptNDJSON(t, "2026-06-03T10:00:00Z", userLine)),
		"transcripts/dave/2026-06-03/b.ndjson": []byte(
			transcriptNDJSON(t, "2026-06-03T10:00:00Z", userLine)), // re-ship
	}
	since := mustTime(t, "2026-06-03T00:00:00Z")
	until := mustTime(t, "2026-06-03T23:59:59Z")

	got, err := readFromObjects(objects, "transcripts/", "dave", since, until)
	if err != nil {
		t.Fatalf("readFromObjects: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("re-shipped uuid line must collapse via fallback; want 1, got %d", len(got))
	}
}

// TestWindowScanner_DedupSeenWithinCap is AC#4/#5: the seen-set rides inside the
// existing maxLines cap (one entry per KEPT line, never per scanned duplicate),
// and Truncated stays correct — dedup only ever removes appends, so it makes the
// line cap less likely to trip and never falsely sets Truncated.
func TestWindowScanner_DedupSeenWithinCap(t *testing.T) {
	since := mustTime(t, "2026-06-03T00:00:00Z")
	until := mustTime(t, "2026-06-03T23:59:59Z")

	// 3 unique ids, each shipped 3x (9 lines total). With dedup ON and a cap of
	// 5, the 3 kept lines are well under the cap → NOT truncated, seen has 3.
	build := func() string {
		var b strings.Builder
		for ship := 0; ship < 3; ship++ {
			for i := 0; i < 3; i++ {
				b.WriteString(transcriptNDJSON(t,
					fmt.Sprintf("2026-06-03T10:00:%02dZ", i),
					assistantLine(fmt.Sprintf("msg_%02d", i), 10)))
				b.WriteString("\n")
			}
		}
		return b.String()
	}

	ws := newWindowScanner(since, until, scannerCaps{maxLines: 5}, true) // dedup ON, cap 5
	if err := ws.scan(strings.NewReader(build())); err != nil {
		t.Fatalf("scan: %v", err)
	}
	res := ws.result()
	if res.Truncated {
		t.Errorf("dedup must not falsely trigger Truncated: 3 kept lines under cap 5")
	}
	if len(res.Lines) != 3 {
		t.Fatalf("want 3 deduped lines, got %d", len(res.Lines))
	}
	if got := len(ws.seen); got != 3 {
		t.Errorf("seen-set size = %d, want 3 (one per kept line, not per scanned duplicate)", got)
	}

	// Same input, dedup OFF: 9 lines exceed cap 5 → truncated at 5 (regression
	// guard that the cap itself still trips when dedup is off).
	wsOff := newWindowScanner(since, until, scannerCaps{maxLines: 5}, false)
	if err := wsOff.scan(strings.NewReader(build())); err != nil {
		t.Fatalf("scan (dedup off): %v", err)
	}
	resOff := wsOff.result()
	if !resOff.Truncated {
		t.Errorf("dedup OFF: 9 lines over cap 5 must truncate")
	}
	if len(resOff.Lines) != 5 {
		t.Errorf("dedup OFF: want 5 lines (the cap), got %d", len(resOff.Lines))
	}
}

// TestS3ArchiveReader_TranscriptDedup exercises the full production read path
// (list + fetch-one-at-a-time + scan + dedup + sort) through the real
// S3ArchiveReader with rootPrefix "transcripts/": overlapping cumulative-reship
// objects collapse to one record per id, end to end (kyber#454 AC#1/#6).
func TestS3ArchiveReader_TranscriptDedup(t *testing.T) {
	store := &fakeS3Store{objects: map[string][]byte{
		"transcripts/dave/2026-06-03/a.ndjson": []byte(strings.Join([]string{
			transcriptNDJSON(t, "2026-06-03T10:00:00Z", assistantLine("msg_01", 10)),
			transcriptNDJSON(t, "2026-06-03T10:01:00Z", assistantLine("msg_02", 20)),
		}, "\n")),
		"transcripts/dave/2026-06-03/b.ndjson": []byte(strings.Join([]string{
			transcriptNDJSON(t, "2026-06-03T10:00:00Z", assistantLine("msg_01", 10)),
			transcriptNDJSON(t, "2026-06-03T10:01:00Z", assistantLine("msg_02", 20)),
			transcriptNDJSON(t, "2026-06-03T10:02:00Z", assistantLine("msg_03", 30)),
		}, "\n")),
	}}
	r := &S3ArchiveReader{store: store, bucket: "logs", rootPrefix: "transcripts/"}

	got := readLines(t, r, "dave",
		mustTime(t, "2026-06-03T00:00:00Z"), mustTime(t, "2026-06-03T23:59:59Z"))
	if len(got) != 3 {
		t.Fatalf("want 3 deduped records end-to-end, got %d: %+v", len(got), got)
	}
	if sum := sumOutputTokens(t, got); sum != 60 {
		t.Errorf("end-to-end deduped token sum = %d, want 60", sum)
	}
}

// TestS3ArchiveReader_ArchiveLaneNoDedup confirms the default (agents/) reader
// does NOT dedup through the full path — the archive surface is unchanged.
func TestS3ArchiveReader_ArchiveLaneNoDedup(t *testing.T) {
	dup := transcriptNDJSON(t, "2026-06-03T10:00:00Z", assistantLine("msg_01", 10))
	store := &fakeS3Store{objects: map[string][]byte{
		"agents/dave/2026-06-03/a.ndjson": []byte(dup + "\n" + dup),
	}}
	r := &S3ArchiveReader{store: store, bucket: "logs"} // default root -> agents/

	got := readLines(t, r, "dave",
		mustTime(t, "2026-06-03T00:00:00Z"), mustTime(t, "2026-06-03T23:59:59Z"))
	if len(got) != 2 {
		t.Fatalf("archive lane must NOT dedup end-to-end; want 2, got %d", len(got))
	}
}
