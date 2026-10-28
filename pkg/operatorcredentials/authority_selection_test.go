package operatorcredentials

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestCompletedSelectionCannotBootstrapAfterMarkerLossAndRestart(t *testing.T) {
	ctx := context.Background()
	a, direct, initial := fixtureAuthority(t)
	reservation, err := a.ReserveAdoption(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Bind(ctx, initial.Version, reservation); err != nil {
		t.Fatal(err)
	}
	selection, err := a.ReadSelection(ctx)
	if err != nil || selection.RegistryUID != initial.UID || selection.UID == "" {
		t.Fatal("selection metadata unavailable", err)
	}
	sealed, err := a.SealSelection(ctx, selection.UID, selection.RegistryUID, reservation)
	if err != nil || sealed.Version == initial.Version || !reflect.DeepEqual(sealed.Document, initial.Document) {
		t.Fatal("selection sealing changed credential contents or failed", err)
	}
	// Normal managed mutation must preserve the durable selection receipt.
	next := sealed.Document.Clone()
	next.Credentials[1].Name = "Updated integration"
	next.Operations = append(next.Operations, fixtureOperation("update", fixtureServiceID))
	if _, err := a.Replace(ctx, sealed.UID, sealed.Version, next); err != nil {
		t.Fatal(err)
	}
	marker := &corev1.Secret{}
	if direct.Get(ctx, a.key, marker) != nil || direct.Delete(ctx, marker) != nil {
		t.Fatal("marker loss fixture failed")
	}
	restarted, _ := newAuthorityStore(a.registry, a.key.Name)
	if _, err := restarted.Load(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal("fresh resolver returned to configuration after completed selection", err)
	}
	if _, err := restarted.ReadSelection(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal("completed selection absence treated as uninitialized", err)
	}
}

func TestSelectionSealRequiresExactUIDsAndReconcilesLostAck(t *testing.T) {
	ctx := context.Background()
	a, direct, initial := fixtureAuthority(t)
	reservation, err := a.ReserveAdoption(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Bind(ctx, initial.Version, reservation); err != nil {
		t.Fatal(err)
	}
	selection, err := a.ReadSelection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SealSelection(ctx, "wrong-marker", selection.RegistryUID, reservation); !errors.Is(err, ErrConflict) {
		t.Fatal("unrelated marker sealed", err)
	}
	if _, err := a.SealSelection(ctx, selection.UID, "wrong-registry", reservation); !errors.Is(err, ErrConflict) {
		t.Fatal("unrelated registry sealed", err)
	}
	if _, err := a.SealSelection(ctx, selection.UID, selection.RegistryUID, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("unreserved seal allowed", err)
	}
	writer := &checkpointUpdateClient{Client: direct, lostAck: true}
	a.registry.writer = writer
	sealed, err := a.SealSelection(ctx, selection.UID, selection.RegistryUID, reservation)
	if err != nil || sealed.UID != initial.UID || writer.writes != 1 {
		t.Fatal("unknown seal write replayed or failed reconciliation", err)
	}
	repeated, err := a.SealSelection(ctx, selection.UID, selection.RegistryUID, reservation)
	if err != nil || repeated.Version != sealed.Version || writer.writes != 1 {
		t.Fatal("idempotent seal wrote again", err)
	}
}

func TestUnsealedCandidateRemainsRecoverableButMalformedSealFailsClosed(t *testing.T) {
	ctx := context.Background()
	a, direct, _ := fixtureAuthority(t)
	restarted, _ := newAuthorityStore(a.registry, a.key.Name)
	if _, err := restarted.Load(ctx); !errors.Is(err, ErrUninitialized) {
		t.Fatal("unselected candidate stranded configured administrator", err)
	}
	secret := &corev1.Secret{}
	if direct.Get(ctx, a.registry.key, secret) != nil {
		t.Fatal("registry fixture missing")
	}
	secret.Annotations = map[string]string{selectedAuthorityAnnotation: ""}
	if direct.Update(ctx, secret) != nil {
		t.Fatal("seal fixture failed")
	}
	restarted, _ = newAuthorityStore(a.registry, a.key.Name)
	if _, err := restarted.Load(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal("malformed durable seal allowed configuration fallback", err)
	}
}
