package taskobject

import (
	"context"
	"errors"
	"fmt"
	"io"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
)

type GCSStore struct {
	client *storage.Client
	bucket string
}

func NewGCSStore(ctx context.Context, bucket string) (*GCSStore, error) {
	if bucket == "" {
		return nil, errors.New("task object bucket is empty")
	}
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create GCS task object client: %w", err)
	}
	return &GCSStore{client: client, bucket: bucket}, nil
}

func (s *GCSStore) Close() error { return s.client.Close() }

func (s *GCSStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) error {
	if err := validateKey(key); err != nil {
		return err
	}
	w := s.client.Bucket(s.bucket).Object(key).NewWriter(ctx)
	w.ContentType = opts.ContentType
	w.Metadata = map[string]string{"filename": SanitizeFilename(opts.Filename)}
	w.ChunkSize = 256 * 1024
	written, err := io.Copy(w, io.LimitReader(body, size+1))
	if err == nil && written != size {
		err = fmt.Errorf("task object size mismatch: declared %d, read %d", size, written)
	}
	if err != nil {
		_ = w.CloseWithError(err)
		return fmt.Errorf("write GCS task object: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finalize GCS task object: %w", err)
	}
	return nil
}

func (s *GCSStore) Open(ctx context.Context, key string, br *ByteRange) (*Object, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	object := s.client.Bucket(s.bucket).Object(key)
	attrs, err := object.Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("stat GCS task object: %w", err)
	}
	offset, length := int64(0), attrs.Size
	if br != nil {
		if br.Offset < 0 || br.Length <= 0 || br.Offset >= attrs.Size {
			return nil, errors.New("task object byte range is unsatisfiable")
		}
		offset, length = br.Offset, br.Length
		if length > attrs.Size-offset {
			length = attrs.Size - offset
		}
	}
	r, err := object.NewRangeReader(ctx, offset, length)
	if err != nil {
		return nil, fmt.Errorf("open GCS task object: %w", err)
	}
	return &Object{Body: r, Size: length, Filename: attrs.Metadata["filename"], ContentType: attrs.ContentType}, nil
}

func (s *GCSStore) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	err := s.client.Bucket(s.bucket).Object(key).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) || isGCSNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete GCS task object: %w", err)
	}
	return nil
}

func isGCSNotFound(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == 404
}
