package archivestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"cloud.google.com/go/storage"
	"github.com/minio/minio-go/v7"
)

// Composer is implemented by stores that can join objects without the bytes
// passing through the control plane.
type Composer interface {
	// Compose writes the concatenation of parts, in order, to dst and returns
	// its length. The parts are left in place; a failed Compose leaves no
	// object at dst.
	Compose(ctx context.Context, dst string, parts []string) (int64, error)
}

// ErrComposeUnsupported reports that a store cannot compose server-side, for
// example an archive store server older than the compose endpoint.
var ErrComposeUnsupported = errors.New("archivestore: compose is not supported by this store")

// Compose joins parts into dst in order: server-side when the store can, and
// otherwise by streaming each part through a single Put.
func Compose(ctx context.Context, s Store, dst string, parts []string) (int64, error) {
	if len(parts) == 0 {
		return 0, errors.New("archivestore: nothing to compose")
	}
	for _, k := range append([]string{dst}, parts...) {
		if err := ValidateKey(k); err != nil {
			return 0, err
		}
	}
	if c, ok := s.(Composer); ok {
		n, err := c.Compose(ctx, dst, parts)
		if !errors.Is(err, ErrComposeUnsupported) {
			return n, err
		}
	}
	pr, pw := io.Pipe()
	go func() {
		for _, k := range parts {
			rc, err := s.Open(ctx, k, 0, -1)
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			_, err = io.Copy(pw, rc)
			rc.Close()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.Close()
	}()
	n, err := s.Put(ctx, dst, pr)
	pr.CloseWithError(err)
	return n, err
}

// Compose concatenates the part files into a temporary sibling and renames it
// into place once complete and synced.
func (s *FilesystemStore) Compose(ctx context.Context, dst string, parts []string) (int64, error) {
	if dir := filepath.Dir(dst); dir != "." {
		if err := s.root.MkdirAll(dir, 0o700); err != nil {
			return 0, fmt.Errorf("creating object directory: %w", err)
		}
	}
	tmp := dst + ".partial"
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("creating object: %w", err)
	}
	var n int64
	for _, k := range parts {
		var src *os.File
		src, err = s.root.Open(k)
		if errors.Is(err, os.ErrNotExist) {
			err = fmt.Errorf("%w: %s", ErrNotFound, k)
		}
		if err != nil {
			break
		}
		var c int64
		c, err = io.Copy(f, ctxReader{ctx, src})
		src.Close()
		n += c
		if err != nil {
			break
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = s.root.Rename(tmp, dst)
	}
	if err != nil {
		_ = s.root.Remove(tmp)
		return 0, fmt.Errorf("composing object: %w", err)
	}
	return n, nil
}

// s3MaxComposeSources is S3's limit on parts in one multipart upload.
const s3MaxComposeSources = 10000

// Compose joins the parts with a server-side multipart copy. Every part but
// the last must be at least 5 MiB, which archive upload parts always are.
func (s *S3Store) Compose(ctx context.Context, dst string, parts []string) (int64, error) {
	if len(parts) > s3MaxComposeSources {
		return 0, fmt.Errorf("archivestore: %d parts exceed S3's limit of %d", len(parts), s3MaxComposeSources)
	}
	srcs := make([]minio.CopySrcOptions, len(parts))
	for i, k := range parts {
		srcs[i] = minio.CopySrcOptions{Bucket: s.bucket, Object: s.prefix + k}
	}
	compose := s.compose
	if compose == nil {
		compose = s.client.ComposeObject
	}
	info, err := compose(ctx, minio.CopyDestOptions{Bucket: s.bucket, Object: s.prefix + dst}, srcs...)
	if err != nil {
		return 0, fmt.Errorf("composing S3 archive: %w", err)
	}
	return info.Size, nil
}

// gcsMaxComposeSources is GCS's limit on sources in one compose request.
const gcsMaxComposeSources = 32

// gcsComposer is the slice of the GCS API compose needs, so the tree of
// compose requests can be tested without a bucket.
type gcsComposer interface {
	compose(ctx context.Context, dst string, srcs []string) (int64, error)
	delete(ctx context.Context, key string) error
}

type gcsBucket struct{ s *GCSStore }

func (b gcsBucket) compose(ctx context.Context, dst string, srcs []string) (int64, error) {
	bkt := b.s.client.Bucket(b.s.bucket)
	handles := make([]*storage.ObjectHandle, len(srcs))
	for i, k := range srcs {
		handles[i] = bkt.Object(b.s.prefix + k)
	}
	c := bkt.Object(b.s.prefix + dst).ComposerFrom(handles...)
	c.ContentType = "application/octet-stream"
	attrs, err := c.Run(ctx)
	if err != nil {
		return 0, err
	}
	return attrs.Size, nil
}

func (b gcsBucket) delete(ctx context.Context, key string) error { return b.s.Delete(ctx, key) }

// Compose joins the parts with GCS compose requests. More than 32 parts are
// joined in groups into temporary objects first, which are then joined, and
// removed afterwards.
func (s *GCSStore) Compose(ctx context.Context, dst string, parts []string) (int64, error) {
	api := s.composer
	if api == nil {
		api = gcsBucket{s}
	}
	return composeTree(ctx, api, dst, parts)
}

func composeTree(ctx context.Context, api gcsComposer, dst string, parts []string) (int64, error) {
	var temps []string
	defer func() {
		for _, t := range temps {
			_ = api.delete(context.WithoutCancel(ctx), t)
		}
	}()
	level := parts
	for round := 0; len(level) > gcsMaxComposeSources; round++ {
		var next []string
		for i := 0; i < len(level); i += gcsMaxComposeSources {
			group := level[i:min(i+gcsMaxComposeSources, len(level))]
			tmp := fmt.Sprintf("%s.compose-%d-%d", dst, round, i/gcsMaxComposeSources)
			if _, err := api.compose(ctx, tmp, group); err != nil {
				return 0, fmt.Errorf("composing GCS archive: %w", err)
			}
			temps = append(temps, tmp)
			next = append(next, tmp)
		}
		level = next
	}
	n, err := api.compose(ctx, dst, level)
	if err != nil {
		return 0, fmt.Errorf("composing GCS archive: %w", err)
	}
	return n, nil
}
