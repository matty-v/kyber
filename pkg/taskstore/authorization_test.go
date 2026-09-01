package taskstore

import (
	"errors"
	"testing"
)

func TestMemoryStoreAuthorizationBindsTenantPrincipalResourceAndCursor(t *testing.T) {
	store, err := NewMemoryStore(DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	agent := AgentRef{Namespace: "kyber-system", Name: "kiosk"}
	owner := AuthorizationContext{TenantID: "tenant_a", PrincipalID: "principal_owner", AgentResourceID: "kyber-system/kiosk"}
	for _, id := range []string{"task_11111111111111111111111111111111", "task_22222222222222222222222222222222"} {
		if _, err := store.Create(t.Context(), CreateParams{ID: id, Agent: agent, CreatedBy: owner.PrincipalID, Authorization: owner, Prompt: "work"}); err != nil {
			t.Fatal(err)
		}
	}

	for name, auth := range map[string]AuthorizationContext{
		"tenant":    {TenantID: "tenant_b", PrincipalID: owner.PrincipalID, AgentResourceID: owner.AgentResourceID},
		"principal": {TenantID: owner.TenantID, PrincipalID: "principal_other", AgentResourceID: owner.AgentResourceID},
		"resource":  {TenantID: owner.TenantID, PrincipalID: owner.PrincipalID, AgentResourceID: "kyber-system/other"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.GetAuthorized(t.Context(), agent, "task_11111111111111111111111111111111", auth); !errors.Is(err, ErrNotFound) {
				t.Fatalf("unauthorized get error=%v, want non-enumerating not found", err)
			}
			page, err := store.List(t.Context(), ListParams{Agent: agent, Authorization: auth})
			if err != nil || len(page.Tasks) != 0 {
				t.Fatalf("unauthorized list=(%+v,%v), want empty page", page, err)
			}
		})
	}

	page, err := store.List(t.Context(), ListParams{Agent: agent, Authorization: owner, Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("owner page=(%+v,%v), want cursor", page, err)
	}
	other := AuthorizationContext{TenantID: owner.TenantID, PrincipalID: "principal_other", AgentResourceID: owner.AgentResourceID}
	if _, err := store.List(t.Context(), ListParams{Agent: agent, Authorization: other, Limit: 1, Cursor: page.NextCursor}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cursor replay across principals error=%v, want invalid cursor", err)
	}
}

func TestMemoryStoreCreateIdempotencyIsTenantScoped(t *testing.T) {
	store, err := NewMemoryStore(DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	agent := AgentRef{Namespace: "kyber-system", Name: "kiosk"}
	base := CreateParams{Agent: agent, CreatedBy: "principal_local", Prompt: "same request", IdempotencyKey: "same-key", RequestHash: "same-hash"}
	first := base
	first.ID = "task_33333333333333333333333333333333"
	first.Authorization = AuthorizationContext{TenantID: "tenant_a", PrincipalID: "principal_local", AgentResourceID: "kyber-system/kiosk"}
	createdA, err := store.Create(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	second := base
	second.ID = "task_44444444444444444444444444444444"
	second.Authorization = AuthorizationContext{TenantID: "tenant_b", PrincipalID: "principal_local", AgentResourceID: "kyber-system/kiosk"}
	createdB, err := store.Create(t.Context(), second)
	if err != nil {
		t.Fatalf("tenant-local principal and idempotency key must not collide across tenants: %v", err)
	}
	if createdB.Replay || createdB.Task.ID == createdA.Task.ID {
		t.Fatalf("cross-tenant create replayed first task: first=%s second=%s replay=%v", createdA.Task.ID, createdB.Task.ID, createdB.Replay)
	}
}
