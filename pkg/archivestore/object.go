package archivestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3PartSize bounds memory for unknown-length uploads: minio-go buffers one
// part at a time, and 10,000 parts of 64 MiB allow archives up to 625 GiB.
const s3PartSize = 64 << 20

// S3Store keeps archives in an S3-compatible bucket under a prefix.
type S3Store struct {
	client *minio.Client
	bucket string
	prefix string
	// compose overrides client.ComposeObject in tests.
	compose func(context.Context, minio.CopyDestOptions, ...minio.CopySrcOptions) (minio.UploadInfo, error)
}

// NewS3Store connects to an S3-compatible endpoint.
func NewS3Store(endpoint, bucket, prefix, region, accessKey, secretKey string, useTLS bool) (*S3Store, error) {
	if bucket == "" || endpoint == "" {
		return nil, errors.New("archivestore: S3 endpoint and bucket are required")
	}
	if u, ok := strings.CutPrefix(endpoint, "https://"); ok {
		endpoint, useTLS = u, true
	} else if u, ok := strings.CutPrefix(endpoint, "http://"); ok {
		endpoint, useTLS = u, false
	}
	client, err := minio.New(strings.TrimRight(endpoint, "/"), &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useTLS,
		Region: region,
	})
	if err != nil {
		return nil, fmt.Errorf("creating S3 archive client: %w", err)
	}
	return &S3Store{client: client, bucket: bucket, prefix: prefix}, nil
}

func (s *S3Store) Name() string { return "s3" }

func (s *S3Store) Put(ctx context.Context, key string, body io.Reader) (int64, error) {
	if err := ValidateKey(key); err != nil {
		return 0, err
	}
	info, err := s.client.PutObject(ctx, s.bucket, s.prefix+key, body, -1,
		minio.PutObjectOptions{ContentType: "application/octet-stream", PartSize: s3PartSize})
	if err != nil {
		return 0, fmt.Errorf("writing S3 archive: %w", err)
	}
	return info.Size, nil
}

func (s *S3Store) Open(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	opts := minio.GetObjectOptions{}
	if length == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	end := int64(0)
	if length > 0 {
		end = offset + length - 1
	}
	if offset > 0 || length > 0 {
		if err := opts.SetRange(offset, end); err != nil {
			return nil, err
		}
	}
	obj, err := s.client.GetObject(ctx, s.bucket, s.prefix+key, opts)
	if err != nil {
		return nil, fmt.Errorf("reading S3 archive: %w", err)
	}
	return obj, nil
}

func (s *S3Store) Size(ctx context.Context, key string) (int64, error) {
	if err := ValidateKey(key); err != nil {
		return 0, err
	}
	info, err := s.client.StatObject(ctx, s.bucket, s.prefix+key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).StatusCode == 404 {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("stat S3 archive: %w", err)
	}
	return info.Size, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, s.bucket, s.prefix+key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("deleting S3 archive: %w", err)
	}
	return nil
}

// GCSStore keeps archives in a GCS bucket under a prefix, authenticated by
// the node's application default credentials.
type GCSStore struct {
	client *storage.Client
	bucket string
	prefix string
	// composer overrides the GCS compose API in tests.
	composer gcsComposer
}

// NewGCSStore connects with application default credentials.
func NewGCSStore(ctx context.Context, bucket, prefix string) (*GCSStore, error) {
	if bucket == "" {
		return nil, errors.New("archivestore: GCS bucket is required")
	}
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating GCS archive client: %w", err)
	}
	return &GCSStore{client: client, bucket: bucket, prefix: prefix}, nil
}

func (s *GCSStore) Name() string { return "gcs" }

func (s *GCSStore) Put(ctx context.Context, key string, body io.Reader) (int64, error) {
	if err := ValidateKey(key); err != nil {
		return 0, err
	}
	// Cancelling ctx aborts the resumable upload so no object is committed.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := s.client.Bucket(s.bucket).Object(s.prefix + key).NewWriter(ctx)
	w.ContentType = "application/octet-stream"
	w.ChunkSize = 8 << 20
	n, err := io.Copy(w, body)
	if err != nil {
		cancel()
		_ = w.Close()
		return 0, fmt.Errorf("writing GCS archive: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("finalizing GCS archive: %w", err)
	}
	return n, nil
}

func (s *GCSStore) Open(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	r, err := s.client.Bucket(s.bucket).Object(s.prefix+key).NewRangeReader(ctx, offset, length)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading GCS archive: %w", err)
	}
	return r, nil
}

func (s *GCSStore) Size(ctx context.Context, key string) (int64, error) {
	if err := ValidateKey(key); err != nil {
		return 0, err
	}
	attrs, err := s.client.Bucket(s.bucket).Object(s.prefix + key).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("stat GCS archive: %w", err)
	}
	return attrs.Size, nil
}

func (s *GCSStore) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	err := s.client.Bucket(s.bucket).Object(s.prefix + key).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("deleting GCS archive: %w", err)
	}
	return nil
}
