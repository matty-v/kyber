//go:build integration

package securityaudit

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func TestCredentialUsagePostgresReplayPrecisionAndIdentity(t *testing.T) {
	store, _ := postgresFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	batch := usageBatchFixture()
	const large = uint64(9007199254740993)
	batch.Items[0].Authenticated = large
	batch.Items[0].Generation = math.MaxUint64
	batch.Items[0].Actions = map[string]uint64{"agents:read": large}
	batch.Dropped = 2
	batch.Unrecognized = 4
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); failures <- store.WriteCredentialUsage(ctx, batch) }()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal("identical concurrent batch replay failed", err)
		}
	}
	view, err := store.ReadCredentialUsage(ctx, batch.Namespace, batch.ObserverID, []CredentialUsageIdentity{batch.Items[0].CredentialUsageIdentity})
	if err != nil || len(view.Items) != 1 || view.Items[0].Authenticated != "9007199254740993" || view.Items[0].Generation != "18446744073709551615" || view.Items[0].Actions["agents:read"] != "9007199254740993" || view.Coverage.Dropped != "2" || !view.Coverage.CurrentObserverReported {
		t.Fatal("replay duplicated or truncated counters", err)
	}
	conflict := usageBatchFixture()
	conflict.ObserverID = batch.ObserverID
	if err := store.WriteCredentialUsage(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatal("changed replay accepted", err)
	}
	next := batch
	next.Sequence = 3
	if err := store.WriteCredentialUsage(ctx, next); !errors.Is(err, ErrConflict) {
		t.Fatal("skipped sequence accepted", err)
	}
	next.Sequence = 2
	next.Items = nil
	next.Dropped = 0
	next.Unrecognized = 0
	if err := store.WriteCredentialUsage(ctx, next); err != nil {
		t.Fatal("empty heartbeat failed", err)
	}
	if err := store.WriteCredentialUsage(ctx, batch); !errors.Is(err, ErrConflict) {
		t.Fatal("old batch replay accepted after progress", err)
	}
	other := batch.Items[0].CredentialUsageIdentity
	other.PrincipalID = "another-principal"
	view, err = store.ReadCredentialUsage(ctx, batch.Namespace, batch.ObserverID, []CredentialUsageIdentity{other})
	if err != nil || len(view.Items) != 0 {
		t.Fatal("same credential ID leaked another principal's activity", err)
	}
	otherTenant := batch.Items[0].CredentialUsageIdentity
	otherTenant.TenantID = "another-tenant"
	view, err = store.ReadCredentialUsage(ctx, batch.Namespace, batch.ObserverID, []CredentialUsageIdentity{otherTenant})
	if err != nil || len(view.Items) != 0 {
		t.Fatal("tenant ownership was mixed", err)
	}
	view, err = store.ReadCredentialUsage(ctx, "another-namespace", batch.ObserverID, []CredentialUsageIdentity{batch.Items[0].CredentialUsageIdentity})
	if err != nil || len(view.Items) != 0 || view.Coverage.Since != nil {
		t.Fatal("cross-namespace activity leaked", err)
	}
	if _, err := store.ReadCredentialUsage(ctx, batch.Namespace, batch.ObserverID, nil); err != nil {
		t.Fatal("empty identity query failed", err)
	}
}

func TestCredentialUsagePostgresMergesCountsAndStopsObserver(t *testing.T) {
	store, _ := postgresFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	batch := usageBatchFixture()
	if err := store.WriteCredentialUsage(ctx, batch); err != nil {
		t.Fatal(err)
	}
	next := usageBatchFixture()
	next.ObserverID = batch.ObserverID
	next.Sequence = 2
	next.Stopped = true
	next.Items[0].Actions = map[string]uint64{"agents:read": 1, "credentials:read": 2}
	if err := store.WriteCredentialUsage(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteCredentialUsage(ctx, next); err != nil {
		t.Fatal("lost final acknowledgement could not replay", err)
	}
	view, err := store.ReadCredentialUsage(ctx, batch.Namespace, batch.ObserverID, []CredentialUsageIdentity{batch.Items[0].CredentialUsageIdentity})
	if err != nil || len(view.Items) != 1 || view.Items[0].Authenticated != "6" || view.Items[0].Denied != "2" || view.Items[0].Actions["agents:read"] != "3" || view.Items[0].Actions["credentials:read"] != "2" || view.Items[0].Actions["logs:read"] != "1" || view.Coverage.FreshObservers != 0 {
		t.Fatal("aggregate merge or final heartbeat failed", err)
	}
	next.Sequence = 3
	if err := store.WriteCredentialUsage(ctx, next); !errors.Is(err, ErrConflict) {
		t.Fatal("stopped observer resumed under the old identity", err)
	}
}

func TestCredentialUsagePostgresConcurrentObserversAndRestart(t *testing.T) {
	store, db := postgresFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	first := usageBatchFixture()
	other := first.Items[0]
	other.CredentialID = "another-credential"
	first.Items = append(first.Items, other)
	second := usageBatchFixture()
	second.Items = append(second.Items, other)
	// Opposite input order must still acquire aggregate-row locks consistently.
	second.Items[0], second.Items[1] = second.Items[1], second.Items[0]
	for i := range second.Items {
		second.Items[i].Generation = 2
	}
	failures := make(chan error, 2)
	go func() { failures <- store.WriteCredentialUsage(ctx, first) }()
	go func() { failures <- store.WriteCredentialUsage(ctx, second) }()
	for i := 0; i < 2; i++ {
		if err := <-failures; err != nil {
			t.Fatal("concurrent reporters conflicted", err)
		}
	}
	restarted, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	view, err := restarted.ReadCredentialUsage(ctx, first.Namespace, first.ObserverID, []CredentialUsageIdentity{first.Items[0].CredentialUsageIdentity, other.CredentialUsageIdentity})
	if err != nil || len(view.Items) != 2 || view.Coverage.FreshObservers != 2 {
		t.Fatal("reporter state did not survive store replacement", err)
	}
	for _, item := range view.Items {
		if item.Authenticated != "6" || item.Generation != "2" {
			t.Fatal("concurrent counters or tied generation lost an update")
		}
	}
}
