package archivestore

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func seal(t *testing.T, key, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewSealWriter(&buf, key)
	if err != nil {
		t.Fatal(err)
	}
	// Uneven writes exercise chunk boundaries.
	for p := plain; len(p) > 0; {
		n := min(len(p), 7777)
		if _, err := w.Write(p[:n]); err != nil {
			t.Fatal(err)
		}
		p = p[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := int64(buf.Len()), SealedLength(int64(len(plain))); got != want {
		t.Fatalf("sealed %d bytes, SealedLength says %d", got, want)
	}
	return buf.Bytes()
}

func openBytes(b []byte) RangeOpener {
	return func(off, n int64) (io.ReadCloser, error) {
		return io.NopCloser(io.NewSectionReader(bytes.NewReader(b), off, n)), nil
	}
}

func TestSealRoundTripAtChunkBoundaries(t *testing.T) {
	key, _ := NewDataKey()
	for _, size := range []int{0, 1, ChunkSize - 1, ChunkSize, ChunkSize + 1, 3*ChunkSize + 5, 70 * ChunkSize} {
		plain := make([]byte, size)
		rand.Read(plain)
		sealed := seal(t, key, plain)
		if p, ok := PlainLength(int64(len(sealed))); !ok || p != int64(size) {
			t.Fatalf("size %d: PlainLength = %d, %v", size, p, ok)
		}
		r, err := NewSealedReader(openBytes(sealed), int64(len(sealed)), key)
		if err != nil {
			t.Fatalf("size %d: open: %v", size, err)
		}
		got, err := io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("size %d: round trip mismatch (%v)", size, err)
		}
		if size > 10 {
			mid := make([]byte, 10)
			if _, err := r.ReadAt(mid, int64(size-10)); err != nil && err != io.EOF || !bytes.Equal(mid, plain[size-10:]) {
				t.Fatalf("size %d: tail ReadAt mismatch", size)
			}
		}
	}
}

func TestSealDetectsTamperingTruncationAndWrongKey(t *testing.T) {
	key, _ := NewDataKey()
	plain := bytes.Repeat([]byte("agent disk "), 20000)
	sealed := seal(t, key, plain)

	flipped := append([]byte(nil), sealed...)
	flipped[len(sealHeader)+100] ^= 1
	r, err := NewSealedReader(openBytes(flipped), int64(len(flipped)), key)
	if err == nil {
		_, err = io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
	}
	if !errors.Is(err, ErrSealCorrupt) {
		t.Errorf("bit flip: err = %v, want ErrSealCorrupt", err)
	}

	// Dropping the final chunk leaves a length that still parses; the missing
	// final flag must be detected.
	truncated := sealed[:len(sealHeader)+2*sealedSize]
	if _, err := NewSealedReader(openBytes(truncated), int64(len(truncated)), key); !errors.Is(err, ErrSealCorrupt) {
		t.Errorf("truncation: err = %v, want ErrSealCorrupt", err)
	}

	other, _ := NewDataKey()
	if _, err := NewSealedReader(openBytes(sealed), int64(len(sealed)), other); !errors.Is(err, ErrSealCorrupt) {
		t.Errorf("wrong key: err = %v, want ErrSealCorrupt", err)
	}
}

func TestWrapKey(t *testing.T) {
	dk, _ := NewDataKey()
	wrapped, err := WrapKey([]byte("signing-key"), dk)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapKey([]byte("signing-key"), wrapped)
	if err != nil || !bytes.Equal(got, dk) {
		t.Fatalf("UnwrapKey = %x, %v", got, err)
	}
	if _, err := UnwrapKey([]byte("other"), wrapped); !errors.Is(err, ErrSealCorrupt) {
		t.Fatalf("UnwrapKey with wrong secret = %v, want ErrSealCorrupt", err)
	}
}

func TestValidateKey(t *testing.T) {
	for key, ok := range map[string]bool{
		"exports/abc/archive.sealed": true, "a": true,
		"": false, "/abs": false, "a/../b": false, "a//b": false, "A": false, "a/.hidden": false, "a/": false,
	} {
		if (ValidateKey(key) == nil) != ok {
			t.Errorf("ValidateKey(%q) ok=%v, want %v", key, !ok, ok)
		}
	}
}

// TestHTTPStoreAgainstServer exercises the built-in store end to end: auth,
// streaming put, ranged reads, size, sealed reads and delete.
func TestHTTPStoreAgainstServer(t *testing.T) {
	fs, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&Server{Store: fs, Token: "tok"})
	defer srv.Close()
	ctx := context.Background()

	bad := &HTTPStore{BaseURL: srv.URL, Token: "wrong"}
	if _, err := bad.Put(ctx, "x", bytes.NewReader([]byte("a"))); err == nil {
		t.Fatal("wrong token accepted")
	}

	s := &HTTPStore{BaseURL: srv.URL, Token: "tok"}
	key, _ := NewDataKey()
	plain := bytes.Repeat([]byte("0123456789"), 30000)
	pr, pw := io.Pipe()
	go func() {
		w, _ := NewSealWriter(pw, key)
		w.Write(plain)
		pw.CloseWithError(w.Close())
	}()
	n, err := s.Put(ctx, "exports/job1/archive.sealed", pr)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if size, err := s.Size(ctx, "exports/job1/archive.sealed"); err != nil || size != n {
		t.Fatalf("Size = %d, %v; want %d", size, err, n)
	}
	r, err := OpenSealed(ctx, s, "exports/job1/archive.sealed", key)
	if err != nil {
		t.Fatalf("OpenSealed: %v", err)
	}
	got := make([]byte, 20)
	if _, err := r.ReadAt(got, 123456); err != nil || !bytes.Equal(got, plain[123456:123476]) {
		t.Fatalf("ranged sealed read mismatch: %v", err)
	}
	if c, err := s.Capacity(ctx); err != nil || c.TotalBytes <= 0 {
		t.Fatalf("Capacity = %+v, %v", c, err)
	}
	if err := s.Delete(ctx, "exports/job1/archive.sealed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Size(ctx, "exports/job1/archive.sealed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Size after delete = %v, want ErrNotFound", err)
	}
}

func TestInterruptedPutLeavesNoObject(t *testing.T) {
	fs, _ := NewFilesystemStore(t.TempDir())
	srv := httptest.NewServer(&Server{Store: fs, Token: "tok"})
	defer srv.Close()
	s := &HTTPStore{BaseURL: srv.URL, Token: "tok", Client: &http.Client{}}
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte("partial"))
		pw.CloseWithError(errors.New("export pod died"))
	}()
	if _, err := s.Put(context.Background(), "exports/j/archive.sealed", pr); err == nil {
		t.Fatal("interrupted Put succeeded")
	}
	if _, err := fs.Size(context.Background(), "exports/j/archive.sealed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("object exists after interrupted put: %v", err)
	}
}
