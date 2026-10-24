package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
)

func committedDeliveryFixture(t *testing.T, completionUnavailable bool) (*OperatorCredentialDelivery, *managedAuthorityFixture, *credentialJournalFixture, string) {
	t.Helper()
	server, authority, journal, handler := managedHTTPFixture(t)
	journal.failCompletion = completionUnavailable
	request := managedCreateFixtureRequest()
	request.OperationID = "ffffffff-ffff-4fff-bfff-ffffffffffff"
	response := managedMutationHTTP(t, handler, request, credentialRegistryETag("managed-fixture", "1"), "original-installation-fixture")
	if response.Code != http.StatusOK && response.Code != http.StatusAccepted {
		t.Fatal("fixture key change failed", response.Code)
	}
	return NewOperatorCredentialDelivery(server), authority, journal, request.OperationID
}

func TestManagedCredentialDeliveryMarksThenPrunesOnlyDurableSuccess(t *testing.T) {
	delivery, authority, journal, id := committedDeliveryFixture(t, true)
	before := authority.document.Clone().Credentials
	if _, err := delivery.RunOnce(context.Background()); !errors.Is(err, operatorcredentials.ErrUnavailable) {
		t.Fatal("missing completion was treated as delivered", err)
	}
	if authority.writes != 1 || authority.document.Operations[0].Status != "pending" {
		t.Fatal("unconfirmed audit permitted outbox cleanup")
	}
	journal.failCompletion = false
	first, err := delivery.RunOnce(context.Background())
	if err != nil || first.Marked != 1 || first.Pruned != 0 || authority.document.Operations[0].Status != "delivered" {
		t.Fatal("durable success not marked", first, err)
	}
	completed, err := journal.GetCredentialOperation(context.Background(), "kyber-system", id)
	if err != nil || completed.Completion == nil || completed.Completion.Outcome != "success" {
		t.Fatal("completion not durable before marking")
	}
	count := len(journal.Records())
	second, err := delivery.RunOnce(context.Background())
	if err != nil || second.Pruned != 1 || len(authority.document.Operations) != 0 {
		t.Fatal("delivered reference not pruned", second, err)
	}
	if !reflect.DeepEqual(before, authority.document.Clone().Credentials) || len(journal.Records()) != count {
		t.Fatal("delivery changed keys or duplicated audit events")
	}
}

func TestManagedCredentialDeliveryRetainsMissingProofAndRotatesPastIt(t *testing.T) {
	delivery, authority, _, _ := committedDeliveryFixture(t, false)
	original := authority.document.Operations[0]
	for i := 0; i < 16; i++ {
		reference := original
		reference.ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		reference.AdmissionEventID, reference.CompletionEventID = uuid.NewString(), uuid.NewString()
		authority.document.Operations = append(authority.document.Operations, reference)
	}
	if err := authority.document.Validate(); err != nil {
		t.Fatal("invalid pending fixture", err)
	}
	first, err := delivery.RunOnce(context.Background())
	if !errors.Is(err, operatorcredentials.ErrUnavailable) || first.Inspected != 16 || authority.writes != 1 {
		t.Fatal("missing proof changed state", first, err)
	}
	second, err := delivery.RunOnce(context.Background())
	if !errors.Is(err, operatorcredentials.ErrUnavailable) || second.Marked != 1 {
		t.Fatal("missing records starved committed delivery", second, err)
	}
	if len(authority.document.Operations) != 17 {
		t.Fatal("missing proof was discarded")
	}
}

func TestManagedCredentialDeliveryLostAcknowledgementNeverReissuesKey(t *testing.T) {
	delivery, authority, journal, _ := committedDeliveryFixture(t, false)
	before := authority.document.Clone().Credentials
	auditCount := len(journal.Records())
	authority.commitThenLoseAck = true
	if _, err := delivery.RunOnce(context.Background()); !errors.Is(err, operatorcredentials.ErrUnavailable) {
		t.Fatal("lost acknowledgement reported confirmed", err)
	}
	authority.commitThenLoseAck = false
	result, err := delivery.RunOnce(context.Background())
	if err != nil || result.Pruned != 1 || len(authority.document.Operations) != 0 {
		t.Fatal("fresh pass did not reconcile committed marker", result, err)
	}
	if !reflect.DeepEqual(before, authority.document.Clone().Credentials) || len(journal.Records()) != auditCount {
		t.Fatal("cleanup replayed issuance or audit")
	}
}

func TestManagedCredentialDeliveryRejectsAnotherRegistryProof(t *testing.T) {
	delivery, authority, journal, id := committedDeliveryFixture(t, false)
	operation := journal.operations[id]
	operation.Intent.RegistryUID = "different-registry"
	journal.operations[id] = operation
	if _, err := delivery.RunOnce(context.Background()); !errors.Is(err, operatorcredentials.ErrUnavailable) {
		t.Fatal("wrong registry accepted", err)
	}
	if authority.writes != 1 || authority.document.Operations[0].Status != "pending" {
		t.Fatal("unrelated proof permitted cleanup")
	}
}
