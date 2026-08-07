package api

// compaction_store.go — kyber#458 Part B: the write+delete-capable object stores
// behind transcript compaction, one per backend (S3/MinIO and GCS). These are a
// SEPARATE, higher-privilege surface from the read-only archive readers: the
// reader must never gain delete, so compaction gets its own store type rather
// than extending s3ObjectStore. Credentials are reused from the existing
// per-backend log config (MinIO Secret / GCS node ADC) and MUST be scoped to the
// log bucket; nothing here grants broader access.

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"cloud.google.com/go/storage"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"google.golang.org/api/iterator"
)

// --- S3 / MinIO ---

type s3CompactionStore struct {
	client *minio.Client
	bucket string
}

// NewS3CompactionStore builds a compaction store against an S3-compatible
// endpoint. Mirrors NewS3ArchiveReader's endpoint normalization + static creds;
// the credentials must be log-bucket-scoped and include delete (the genuinely
// new privilege vs the reader).
func NewS3CompactionStore(endpoint, bucket, region, accessKey, secretKey string, useTLS bool) (CompactionStore, error) {
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
	return &s3CompactionStore{client: client, bucket: bucket}, nil
}

func (s *s3CompactionStore) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("listing %q: %w", prefix, obj.Err)
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}

func (s *s3CompactionStore) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("opening %q: %w", key, err)
	}
	return obj, nil
}

func (s *s3CompactionStore) PutObject(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/x-ndjson"})
	if err != nil {
		return fmt.Errorf("writing %q: %w", key, err)
	}
	return nil
}

func (s *s3CompactionStore) RemoveObject(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("removing %q: %w", key, err)
	}
	return nil
}

// --- GCS ---

type gcsCompactionStore struct {
	client *storage.Client
	bucket string
}

// NewGCSCompactionStore builds a compaction store against a GCS bucket via node
// ADC (no static key), mirroring NewGCSArchiveReader. The node SA's bucket IAM
// must allow object delete for --apply (scoped to the log bucket).
func NewGCSCompactionStore(ctx context.Context, bucket string) (CompactionStore, error) {
	if bucket == "" {
		return nil, fmt.Errorf("archive bucket name is empty")
	}
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create GCS client: %w", err)
	}
	return &gcsCompactionStore{client: client, bucket: bucket}, nil
}

func (g *gcsCompactionStore) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	it := g.client.Bucket(g.bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing %q: %w", prefix, err)
		}
		keys = append(keys, attrs.Name)
	}
	return keys, nil
}

func (g *gcsCompactionStore) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := g.client.Bucket(g.bucket).Object(key).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening %q: %w", key, err)
	}
	return rc, nil
}

func (g *gcsCompactionStore) PutObject(ctx context.Context, key string, data []byte) error {
	w := g.client.Bucket(g.bucket).Object(key).NewWriter(ctx)
	w.ContentType = "application/x-ndjson"
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return fmt.Errorf("writing %q: %w", key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("writing %q: %w", key, err)
	}
	return nil
}

func (g *gcsCompactionStore) RemoveObject(ctx context.Context, key string) error {
	if err := g.client.Bucket(g.bucket).Object(key).Delete(ctx); err != nil {
		return fmt.Errorf("removing %q: %w", key, err)
	}
	return nil
}

// Close releases the GCS client. The CLI type-asserts io.Closer so the S3 store
// (no long-lived handle) needs no Close.
func (g *gcsCompactionStore) Close() error { return g.client.Close() }
