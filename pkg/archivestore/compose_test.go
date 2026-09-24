package archivestore

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/minio/minio-go/v7"
)

// sealParts seals plain as parts of partSize plaintext bytes, the way an
// upload in parts does, and returns each part's stored bytes.
func sealParts(t *testing.T, key, plain []byte, partSize int) [][]byte {
	t.Helper()
	count := max(1, (len(plain)+partSize-1)/partSize)
	parts := make([][]byte, count)
	for n := range count {
		chunk := plain[n*partSize : min((n+1)*partSize, len(plain))]
		var buf bytes.Buffer
		w, err := NewPartSealWriter(&buf, key, uint64(n*partSize/ChunkSize), n == 0, n == count-1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("part %d: %v", n, err)
		}
		parts[n] = buf.Bytes()
	}
	return parts
}

func TestPartsSealByteIdenticalToOnePut(t *testing.T) {
	key, _ := NewDataKey()
	partSize := 3 * ChunkSize
	for _, size := range []int{1, ChunkSize, partSize, partSize + 1, 2*partSize + ChunkSize/2, 3 * partSize} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			plain := make([]byte, size)
			_, _ = rand.Read(plain)
			whole := seal(t, key, plain)
			joined := bytes.Join(sealParts(t, key, plain, partSize), nil)
			if !bytes.Equal(whole, joined) {
				t.Fatalf("joined parts differ from a single sealed stream (%d vs %d bytes)", len(joined), len(whole))
			}
			r, err := NewSealedReader(openBytes(joined), int64(len(joined)), key)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]byte, size)
			if _, err := r.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			if !bytes.Equal(got, plain) {
				t.Fatal("plaintext does not round-trip through parts")
			}
		})
	}
}

func TestNonFinalPartMustEndOnAChunkBoundary(t *testing.T) {
	key, _ := NewDataKey()
	for _, n := range []int{0, ChunkSize - 1, ChunkSize + 1} {
		w, err := NewPartSealWriter(io.Discard, key, 0, true, false)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(make([]byte, n))
		if err := w.Close(); err == nil {
			t.Fatalf("a %d-byte non-final part sealed without error", n)
		}
	}
}

func TestPartsOutOfOrderAreNotAValidArchive(t *testing.T) {
	key, _ := NewDataKey()
	plain := make([]byte, 5*ChunkSize)
	parts := sealParts(t, key, plain, 2*ChunkSize)
	swapped := bytes.Join([][]byte{parts[0], parts[2], parts[1]}, nil)
	r, err := NewSealedReader(openBytes(swapped), int64(len(swapped)), key)
	if err == nil {
		_, err = r.ReadAt(make([]byte, len(plain)), 0)
	}
	if !errors.Is(err, ErrSealCorrupt) {
		t.Fatalf("parts joined out of order must fail authentication, got %v", err)
	}
}

func putAll(t *testing.T, s Store, parts [][]byte) []string {
	t.Helper()
	keys := make([]string, len(parts))
	for i, p := range parts {
		keys[i] = fmt.Sprintf("uploads/u1/parts/%06d.sealed", i)
		if _, err := s.Put(context.Background(), keys[i], bytes.NewReader(p)); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func readAll(t *testing.T, s Store, key string) []byte {
	t.Helper()
	rc, err := s.Open(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFilesystemComposeJoinsParts(t *testing.T) {
	fs, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := NewDataKey()
	plain := make([]byte, 7*ChunkSize+123)
	_, _ = rand.Read(plain)
	parts := sealParts(t, key, plain, 2*ChunkSize)
	keys := putAll(t, fs, parts)
	n, err := Compose(context.Background(), fs, "uploads/u1/archive.sealed", keys)
	if err != nil {
		t.Fatal(err)
	}
	want := seal(t, key, plain)
	if got := readAll(t, fs, "uploads/u1/archive.sealed"); n != int64(len(want)) || !bytes.Equal(got, want) {
		t.Fatalf("composed object differs from a single sealed Put (%d bytes, want %d)", n, len(want))
	}
	// A missing part fails without leaving a readable object.
	if _, err := Compose(context.Background(), fs, "uploads/u2/archive.sealed", []string{keys[0], "uploads/u2/parts/000009.sealed"}); err == nil {
		t.Fatal("compose with a missing part succeeded")
	}
	if _, err := fs.Size(context.Background(), "uploads/u2/archive.sealed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed compose left an object: %v", err)
	}
}

// streamOnlyStore hides FilesystemStore's Composer to exercise the fallback.
type streamOnlyStore struct{ Store }

func TestComposeFallsBackToStreaming(t *testing.T) {
	fs, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := streamOnlyStore{fs}
	parts := [][]byte{[]byte("alpha-"), []byte("beta-"), []byte("gamma")}
	keys := putAll(t, s, parts)
	if _, err := Compose(context.Background(), s, "uploads/u1/archive.sealed", keys); err != nil {
		t.Fatal(err)
	}
	if got := string(readAll(t, s, "uploads/u1/archive.sealed")); got != "alpha-beta-gamma" {
		t.Fatalf("streamed compose = %q", got)
	}
}

func TestHTTPStoreComposeAndOldServerFallback(t *testing.T) {
	fs, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&Server{Store: fs, Token: "t"})
	defer srv.Close()
	hs := &HTTPStore{BaseURL: srv.URL, Token: "t"}
	keys := putAll(t, hs, [][]byte{[]byte("one-"), []byte("two")})
	n, err := hs.Compose(context.Background(), "uploads/u1/archive.sealed", keys)
	if err != nil || n != 7 {
		t.Fatalf("server-side compose: n=%d err=%v", n, err)
	}
	if got := string(readAll(t, hs, "uploads/u1/archive.sealed")); got != "one-two" {
		t.Fatalf("composed = %q", got)
	}
	if _, err := hs.Compose(context.Background(), "uploads/u1/other.sealed", []string{"uploads/u1/parts/000099.sealed"}); err == nil || errors.Is(err, ErrComposeUnsupported) {
		t.Fatalf("a missing part must be an error, not unsupported: %v", err)
	}

	// A store server that predates /v1/compose answers 404: Compose streams.
	old := http.NewServeMux()
	newer := &Server{Store: fs, Token: "t"}
	old.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == composePath {
			http.NotFound(w, r)
			return
		}
		newer.ServeHTTP(w, r)
	})
	oldSrv := httptest.NewServer(old)
	defer oldSrv.Close()
	oldStore := &HTTPStore{BaseURL: oldSrv.URL, Token: "t"}
	if _, err := oldStore.Compose(context.Background(), "uploads/u1/x.sealed", keys); !errors.Is(err, ErrComposeUnsupported) {
		t.Fatalf("old server: %v, want ErrComposeUnsupported", err)
	}
	if _, err := Compose(context.Background(), oldStore, "uploads/u1/x.sealed", keys); err != nil {
		t.Fatal(err)
	}
	if got := string(readAll(t, oldStore, "uploads/u1/x.sealed")); got != "one-two" {
		t.Fatalf("fallback compose = %q", got)
	}
}

func TestS3ComposeUsesServerSideCopyInOrder(t *testing.T) {
	var gotDst minio.CopyDestOptions
	var gotSrcs []minio.CopySrcOptions
	s := &S3Store{bucket: "b", prefix: "kyber/", compose: func(_ context.Context, dst minio.CopyDestOptions, srcs ...minio.CopySrcOptions) (minio.UploadInfo, error) {
		gotDst, gotSrcs = dst, srcs
		return minio.UploadInfo{Size: 42}, nil
	}}
	n, err := s.Compose(context.Background(), "uploads/u1/archive.sealed", []string{"uploads/u1/parts/000000.sealed", "uploads/u1/parts/000001.sealed"})
	if err != nil || n != 42 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if gotDst.Bucket != "b" || gotDst.Object != "kyber/uploads/u1/archive.sealed" {
		t.Fatalf("dst = %+v", gotDst)
	}
	var objs []string
	for _, src := range gotSrcs {
		if src.Bucket != "b" {
			t.Fatalf("src bucket = %q", src.Bucket)
		}
		objs = append(objs, src.Object)
	}
	if want := []string{"kyber/uploads/u1/parts/000000.sealed", "kyber/uploads/u1/parts/000001.sealed"}; !reflect.DeepEqual(objs, want) {
		t.Fatalf("sources = %v, want %v", objs, want)
	}
}

// fakeGCS records compose calls and joins in-memory objects.
type fakeGCS struct {
	objects map[string][]byte
	calls   [][]string
	deleted []string
}

func (f *fakeGCS) compose(_ context.Context, dst string, srcs []string) (int64, error) {
	if len(srcs) > gcsMaxComposeSources {
		return 0, fmt.Errorf("compose of %d sources exceeds GCS's limit", len(srcs))
	}
	f.calls = append(f.calls, append([]string{dst}, srcs...))
	var b []byte
	for _, s := range srcs {
		b = append(b, f.objects[s]...)
	}
	f.objects[dst] = b
	return int64(len(b)), nil
}

func (f *fakeGCS) delete(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	delete(f.objects, key)
	return nil
}

func TestGCSComposeBuildsATreeOverThirtyTwoParts(t *testing.T) {
	f := &fakeGCS{objects: map[string][]byte{}}
	var parts []string
	var want []byte
	for i := range 70 {
		k := fmt.Sprintf("uploads/u1/parts/%06d.sealed", i)
		f.objects[k] = []byte{byte(i)}
		want = append(want, byte(i))
		parts = append(parts, k)
	}
	s := &GCSStore{composer: f}
	n, err := s.Compose(context.Background(), "uploads/u1/archive.sealed", parts)
	if err != nil || n != 70 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !bytes.Equal(f.objects["uploads/u1/archive.sealed"], want) {
		t.Fatal("composed object is not the parts in order")
	}
	if len(f.calls) != 4 { // 70 parts → 3 groups → 1 final compose
		t.Fatalf("compose calls = %d, want 4", len(f.calls))
	}
	if len(f.deleted) != 3 {
		t.Fatalf("temporary objects deleted = %v, want 3", f.deleted)
	}
}
