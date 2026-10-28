package operatorcredentials

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

type delayedCredentialWriter struct {
	client.Client
	ready   chan struct{}
	release chan struct{}
}

func (w delayedCredentialWriter) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	close(w.ready)
	<-w.release
	return w.Client.Update(ctx, obj, opts...)
}

type lostCredentialUpdateWriter struct{ client.Client }

type noOpCredentialUpdateWriter struct{ client.Client }

func (w noOpCredentialUpdateWriter) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return nil
}

func (w lostCredentialUpdateWriter) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if err := w.Client.Update(ctx, obj, opts...); err != nil {
		return err
	}
	return errors.New("private transport diagnostic")
}

func TestWriteFenceMissingRegistryNeverMeansBootstrap(t *testing.T) {
	store, _ := fixtureStore(t)
	if _, err := store.Fence(context.Background(), "previous-version"); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrUninitialized) {
		t.Fatal("missing recovery authority allowed bootstrap", err)
	}
}

func TestWriteFenceRequiresObservedVersionAdvance(t *testing.T) {
	a, direct, initial := fixtureAuthority(t)
	a.registry.writer = noOpCredentialUpdateWriter{Client: direct}
	if _, err := a.registry.Fence(context.Background(), initial.Version); !errors.Is(err, ErrUnavailable) {
		t.Fatal("unchanged version claimed a fence", err)
	}
	current, err := a.registry.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if current.Version != initial.Version {
		t.Fatal("no-op fixture changed storage")
	}
}

func TestWriteFencePreventsLateCredentialCommit(t *testing.T) {
	for _, lostAck := range []bool{false, true} {
		t.Run(map[bool]string{false: "acknowledged fence", true: "lost fence acknowledgement"}[lostAck], func(t *testing.T) {
			a, direct, initial := fixtureAuthority(t)
			ctx := context.Background()
			delayed := delayedCredentialWriter{Client: direct, ready: make(chan struct{}), release: make(chan struct{})}
			unpause := sync.OnceFunc(func() { close(delayed.release) })
			t.Cleanup(unpause)
			writer, err := newSecretStore(direct, delayed, a.registry.key.Namespace, a.registry.key.Name)
			if err != nil {
				t.Fatal(err)
			}
			next := initial.Document.Clone()
			next.Credentials[1].Name = "Late update"
			next.Operations = append(next.Operations, fixtureOperation("update", fixtureServiceID))
			result := make(chan error, 1)
			go func() { _, err := writer.Replace(ctx, initial.Version, next); result <- err }()
			select {
			case <-delayed.ready:
			case err := <-result:
				t.Fatal("write refused before the race", err)
			case <-time.After(2 * time.Second):
				t.Fatal("write did not reach the barrier")
			}
			fencer := a.registry
			if lostAck {
				fencer.writer = lostCredentialUpdateWriter{Client: direct}
			}
			fenced, fenceErr := fencer.Fence(ctx, initial.Version)
			unpause()
			if lostAck {
				if !errors.Is(fenceErr, ErrUnavailable) {
					t.Fatal("lost fence response not unknown", fenceErr)
				}
			} else if fenceErr != nil || fenced.Version == initial.Version {
				t.Fatal("fence did not advance version", fenceErr)
			}
			select {
			case err := <-result:
				if !errors.Is(err, ErrConflict) {
					t.Fatal("late credential write committed", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("late write did not finish")
			}
			current, err := a.registry.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if current.Version == initial.Version || !reflect.DeepEqual(current.Document, initial.Document) {
				t.Fatal("fence changed policy or failed to advance")
			}
		})
	}
}

func TestWriteFenceRecognizesAlreadyCommittedUnknownUpdate(t *testing.T) {
	a, direct, initial := fixtureAuthority(t)
	ctx := context.Background()
	writer, err := newSecretStore(direct, lostCredentialUpdateWriter{Client: direct}, a.registry.key.Namespace, a.registry.key.Name)
	if err != nil {
		t.Fatal(err)
	}
	next := initial.Document.Clone()
	next.Credentials[0] = next.Credentials[0].WithSecretMaterial([]byte("private-rotated-fixture"))
	next.Credentials[0].Generation++
	op := fixtureOperation("rotate", fixtureAdminID)
	next.Operations = append(next.Operations, op)
	if _, err := writer.Replace(ctx, initial.Version, next); !errors.Is(err, ErrUnavailable) {
		t.Fatal("lost mutation response not unknown", err)
	}
	if _, err := a.registry.Fence(ctx, initial.Version); !errors.Is(err, ErrConflict) {
		t.Fatal("fence overwrote a committed mutation", err)
	}
	current, err := a.registry.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range current.Document.Operations {
		if record.ID == op.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("committed operation reference lost")
	}
	for _, record := range current.Document.Credentials {
		if record.ID == fixtureAdminID && record.Generation != 2 {
			t.Fatal("rotation replayed or lost")
		}
	}
}
