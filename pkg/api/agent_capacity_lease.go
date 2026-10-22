package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/google/uuid"
	coordinationv1 "k8s.io/api/coordination/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// acquireAgentCapacityLease serializes Kyber API allocation writes for one
// Machine across control-plane replicas. The read under the lease must use the
// uncached API reader; otherwise a second writer could see pre-commit data.
func (s *Server) acquireAgentCapacityLease(ctx context.Context, machineName string) (func(), error) {
	hash := sha256.Sum256([]byte(machineName))
	key := types.NamespacedName{Name: fmt.Sprintf("kyber-agent-capacity-%x", hash[:8]), Namespace: s.Namespace}
	holder := uuid.NewString()
	seconds := int32(60)
	deadline := time.Now().Add(10 * time.Second)
	reader := s.APIReader
	if reader == nil {
		reader = s.K8sClient
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("machine capacity reservation is busy")
		}
		lease := &coordinationv1.Lease{}
		err := reader.Get(ctx, key, lease)
		now := metav1.NewMicroTime(time.Now().UTC())
		if k8serrors.IsNotFound(err) {
			lease = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &seconds, AcquireTime: &now, RenewTime: &now}}
			err = s.K8sClient.Create(ctx, lease)
		} else if err == nil {
			busy := lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" &&
				lease.Spec.RenewTime != nil && lease.Spec.LeaseDurationSeconds != nil &&
				now.Time.Before(lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds)*time.Second))
			if busy {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			lease.Spec.HolderIdentity = &holder
			lease.Spec.LeaseDurationSeconds = &seconds
			lease.Spec.AcquireTime = &now
			lease.Spec.RenewTime = &now
			err = s.K8sClient.Update(ctx, lease)
		}
		if k8serrors.IsAlreadyExists(err) || k8serrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			current := &coordinationv1.Lease{}
			if reader.Get(releaseCtx, key, current) != nil || current.Spec.HolderIdentity == nil || *current.Spec.HolderIdentity != holder {
				return
			}
			empty := ""
			current.Spec.HolderIdentity = &empty
			_ = s.K8sClient.Update(releaseCtx, current)
		}, nil
	}
}
