package operatorcredentials

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestAuthorityMutationPinsCurrentIncarnationAndVersion(t *testing.T) {
	authority, direct, initial := fixtureAuthority(t)
	bindFixture(t, authority, direct, initial.Version)
	current, err := authority.Load(context.Background())
	if err != nil || current.UID != "registry-original" {
		t.Fatal("selected incarnation unavailable", err)
	}
	next := current.Document.Clone()
	next.Credentials[1].Name = "Changed integration"
	next.Operations = append(next.Operations, fixtureOperation("update", fixtureServiceID))
	if _, err := authority.Replace(context.Background(), "another-registry", current.Version, next); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong registry incarnation accepted", err)
	}
	updated, err := authority.Replace(context.Background(), current.UID, current.Version, next)
	if err != nil || updated.UID != current.UID || updated.Version == current.Version {
		t.Fatal("current selected write failed", err)
	}
	next.Credentials[1].Name = "Stale overwrite"
	if _, err := authority.Replace(context.Background(), current.UID, current.Version, next); !errors.Is(err, ErrConflict) {
		t.Fatal("stale selected write accepted", err)
	}
	final, err := authority.Load(context.Background())
	if err != nil || final.Document.Credentials[1].Name != "Changed integration" {
		t.Fatal("stale write changed current state", err)
	}
}

func TestAuthorityMutationMissingMarkerNeverWritesRegistry(t *testing.T) {
	authority, direct, initial := fixtureAuthority(t)
	bindFixture(t, authority, direct, initial.Version)
	current, err := authority.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	marker := &corev1.Secret{}
	if err := direct.Get(context.Background(), authority.key, marker); err != nil {
		t.Fatal(err)
	}
	if err := direct.Delete(context.Background(), marker); err != nil {
		t.Fatal(err)
	}
	next := current.Document.Clone()
	next.Credentials[1].Name = "Unselected change"
	next.Operations = append(next.Operations, fixtureOperation("update", fixtureServiceID))
	if _, err := authority.Replace(context.Background(), current.UID, current.Version, next); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing marker became writable bootstrap", err)
	}
	final, err := authority.registry.Load(context.Background())
	if err != nil || final.Version != current.Version {
		t.Fatal("unselected registry changed", err)
	}
}

func TestAuthorityFencePinsIncarnationAndPreservesDocument(t *testing.T) {
	authority, direct, initial := fixtureAuthority(t)
	bindFixture(t, authority, direct, initial.Version)
	current, err := authority.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Fence(context.Background(), "another-registry", current.Version); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong incarnation fenced", err)
	}
	fenced, err := authority.Fence(context.Background(), current.UID, current.Version)
	if err != nil || fenced.UID != current.UID || fenced.Version == current.Version {
		t.Fatal("selected fence failed", err)
	}
	if _, err := authority.Replace(context.Background(), current.UID, current.Version, current.Document); !errors.Is(err, ErrConflict) {
		t.Fatal("fenced version remained writable", err)
	}
}
