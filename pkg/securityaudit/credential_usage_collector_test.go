package securityaudit

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

type capturedUsageWriter struct {
	mu      sync.Mutex
	batches []CredentialUsageBatch
	fail    bool
	hook    func(context.Context) error
}

func (w *capturedUsageWriter) WriteCredentialUsage(ctx context.Context, batch CredentialUsageBatch) error {
	if batch.Validate() != nil {
		return ErrInvalid
	}
	w.mu.Lock()
	w.batches = append(w.batches, batch.normalized())
	fail, hook := w.fail, w.hook
	w.mu.Unlock()
	if hook != nil {
		if err := hook(ctx); err != nil {
			return err
		}
	}
	if fail {
		return ErrUnavailable
	}
	return nil
}
func acceptedUsageObservation() CredentialUsageObservation {
	return CredentialUsageObservation{CredentialUsageIdentity: CredentialUsageIdentity{CredentialID: "credential-fixture", PrincipalID: "principal-fixture", Authority: "registry"}, Transport: "browser-session", Outcome: "accepted", Action: "agents:read", At: time.Now().UTC(), Generation: 1}
}
func TestCredentialUsageCollectorRetainsLostAcknowledgementAndReportsOverflow(t *testing.T) {
	writer := &capturedUsageWriter{fail: true}
	collector, err := newCredentialUsageCollector(writer, "kyber-system", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	observation := acceptedUsageObservation()
	for i := 0; i < 3; i++ {
		collector.ObserveCredentialUsage(observation)
	}
	if err := collector.Flush(context.Background()); err == nil {
		t.Fatal("unavailable writer acknowledged")
	}
	collector.ObserveCredentialUsage(observation)
	if err := collector.Flush(context.Background()); err == nil {
		t.Fatal("unavailable retry acknowledged")
	}
	first, _ := json.Marshal(writer.batches[0])
	retry, _ := json.Marshal(writer.batches[1])
	if string(first) != string(retry) || writer.batches[0].Dropped != 1 || writer.batches[0].Items[0].Authenticated != 2 {
		t.Fatal("pending batch changed or overflow disappeared")
	}
	status := collector.CredentialUsageLocalStatus()
	if status.PendingObservations != 3 || status.LossReportsAwaitingConfirmation != "1" {
		t.Fatal("pending coverage hidden", status)
	}
	writer.fail = false
	if err := collector.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := collector.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := writer.batches[len(writer.batches)-1]
	if next.Sequence != 2 || len(next.Items) != 1 || next.Items[0].Authenticated != 1 || next.Dropped != 0 {
		t.Fatal("new observations replayed or were lost")
	}
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	final := writer.batches[len(writer.batches)-1]
	if !final.Stopped || !collector.CredentialUsageLocalStatus().Closed {
		t.Fatal("observation window not closed")
	}
	count := len(writer.batches)
	collector.ObserveCredentialUsage(observation)
	if err := collector.Flush(context.Background()); err != nil || len(writer.batches) != count {
		t.Fatal("closed reporter resumed")
	}
}
func TestCredentialUsageCollectorObservationNeverWaitsForDatabase(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	writer := &capturedUsageWriter{hook: func(ctx context.Context) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	collector, err := NewCredentialUsageCollector(writer, "kyber-system", "")
	if err != nil {
		t.Fatal(err)
	}
	flushed := make(chan error, 1)
	go func() { flushed <- collector.Flush(context.Background()) }()
	<-started
	observed := make(chan struct{})
	go func() { collector.ObserveCredentialUsage(acceptedUsageObservation()); close(observed) }()
	select {
	case <-observed:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("authentication observation waited for database")
	}
	close(release)
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
}
func TestCredentialUsageCollectorSeparatesUnidentifiedOutcomes(t *testing.T) {
	writer := &capturedUsageWriter{}
	collector, err := NewCredentialUsageCollector(writer, "kyber-system", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"unrecognized", "unavailable", "unattributed"} {
		collector.ObserveCredentialUsage(CredentialUsageObservation{Outcome: outcome})
	}
	invalid := acceptedUsageObservation()
	invalid.Action = "/api/raw?token=not-persisted"
	collector.ObserveCredentialUsage(invalid)
	if err := collector.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	batch := writer.batches[0]
	if len(batch.Items) != 0 || batch.Unrecognized != 1 || batch.Unavailable != 1 || batch.Unattributed != 1 || batch.Dropped != 1 {
		t.Fatal("unidentified traffic gained a guessed identity or false zero")
	}
}
