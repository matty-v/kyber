// Package taskobject contains the private object-storage boundary used by
// durable task results. Implementations must not produce public object URLs.
package taskobject

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

var ErrNotFound = errors.New("task object not found")

// ByteRange describes a half-open byte range. Length must be positive.
type ByteRange struct {
	Offset int64
	Length int64
}

type PutOptions struct {
	Filename    string
	ContentType string
}

type Object struct {
	Body        io.ReadCloser
	Size        int64
	Filename    string
	ContentType string
}

// ObjectStore is deliberately small so cloud-backed implementations can stream
// without temporary files. Keys are opaque, private application identifiers.
type ObjectStore interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) error
	Open(ctx context.Context, key string, byteRange *ByteRange) (*Object, error)
	Delete(ctx context.Context, key string) error
}

type memoryEntry struct {
	data []byte
	opts PutOptions
}

// MemoryStore is a concurrency-safe implementation intended for tests and
// local composition. It still enforces declared sizes and range semantics.
type MemoryStore struct {
	mu      sync.RWMutex
	objects map[string]memoryEntry
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{objects: make(map[string]memoryEntry)} }

func (s *MemoryStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if size < 0 {
		return errors.New("task object size must not be negative")
	}
	r := io.LimitReader(body, size+1)
	b, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read task object: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if int64(len(b)) != size {
		return fmt.Errorf("task object size mismatch: declared %d, read %d", size, len(b))
	}
	opts.Filename = SanitizeFilename(opts.Filename)
	s.mu.Lock()
	s.objects[key] = memoryEntry{data: append([]byte(nil), b...), opts: opts}
	s.mu.Unlock()
	return nil
}

func (s *MemoryStore) Open(ctx context.Context, key string, br *ByteRange) (*Object, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	e, ok := s.objects[key]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	start, end := int64(0), int64(len(e.data))
	if br != nil {
		if br.Offset < 0 || br.Length <= 0 {
			return nil, errors.New("invalid task object byte range")
		}
		if br.Offset >= end {
			return nil, errors.New("task object byte range is unsatisfiable")
		}
		start = br.Offset
		if br.Length < end-start {
			end = start + br.Length
		}
	}
	b := append([]byte(nil), e.data[start:end]...)
	return &Object{Body: io.NopCloser(bytes.NewReader(b)), Size: int64(len(b)), Filename: e.opts.Filename, ContentType: e.opts.ContentType}, nil
}

func (s *MemoryStore) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.objects, key)
	s.mu.Unlock()
	return nil
}

func validateKey(key string) error {
	if key == "" {
		return errors.New("task object key must not be empty")
	}
	return nil
}
