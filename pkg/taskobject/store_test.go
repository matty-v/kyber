package taskobject

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestMemoryStoreRoundTripAndRange(t *testing.T) {
	s := NewMemoryStore()
	if err := s.Put(context.Background(), "task/result", bytes.NewBufferString("abcdef"), 6, PutOptions{Filename: "../report.txt", ContentType: "text/plain"}); err != nil {
		t.Fatal(err)
	}
	o, err := s.Open(context.Background(), "task/result", &ByteRange{Offset: 2, Length: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Body.Close()
	b, _ := io.ReadAll(o.Body)
	if string(b) != "cde" || o.Size != 3 || o.Filename != "report.txt" {
		t.Fatalf("unexpected object: data=%q size=%d filename=%q", b, o.Size, o.Filename)
	}
	if err := s.Delete(context.Background(), "task/result"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(context.Background(), "task/result", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreRejectsSizeMismatch(t *testing.T) {
	err := NewMemoryStore().Put(context.Background(), "key", bytes.NewBufferString("long"), 3, PutOptions{})
	if err == nil {
		t.Fatal("expected size mismatch")
	}
}
