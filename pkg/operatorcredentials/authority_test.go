package operatorcredentials

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func fixtureAuthority(t *testing.T) (*AuthorityStore, client.Client, Snapshot) {
	t.Helper()
	registry, direct := fixtureStore(t)
	snapshot, err := registry.Create(context.Background(), fixtureDocument())
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	if err := direct.Get(context.Background(), registry.key, secret); err != nil {
		t.Fatal(err)
	}
	secret.UID = types.UID("registry-original")
	if err := direct.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	snapshot, err = registry.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	authority, err := newAuthorityStore(registry, "authority-fixture")
	if err != nil {
		t.Fatal(err)
	}
	registry.writer = maintenanceClient{Client: direct}
	return authority, direct, snapshot
}

// Keep uncertain bind reservations: tests reconcile those writes explicitly.
func fixtureBind(ctx context.Context, authority *AuthorityStore, version string) error {
	reservation, err := authority.ReserveAdoption(ctx)
	if err != nil {
		return err
	}
	err = authority.Bind(ctx, version, reservation)
	if err == nil || errors.Is(err, ErrConflict) {
		if releaseErr := reservation.Release(context.Background()); releaseErr != nil {
			return errors.Join(err, releaseErr)
		}
	}
	return err
}

func bindFixture(t *testing.T, authority *AuthorityStore, direct client.Client, version string) {
	t.Helper()
	if err := fixtureBind(context.Background(), authority, version); err != nil {
		t.Fatal(err)
	}
	// The fake API does not assign UIDs, unlike the real API server.
	marker := &corev1.Secret{}
	if err := direct.Get(context.Background(), authority.key, marker); err != nil {
		t.Fatal(err)
	}
	marker.UID = types.UID("authority-original")
	if err := direct.Update(context.Background(), marker); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorityBindingAndFreshReads(t *testing.T) {
	ctx := context.Background()
	a, direct, original := fixtureAuthority(t)
	if _, err := a.Load(ctx); !errors.Is(err, ErrUninitialized) {
		t.Fatal("missing adoption marker did not identify bootstrap", err)
	}
	if err := fixtureBind(ctx, a, "stale"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	bindFixture(t, a, direct, original.Version)
	loaded, err := a.Load(ctx)
	if err != nil || loaded.Version != original.Version {
		t.Fatal("bound registry not loaded", err)
	}
	if err := fixtureBind(ctx, a, loaded.Version); !errors.Is(err, ErrConflict) {
		t.Fatal("existing marker overwritten", err)
	}
	next := loaded.Document.Clone()
	next.Credentials[1].Name = "Changed on another replica"
	next.Operations = append(next.Operations, fixtureOperation("update", fixtureServiceID))
	updated, err := a.registry.Replace(ctx, loaded.Version, next)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err = a.Load(ctx)
	if err != nil || loaded.Version != updated.Version || loaded.Document.Credentials[1].Name != "Changed on another replica" {
		t.Fatal("stale registry accepted", err)
	}
	// Reconstructing the resolver simulates a new process/replica.
	b, err := newAuthorityStore(a.registry, a.key.Name)
	if err != nil {
		t.Fatal(err)
	}
	if loaded, err = b.Load(ctx); err != nil || loaded.Version != updated.Version {
		t.Fatal("new replica lost authoritative selection", err)
	}
}

func TestAuthorityBindRequiresItsLiveAdoptionReservation(t *testing.T) {
	ctx := context.Background()
	a, direct, snapshot := fixtureAuthority(t)
	if err := a.Bind(ctx, snapshot.Version, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("unreserved adoption accepted", err)
	}
	other, err := newMaintenanceStore(a.registry, "other-maintenance")
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := other.Acquire(ctx, MaintenanceAdoption)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Bind(ctx, snapshot.Version, wrong); !errors.Is(err, ErrInvalid) {
		t.Fatal("unrelated maintenance accepted", err)
	}
	maintenance, err := newMaintenanceStore(a.registry, a.registry.key.Name+"-maintenance")
	if err != nil {
		t.Fatal(err)
	}
	upgrading, err := maintenance.Acquire(ctx, MaintenanceUpgrade)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Bind(ctx, snapshot.Version, upgrading); !errors.Is(err, ErrInvalid) {
		t.Fatal("upgrade reservation authorized adoption", err)
	}
	if err := upgrading.Release(ctx); err != nil {
		t.Fatal(err)
	}
	reserved, err := a.ReserveAdoption(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := reserved.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Bind(ctx, snapshot.Version, reserved); !errors.Is(err, ErrUnavailable) {
		t.Fatal("released reservation authorized adoption", err)
	}
	if err := direct.Get(ctx, a.key, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatal("failed reservation checks created a marker")
	}
}

func TestAuthorityNeverFallsBackAfterAdoption(t *testing.T) {
	for _, mode := range []string{"missing registry", "replacement registry", "corrupt registry", "missing marker", "invalid marker"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			a, direct, snapshot := fixtureAuthority(t)
			bindFixture(t, a, direct, snapshot.Version)
			if _, err := a.Load(ctx); err != nil {
				t.Fatal(err)
			}
			key := a.registry.key
			if mode == "missing marker" || mode == "invalid marker" {
				key = a.key
			}
			secret := &corev1.Secret{}
			if err := direct.Get(ctx, key, secret); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing registry", "missing marker":
				if err := direct.Delete(ctx, secret); err != nil {
					t.Fatal(err)
				}
			case "replacement registry":
				secret.UID = "replacement"
				if err := direct.Update(ctx, secret); err != nil {
					t.Fatal(err)
				}
			case "corrupt registry":
				secret.Data[dataKey] = []byte("private backend detail")
				if err := direct.Update(ctx, secret); err != nil {
					t.Fatal(err)
				}
			case "invalid marker":
				secret.Labels = nil
				if err := direct.Update(ctx, secret); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.Load(ctx); err == nil || errors.Is(err, ErrUninitialized) {
				t.Fatal("adopted authority allowed bootstrap fallback", err)
			}
			if mode == "missing marker" {
				if err := fixtureBind(ctx, a, snapshot.Version); !errors.Is(err, ErrConflict) {
					t.Fatal("observed authority silently rebound after marker deletion", err)
				}
			}
			// Missing/corrupt registry remains fail closed on a fresh replica as well.
			if mode != "missing marker" {
				fresh, err := newAuthorityStore(a.registry, a.key.Name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fresh.Load(ctx); err == nil || errors.Is(err, ErrUninitialized) {
					t.Fatal("new process allowed bootstrap fallback", err)
				}
			}
		})
	}
}

func TestAuthorityRejectsMalformedMarkers(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Secret){
		"protocol":           func(s *corev1.Secret) { s.Data["protocol"] = []byte("2") },
		"unexpected data":    func(s *corev1.Secret) { s.Data["extra"] = []byte("private") },
		"different registry": func(s *corev1.Secret) { s.Data["registryName"] = []byte("other") },
		"empty UID":          func(s *corev1.Secret) { s.Data["registryUID"] = nil },
		"mutable":            func(s *corev1.Secret) { s.Immutable = nil },
		"garbage collection": func(s *corev1.Secret) { s.OwnerReferences = []metav1.OwnerReference{{UID: "workload"}} },
	} {
		t.Run(name, func(t *testing.T) {
			a, direct, snapshot := fixtureAuthority(t)
			bindFixture(t, a, direct, snapshot.Version)
			marker := &corev1.Secret{}
			if err := direct.Get(context.Background(), a.key, marker); err != nil {
				t.Fatal(err)
			}
			mutate(marker)
			// Fake client deliberately permits malformed fixtures, including fields
			// a real API would refuse to change on an immutable Secret.
			if err := direct.Update(context.Background(), marker); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Load(context.Background()); !errors.Is(err, ErrUnsupported) {
				t.Fatal("malformed marker accepted", err)
			}
		})
	}
}

type uncertainAuthorityWriter struct{ client.Client }

func (w uncertainAuthorityWriter) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if err := w.Client.Create(ctx, object, options...); err != nil {
		return err
	}
	return errors.New("lost response containing private transport detail")
}

func TestAuthorityLostBindResponseAndConcurrentBind(t *testing.T) {
	ctx := context.Background()
	a, direct, snapshot := fixtureAuthority(t)
	a.registry.writer = uncertainAuthorityWriter{maintenanceClient{Client: direct}}
	if err := fixtureBind(ctx, a, snapshot.Version); err != ErrUnavailable {
		t.Fatal("uncertain response not redacted", err)
	}
	marker := &corev1.Secret{}
	if err := direct.Get(ctx, a.key, marker); err != nil {
		t.Fatal("bind did not commit", err)
	}
	marker.UID = "authority-original"
	if err := direct.Update(ctx, marker); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Load(ctx); err != nil {
		t.Fatal("lost response not reconciled by read", err)
	}

	a, _, snapshot = fixtureAuthority(t)
	b, err := newAuthorityStore(a.registry, a.key.Name)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, contender := range []*AuthorityStore{a, b} {
		wg.Add(1)
		go func(s *AuthorityStore) { defer wg.Done(); results <- fixtureBind(ctx, s, snapshot.Version) }(contender)
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal("concurrent bindings did not have one winner")
	}
}

func TestAuthorityReadFailureNeverMeansBootstrap(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		a, direct, snapshot := fixtureAuthority(t)
		if adopted {
			bindFixture(t, a, direct, snapshot.Version)
		}
		reader := &failingReader{Reader: direct, failure: errors.New("private transport detail")}
		a.registry.reader = reader
		if _, err := a.Load(context.Background()); err != ErrUnavailable {
			t.Fatal("read failure treated as missing marker or leaked details", err)
		}
		reader.failure = nil
		reader.blocked = true
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		_, err := a.Load(ctx)
		cancel()
		if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("deadline lost", err)
		}
	}
}

func TestAuthorityConstructorRejectsAliases(t *testing.T) {
	registry, _ := fixtureStore(t)
	for _, name := range []string{"", "../other", registry.key.Name} {
		if _, err := newAuthorityStore(registry, name); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid/aliased marker accepted", err)
		}
	}
	if _, err := newAuthorityStore(nil, "valid"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
