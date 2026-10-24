package api

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
)

// OperatorCredentialDelivery confirms already-committed effects in the required
// journal, then marks and prunes their bounded outbox references. It cannot
// issue keys, fence an uncertain issuance, or change credential access. Runtime
// scheduling is deliberately separate from this staged primitive.
type OperatorCredentialDelivery struct {
	server *Server
	mu     sync.Mutex
	after  string
}

func NewOperatorCredentialDelivery(server *Server) *OperatorCredentialDelivery {
	return &OperatorCredentialDelivery{server: server}
}

type OperatorCredentialDeliveryResult struct {
	Inspected int
	Confirmed int
	Marked    int
	Pruned    int
}

// RunOnce inspects at most sixteen references under one bounded deadline and
// performs at most one registry CAS. The rotating cursor advances on errors too,
// so a missing journal record cannot permanently starve unrelated references.
// It is process-local: restarting merely repeats idempotent journal delivery.
func (d *OperatorCredentialDelivery) RunOnce(ctx context.Context) (OperatorCredentialDeliveryResult, error) {
	result := OperatorCredentialDeliveryResult{}
	if d == nil || d.server == nil {
		return result, operatorcredentials.ErrInvalid
	}
	if !d.mu.TryLock() {
		return result, operatorcredentials.ErrConflict
	}
	defer d.mu.Unlock()
	authority, journal, available := d.server.credentialManagementDependencies()
	if !available {
		return result, operatorcredentials.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snapshot, err := authority.Load(ctx)
	if err != nil || snapshot.UID == "" || snapshot.Version == "" || snapshot.Document.Namespace != d.server.Namespace {
		return result, operatorcredentials.ErrUnavailable
	}
	if err := snapshot.Document.Validate(); err != nil {
		return result, operatorcredentials.ErrUnavailable
	}
	references := append([]operatorcredentials.Operation{}, snapshot.Document.Operations...)
	sort.Slice(references, func(i, j int) bool { return references[i].ID < references[j].ID })
	start := sort.Search(len(references), func(i int) bool { return references[i].ID > d.after })
	if start == len(references) {
		start = 0
	}
	// Reserve time for the final two-second CAS even when journal work stalls.
	workCtx, workCancel := context.WithTimeout(ctx, 5*time.Second)
	defer workCancel()
	mark, prune := map[string]bool{}, map[string]bool{}
	var deliveryErr error
	for n := 0; n < len(references) && n < 16 && workCtx.Err() == nil; n++ {
		reference := references[(start+n)%len(references)]
		d.after = reference.ID
		result.Inspected++
		// Adoption has a different admission boundary and is intentionally retained
		// until that executor provides its own durable completion contract.
		if reference.Authority != "registry" || reference.Action == "adopt" {
			continue
		}
		lookupCtx, lookupCancel := context.WithTimeout(workCtx, securityAuditTimeout)
		operation, lookupErr := journal.GetCredentialOperation(lookupCtx, d.server.Namespace, reference.ID)
		lookupCancel()
		if lookupErr != nil || operation.Intent.Validate() != nil || operation.Intent.Admission.OperationID != reference.ID {
			deliveryErr = operatorcredentials.ErrUnavailable
			continue
		}
		outcome, proofErr := credentialRecoveryEvidence(operation.Intent, snapshot, nil)
		if proofErr != nil || outcome != "success" {
			deliveryErr = operatorcredentials.ErrUnavailable
			continue
		}
		if operation.Completion == nil {
			finishCtx, finishCancel := context.WithTimeout(workCtx, securityAuditTimeout)
			completion, finishErr := journal.FinishCredentialOperation(finishCtx, d.server.Namespace, reference.ID, "success", time.Now())
			finishCancel()
			if finishErr != nil {
				deliveryErr = operatorcredentials.ErrUnavailable
				continue
			}
			operation.Completion = &completion
		}
		// An acknowledged durable success is necessary even for a previously marked
		// reference. A missing/failed/conflicting journal never permits pruning.
		if operation.Completion.Outcome != "success" || operation.Completion.ID != reference.CompletionEventID {
			deliveryErr = operatorcredentials.ErrUnavailable
			continue
		}
		result.Confirmed++
		if reference.Status == "delivered" {
			prune[reference.ID] = true
		} else {
			mark[reference.ID] = true
		}
	}
	if workCtx.Err() != nil {
		deliveryErr = operatorcredentials.ErrUnavailable
	}
	if len(mark)+len(prune) == 0 {
		return result, deliveryErr
	}
	next := snapshot.Document.Clone()
	next.Operations = next.Operations[:0]
	for _, reference := range snapshot.Document.Operations {
		if prune[reference.ID] {
			continue
		}
		if mark[reference.ID] {
			reference.Status = "delivered"
		}
		next.Operations = append(next.Operations, reference)
	}
	// A conflict or lost acknowledgement never retries this candidate. A later
	// pass reads fresh state and uses the durable journal again.
	if _, err := authority.Replace(ctx, snapshot.UID, snapshot.Version, next); err != nil {
		return result, errors.Join(deliveryErr, operatorcredentials.ErrUnavailable)
	}
	result.Marked, result.Pruned = len(mark), len(prune)
	return result, deliveryErr
}
