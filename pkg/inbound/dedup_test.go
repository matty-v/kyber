package inbound

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInMemoryDeduper(t *testing.T) {
	t.Run("first sighting is not dedup", func(t *testing.T) {
		d := newInMemoryDeduper()
		seen, err := d.SeenWithin(context.Background(), []byte("hello"), time.Minute)
		if err != nil {
			t.Fatalf("SeenWithin: %v", err)
		}
		if seen {
			t.Fatalf("first sighting should not be seen")
		}
	})

	t.Run("second sighting within ttl is dedup", func(t *testing.T) {
		d := newInMemoryDeduper()
		body := []byte(`{"x":1}`)
		if _, err := d.SeenWithin(context.Background(), body, time.Minute); err != nil {
			t.Fatalf("first SeenWithin: %v", err)
		}
		seen, err := d.SeenWithin(context.Background(), body, time.Minute)
		if err != nil {
			t.Fatalf("second SeenWithin: %v", err)
		}
		if !seen {
			t.Fatalf("second sighting should be seen")
		}
	})

	t.Run("different bodies do not dedup each other", func(t *testing.T) {
		d := newInMemoryDeduper()
		ctx := context.Background()
		if seen, _ := d.SeenWithin(ctx, []byte("a"), time.Minute); seen {
			t.Fatalf("a should be unseen")
		}
		if seen, _ := d.SeenWithin(ctx, []byte("b"), time.Minute); seen {
			t.Fatalf("b should be unseen")
		}
	})

	t.Run("expiry past ttl resets", func(t *testing.T) {
		now := time.Unix(1_700_000_000, 0)
		d := newInMemoryDeduper()
		d.now = func() time.Time { return now }
		body := []byte("expiring")
		if _, err := d.SeenWithin(context.Background(), body, time.Second); err != nil {
			t.Fatal(err)
		}
		// advance clock past TTL.
		now = now.Add(2 * time.Second)
		seen, err := d.SeenWithin(context.Background(), body, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if seen {
			t.Fatalf("entry should have expired and the new sighting not be flagged")
		}
	})
}

// TestInMemoryDeduperConcurrent fires N goroutines at the SAME body and
// asserts exactly one is admitted. Guards against future regressions in
// the deduper's atomicity — the platform relies on this being race-free
// to absorb GitHub's "duplicate delivery" retry behaviour.
func TestInMemoryDeduperConcurrent(t *testing.T) {
	const N = 64
	d := newInMemoryDeduper()
	body := []byte(`{"delivery":"abc","race":true}`)

	var admitted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start // line up so all goroutines hit SeenWithin together
			seen, err := d.SeenWithin(context.Background(), body, time.Minute)
			if err != nil {
				t.Errorf("SeenWithin: %v", err)
				return
			}
			if !seen {
				admitted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := admitted.Load(); got != 1 {
		t.Fatalf("admitted=%d, want exactly 1 (concurrent identical bodies must dedup down to one)", got)
	}
}

func TestHashBodyStable(t *testing.T) {
	a := hashBody([]byte("abc"))
	b := hashBody([]byte("abc"))
	if a != b {
		t.Fatalf("hash should be deterministic; got %s vs %s", a, b)
	}
	if hashBody([]byte("abc")) == hashBody([]byte("abd")) {
		t.Fatalf("different inputs should hash differently")
	}
}
