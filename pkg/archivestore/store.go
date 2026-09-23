// Package archivestore holds agent-disk archives (MAT-87/MAT-88) outside any
// agent volume. It has three backends behind one small interface:
//
//   - the built-in Kyber archive store (a one-replica StatefulSet serving a
//     filesystem over a private, token-authenticated HTTP API), which is the
//     default on every installation target because it needs nothing but the
//     cluster's default StorageClass;
//   - S3-compatible object storage;
//   - Google Cloud Storage.
//
// Objects are written with unknown length (an export streams as it walks the
// disk) and read by byte range (verification and restore read the ZIP's
// central directory first). Everything stored is sealed; see seal.go.
package archivestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
)

var ErrNotFound = errors.New("archivestore: object not found")

// Store is the archive byte store.
type Store interface {
	// Put streams body to key and returns the stored length. A failed Put
	// leaves no object at key.
	Put(ctx context.Context, key string, body io.Reader) (int64, error)
	// Open reads length bytes at offset. length < 0 reads to the end.
	Open(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
	// Size reports an object's stored length.
	Size(ctx context.Context, key string) (int64, error)
	// Delete removes key; deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// Name identifies the backend for status and logs.
	Name() string
}

// Capacity is a store's free space, when the backend can report it.
type Capacity struct {
	TotalBytes     int64 `json:"totalBytes"`
	AvailableBytes int64 `json:"availableBytes"`
}

// CapacityReporter is implemented by bounded backends (the built-in store).
// Object storage is treated as unbounded.
type CapacityReporter interface {
	Capacity(ctx context.Context) (Capacity, error)
}

var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)

// ValidateKey rejects anything but a lowercase, slash-separated key with no
// empty, dot or dot-dot segments, so a key can never address a path outside
// the store's root.
func ValidateKey(key string) error {
	if len(key) > 256 || !keyPattern.MatchString(key) {
		return fmt.Errorf("archivestore: invalid key %q", key)
	}
	return nil
}

// OpenSealed returns a plaintext io.ReaderAt over a sealed object.
func OpenSealed(ctx context.Context, s Store, key string, dataKey []byte) (*SealedReader, error) {
	size, err := s.Size(ctx, key)
	if err != nil {
		return nil, err
	}
	return NewSealedReader(func(off, n int64) (io.ReadCloser, error) {
		return s.Open(ctx, key, off, n)
	}, size, dataKey)
}
