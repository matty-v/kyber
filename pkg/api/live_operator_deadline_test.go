package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type recordedOperatorDeadlines struct {
	*httptest.ResponseRecorder
	read, write time.Time
}

func (w *recordedOperatorDeadlines) SetReadDeadline(d time.Time) error  { w.read = d; return nil }
func (w *recordedOperatorDeadlines) SetWriteDeadline(d time.Time) error { w.write = d; return nil }

func TestLiveAccessDeadlineCannotBeExtendedAfterCancellation(t *testing.T) {
	for _, cause := range []error{errOperatorAccessChanged, errOperatorAuthorityUnavailable} {
		t.Run(cause.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			recorded := &recordedOperatorDeadlines{ResponseRecorder: httptest.NewRecorder()}
			writer := &liveOperatorWriter{ResponseWriter: recorded, ctx: ctx}
			control := http.NewResponseController(writer)
			future := time.Now().Add(time.Hour)
			if control.SetReadDeadline(future) != nil || control.SetWriteDeadline(future) != nil || recorded.read != future || recorded.write != future {
				t.Fatal("healthy transfer deadline changed")
			}
			cancel(cause)
			for _, reset := range []time.Time{time.Time{}, future} {
				before := time.Now()
				if control.SetReadDeadline(reset) != nil || control.SetWriteDeadline(reset) != nil {
					t.Fatal("deadline cancellation failed")
				}
				if recorded.read.Before(before) || recorded.write.Before(before) || recorded.read.After(time.Now()) || recorded.write.After(time.Now()) {
					t.Fatal("handler revived a cancelled transfer deadline")
				}
			}
		})
	}
}

type delayedOperatorReadDeadline struct {
	*recordedOperatorDeadlines
	entered, release chan struct{}
}

func (w *delayedOperatorReadDeadline) SetReadDeadline(d time.Time) error {
	if d.After(time.Now().Add(30 * time.Minute)) {
		close(w.entered)
		<-w.release
	}
	return w.recordedOperatorDeadlines.SetReadDeadline(d)
}

func TestLiveAccessCancellationWinsInFlightDeadlineChange(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	recorded := &recordedOperatorDeadlines{ResponseRecorder: httptest.NewRecorder()}
	delayed := &delayedOperatorReadDeadline{recordedOperatorDeadlines: recorded, entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-delayed.release:
		default:
			close(delayed.release)
		}
	}()
	writer := &liveOperatorWriter{ResponseWriter: delayed, ctx: ctx}
	control := http.NewResponseController(writer)
	healthyDone, cancelDone := make(chan struct{}), make(chan struct{})
	go func() { _ = control.SetReadDeadline(time.Now().Add(time.Hour)); close(healthyDone) }()
	select {
	case <-delayed.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("healthy deadline update did not start")
	}
	cancel(errOperatorAccessChanged)
	go func() { _ = control.SetReadDeadline(time.Now()); close(cancelDone) }()
	// Let cancellation race a handler deadline update that already passed its
	// initial context check. The late healthy update must not win afterward.
	select {
	case <-cancelDone:
	case <-time.After(100 * time.Millisecond):
	}
	close(delayed.release)
	for _, done := range []chan struct{}{healthyDone, cancelDone} {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("deadline update did not finish")
		}
	}
	if recorded.read.IsZero() || recorded.read.After(time.Now()) {
		t.Fatal("in-flight handler deadline revived cancelled access")
	}
}
