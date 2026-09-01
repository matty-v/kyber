package taskobject

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type S3Store struct {
	client *minio.Client
	bucket string
}

func NewS3Store(endpoint, bucket, region, accessKey, secretKey string, useTLS bool) (*S3Store, error) {
	if endpoint == "" || bucket == "" || accessKey == "" || secretKey == "" {
		return nil, errors.New("task object S3 configuration is incomplete")
	}
	if parsed, err := url.Parse(endpoint); err == nil && parsed.Host != "" {
		endpoint = parsed.Host
		useTLS = parsed.Scheme != "http"
	}
	client, err := minio.New(strings.TrimSuffix(endpoint, "/"), &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: useTLS, Region: region})
	if err != nil {
		return nil, fmt.Errorf("create S3 task object client: %w", err)
	}
	return &S3Store{client: client, bucket: bucket}, nil
}

func (s *S3Store) Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) error {
	if err := validateKey(key); err != nil {
		return err
	}
	_, err := s.client.PutObject(ctx, s.bucket, key, body, size, minio.PutObjectOptions{ContentType: opts.ContentType, UserMetadata: map[string]string{"filename": SanitizeFilename(opts.Filename)}})
	if err != nil {
		return fmt.Errorf("write S3 task object: %w", err)
	}
	return nil
}

func (s *S3Store) Open(ctx context.Context, key string, br *ByteRange) (*Object, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	object, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("open S3 task object: %w", err)
	}
	info, err := object.Stat()
	_ = object.Close()
	if err != nil {
		if minio.ToErrorResponse(err).StatusCode == 404 {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("stat S3 task object: %w", err)
	}
	opts := minio.GetObjectOptions{}
	length := info.Size
	if br != nil {
		if br.Offset < 0 || br.Length <= 0 || br.Offset >= info.Size {
			return nil, errors.New("task object byte range is unsatisfiable")
		}
		end := br.Offset + br.Length - 1
		if end >= info.Size {
			end = info.Size - 1
		}
		if err := opts.SetRange(br.Offset, end); err != nil {
			return nil, fmt.Errorf("set S3 task object range: %w", err)
		}
		length = end - br.Offset + 1
	}
	object, err = s.client.GetObject(ctx, s.bucket, key, opts)
	if err != nil {
		return nil, fmt.Errorf("read S3 task object: %w", err)
	}
	filename := info.Metadata.Get("X-Amz-Meta-Filename")
	return &Object{Body: object, Size: length, Filename: filename, ContentType: info.ContentType}, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete S3 task object: %w", err)
	}
	return nil
}
