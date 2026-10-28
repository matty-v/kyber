//go:build integration

package securityaudit

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestPostgresNativeAuthorizationLookupIsExactAndRetainedTruthful(t *testing.T) {
	store, db := postgresFixture(t)
	ctx := context.Background()
	event := fixtureAdoptionIntent().Admission
	appended, err := store.Append(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	restarted, _ := NewPostgresStore(db)
	loaded, err := restarted.LookupEvent(ctx, event.Namespace, event.ID)
	if err != nil || loaded.Event != appended.Event || loaded.Sequence != appended.Sequence || !loaded.StoredAt.Equal(appended.StoredAt) {
		t.Fatal("native authorization lookup changed recorded facts", err)
	}
	for _, query := range []struct{ namespace, id string }{{"other-installation", event.ID}, {event.Namespace, uuid.NewString()}} {
		if _, err := store.LookupEvent(ctx, query.namespace, query.id); !errors.Is(err, ErrEventNotFound) {
			t.Fatal("lookup borrowed another record/namespace", err)
		}
	}
	if _, err := store.LookupEvent(ctx, event.Namespace, "not-canonical"); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid native event identity accepted")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM operator_security_events WHERE namespace=$1 AND id=$2`, event.Namespace, event.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LookupEvent(ctx, event.Namespace, event.ID); !errors.Is(err, ErrEventNotFound) {
		t.Fatal("retained-away authorization invented a replacement", err)
	}
}
