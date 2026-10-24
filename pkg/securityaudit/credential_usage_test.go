package securityaudit

import (
	"github.com/google/uuid"
	"math"
	"testing"
	"time"
)

func usageBatchFixture() CredentialUsageBatch {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	return CredentialUsageBatch{Namespace: "kyber-system", ObserverID: uuid.NewString(), StartedAt: at, Sequence: 1, Items: []CredentialUsageDelta{{CredentialUsageIdentity: CredentialUsageIdentity{CredentialID: "credential-fixture", PrincipalID: "principal-fixture", Authority: "registry"}, Transport: "bearer", Authenticated: 3, Denied: 1, LastAcceptedAt: &at, Generation: 1, Actions: map[string]uint64{"agents:read": 2, "logs:read": 1}}}}
}
func TestCredentialUsageValidationRejectsUnboundedOrUnattributableData(t *testing.T) {
	cases := map[string]func(*CredentialUsageBatch){
		"unknown action": func(b *CredentialUsageBatch) { b.Items[0].Actions = map[string]uint64{"/api/raw?key=fixture": 3} },
		"wrong count":    func(b *CredentialUsageBatch) { b.Items[0].Authenticated = 4 },
		"overflow": func(b *CredentialUsageBatch) {
			b.Items[0].Actions = map[string]uint64{"agents:read": math.MaxUint64, "logs:read": 1}
		},
		"unknown transport":     func(b *CredentialUsageBatch) { b.Items[0].Transport = "client supplied" },
		"missing principal":     func(b *CredentialUsageBatch) { b.Items[0].PrincipalID = "" },
		"missing accepted time": func(b *CredentialUsageBatch) { b.Items[0].LastAcceptedAt = nil },
		"duplicate identity":    func(b *CredentialUsageBatch) { b.Items = append(b.Items, b.Items[0]) },
		"zero sequence":         func(b *CredentialUsageBatch) { b.Sequence = 0 },
		"noncanonical observer": func(b *CredentialUsageBatch) { b.ObserverID = "not-an-observer" },
		"invalid namespace":     func(b *CredentialUsageBatch) { b.Namespace = "../other" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			b := usageBatchFixture()
			change(&b)
			if b.Validate() == nil {
				t.Fatal("invalid usage accepted")
			}
		})
	}
	b := usageBatchFixture()
	if b.Validate() != nil {
		t.Fatal("valid batch rejected")
	}
	b.Items[0].Authenticated = 0
	b.Items[0].Actions = nil
	b.Items[0].LastAcceptedAt = nil
	b.Items[0].Generation = 0
	if b.Validate() != nil {
		t.Fatal("denial-only observation rejected; accepted observation may have been lost")
	}
	b.Items = nil
	b.Stopped = true
	if b.Validate() != nil {
		t.Fatal("empty final heartbeat rejected")
	}
}
