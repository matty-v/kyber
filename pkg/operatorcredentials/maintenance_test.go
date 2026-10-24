package operatorcredentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type maintenanceClient struct {
	client.Client
	lostCreate bool
}

func (c maintenanceClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	obj.SetUID(types.UID(uuid.NewString())) // Real API servers assign this.
	if err := c.Client.Create(ctx, obj, opts...); err != nil {
		return err
	}
	if c.lostCreate {
		return errors.New("private transport diagnostic")
	}
	return nil
}

func fixtureMaintenance(t *testing.T, lostCreate bool) (*MaintenanceStore, client.Client) {
	t.Helper()
	direct, c := fixtureStore(t)
	direct.writer = maintenanceClient{Client: c, lostCreate: lostCreate}
	s, err := newMaintenanceStore(direct, "maintenance-fixture")
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func TestMaintenanceSerializesIndependentOwners(t *testing.T) {
	s, _ := fixtureMaintenance(t, false)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan *MaintenanceReservation, 2)
	errorsC := make(chan error, 2)
	for _, kind := range []string{MaintenanceAdoption, MaintenanceUpgrade} {
		wg.Add(1)
		go func(kind string) {
			defer wg.Done()
			r, err := s.Acquire(ctx, kind)
			results <- r
			errorsC <- err
		}(kind)
	}
	wg.Wait()
	close(results)
	close(errorsC)
	winners, conflicts := 0, 0
	var winner *MaintenanceReservation
	for r := range results {
		if r != nil {
			winners++
			winner = r
		}
	}
	for err := range errorsC {
		if errors.Is(err, ErrConflict) {
			conflicts++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("winners=%d conflicts=%d", winners, conflicts)
	}
	if err := winner.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(ctx, MaintenanceUpgrade); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceLostAcknowledgementAndRestart(t *testing.T) {
	s, _ := fixtureMaintenance(t, true)
	ctx := context.Background()
	first, err := s.Acquire(ctx, MaintenanceAdoption)
	if err != nil {
		t.Fatal(err)
	}
	// A second executor must not borrow the operation identity from storage.
	restarted, err := newMaintenanceStore(s.direct, s.key.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Acquire(ctx, MaintenanceAdoption); !errors.Is(err, ErrConflict) {
		t.Fatal("existing execution stolen", err)
	}
	second := first
	if err := second.Check(ctx, MaintenanceAdoption); err != nil {
		t.Fatal(err)
	}
	if err := second.Check(ctx, MaintenanceUpgrade); !errors.Is(err, ErrInvalid) {
		t.Fatal("wrong kind accepted", err)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := first.Check(ctx, MaintenanceAdoption); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing reservation accepted", err)
	}
}

func TestMaintenanceNeverExpiresOrDeletesReplacement(t *testing.T) {
	s, c := fixtureMaintenance(t, false)
	ctx := context.Background()
	first, err := s.Acquire(ctx, MaintenanceAdoption)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, s.key, secret); err != nil {
		t.Fatal(err)
	}
	secret.Data["startedAt"] = []byte(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano))
	// Fake permits immutable-data updates; the real-API test covers immutability.
	if err := c.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(ctx, MaintenanceUpgrade); !errors.Is(err, ErrConflict) {
		t.Fatal("aged reservation stolen", err)
	}
	if err := c.Delete(ctx, secret); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.Acquire(ctx, MaintenanceUpgrade)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal("stale owner released replacement", err)
	}
	if err := replacement.Check(ctx, MaintenanceUpgrade); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceRejectsUnownedStateAndRedactsHandle(t *testing.T) {
	s, c := fixtureMaintenance(t, false)
	ctx := context.Background()
	owner := uuid.NewString()
	if err := c.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.key.Namespace, Name: s.key.Name}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(ctx, MaintenanceAdoption); !errors.Is(err, ErrUnsupported) {
		t.Fatal("unrelated Secret accepted", err)
	}
	if _, err := s.Acquire(ctx, "other"); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid kind accepted", err)
	}
	r := MaintenanceReservation{store: s, owner: owner, uid: "private-uid", version: "private-version"}
	if _, err := json.Marshal(r); !errors.Is(err, ErrSensitive) {
		t.Fatal("handle serialized", err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%q", "%x"} {
		text := fmt.Sprintf(format, r)
		if !strings.Contains(text, "[redacted]") || strings.Contains(text, owner) || strings.Contains(text, "private-") {
			t.Fatal("handle leaked")
		}
	}
}

func TestMaintenanceOutageDoesNotReuseOwnershipOrRelease(t *testing.T) {
	s, direct := fixtureMaintenance(t, false)
	ctx := context.Background()
	r, err := s.Acquire(ctx, MaintenanceAdoption)
	if err != nil {
		t.Fatal(err)
	}
	s.direct.reader = &failingReader{Reader: direct, failure: errors.New("private backend response")}
	checks := []func() error{
		func() error { return r.Check(ctx, MaintenanceAdoption) },
		func() error { _, err := s.Acquire(ctx, MaintenanceUpgrade); return err },
		func() error { return r.Release(ctx) },
	}
	for _, check := range checks {
		if err := check(); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "private") {
			t.Fatal("outage used cached ownership or exposed backend details", err)
		}
	}
	s.direct.reader = direct
	if err := r.Check(ctx, MaintenanceAdoption); err != nil {
		t.Fatal("outage released reservation", err)
	}
	s.direct.reader = &failingReader{Reader: direct, blocked: true}
	bounded, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	if err := r.Check(bounded, MaintenanceAdoption); !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("deadline not preserved", err)
	}
}

func TestMaintenanceInspectionIsReadOnlyAndNeverInfersExpiry(t *testing.T) {
	s, c := fixtureMaintenance(t, false)
	ctx := context.Background()
	idle, err := s.Inspect(ctx)
	if err != nil || idle.Present {
		t.Fatal("missing reservation not idle", err)
	}
	reservation, err := s.Acquire(ctx, MaintenanceAdoption)
	if err != nil {
		t.Fatal(err)
	}
	before := &corev1.Secret{}
	if err := c.Get(ctx, s.key, before); err != nil {
		t.Fatal(err)
	}
	before.Data["startedAt"] = []byte("2000-01-01T00:00:00Z")
	if err := c.Update(ctx, before); err != nil {
		t.Fatal(err)
	}
	status, err := s.Inspect(ctx)
	if err != nil || !status.Present || status.ExecutionID != reservation.owner || status.Kind != MaintenanceAdoption || status.StartedAt.Year() != 2000 || status.UID == "" || status.Version == "" {
		t.Fatal("inspection lost blocking execution", err)
	}
	after := &corev1.Secret{}
	if err := c.Get(ctx, s.key, after); err != nil {
		t.Fatal(err)
	}
	if before.ResourceVersion != after.ResourceVersion {
		t.Fatal("inspection changed reservation")
	}
	if _, err := s.Acquire(ctx, MaintenanceUpgrade); !errors.Is(err, ErrConflict) {
		t.Fatal("inspection permitted takeover", err)
	}
	after.Data["private-value"] = []byte("must-not-appear")
	if err := c.Update(ctx, after); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Inspect(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatal("malformed reservation looked idle", err)
	}
}
