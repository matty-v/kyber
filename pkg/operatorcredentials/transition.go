package operatorcredentials

import (
	"bytes"
	"reflect"
)

func sameCreation(a, b Credential) bool {
	if a.CreatedAt == nil || b.CreatedAt == nil {
		return a.CreatedAt == nil && b.CreatedAt == nil
	}
	return a.CreatedAt.Equal(*b.CreatedAt)
}
func samePolicy(a, b Credential) bool {
	a.Name, b.Name = "", ""
	a.CreatedAt, b.CreatedAt = nil, nil
	a.material, b.material = nil, nil
	a.Generation, b.Generation = 0, 0
	a.PermissionRevision, b.PermissionRevision = 0, 0
	return reflect.DeepEqual(a, b)
}

// transition checks persistence invariants, not API permissions. Insertion and
// edits still require the administrator policy compiler and required audit
// admission when runtime integration is enabled.
func transition(old *Document, next Document) error {
	previous := map[string]Credential{}
	operations := map[string]Operation{}
	if old != nil {
		if old.Namespace != next.Namespace || old.SchemaVersion != next.SchemaVersion || !bytes.Equal(old.legacyBrowserSigningKey, next.legacyBrowserSigningKey) || old.legacyBrowserSigningGeneration != next.legacyBrowserSigningGeneration || !old.legacyBrowserIssuedBefore.Equal(next.legacyBrowserIssuedBefore) || !old.legacyBrowserExpiresBefore.Equal(next.legacyBrowserExpiresBefore) {
			return ErrInvalid
		}
		for _, c := range old.Credentials {
			previous[c.ID] = c
		}
		for _, op := range old.Operations {
			operations[op.ID] = op
		}
	}
	newReferences := map[string]bool{}
	actors := map[string]Credential{}
	if old == nil {
		for _, c := range next.Credentials {
			actors[c.ID] = c
		}
	} else {
		for _, c := range old.Credentials {
			actors[c.ID] = c
		}
	}
	nextOperations := map[string]Operation{}
	for _, op := range next.Operations {
		nextOperations[op.ID] = op
		if prior, ok := operations[op.ID]; ok {
			before, after := prior, op
			before.Status, after.Status = "", ""
			if !reflect.DeepEqual(before, after) || prior.Status == "delivered" && op.Status != "delivered" {
				return ErrInvalid
			}
		} else {
			if op.Status != "pending" {
				return ErrInvalid
			}
			actor, ok := actors[op.ActorCredentialID]
			if !ok || actor.State != StateActive || actor.Role != RoleAdministrator || actor.PrincipalID != op.ActorPrincipalID || actor.Generation != op.ActorGeneration {
				return ErrInvalid
			}
			newReferences[op.TargetCredentialID] = true
		}
	}
	for id, op := range operations {
		if _, ok := nextOperations[id]; !ok && op.Status == "pending" {
			return ErrInvalid
		}
	}
	for _, c := range next.Credentials {
		prior, exists := previous[c.ID]
		if exists {
			delete(previous, c.ID)
			if prior.PrincipalID != c.PrincipalID || prior.TenantID != c.TenantID || prior.Kind != c.Kind || prior.Issuer != c.Issuer || prior.Subject != c.Subject || prior.legacyBrowserGeneration != c.legacyBrowserGeneration || !sameCreation(prior, c) {
				return ErrInvalid
			}
			materialChanged := !bytes.Equal(prior.material, c.material)
			policyChanged := !samePolicy(prior, c)
			if prior.State == StateRevoked {
				if c.State != StateRevoked || materialChanged || policyChanged || c.Generation != prior.Generation || c.PermissionRevision != prior.PermissionRevision {
					return ErrInvalid
				}
			} else {
				if c.State == StateRevoked {
					if c.Generation != prior.Generation {
						return ErrInvalid
					}
				} else if materialChanged {
					if c.Generation != prior.Generation+1 {
						return ErrInvalid
					}
				} else if c.Generation != prior.Generation {
					return ErrInvalid
				}
				if policyChanged {
					if c.PermissionRevision != prior.PermissionRevision+1 {
						return ErrInvalid
					}
				} else if c.PermissionRevision != prior.PermissionRevision {
					return ErrInvalid
				}
			}
			if reflect.DeepEqual(prior, c) {
				continue
			}
		}
		if !exists && old != nil && c.legacyBrowserGeneration != 0 {
			return ErrInvalid
		}
		if !newReferences[c.ID] && !newReferences[""] {
			return ErrInvalid
		}
	}
	// Tombstones and accepted records cannot vanish through whole-document writes.
	if len(previous) != 0 {
		return ErrInvalid
	}
	return nil
}
