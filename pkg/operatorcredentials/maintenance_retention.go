package operatorcredentials

import (
	"context"
	"log/slog"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"
)

const maintenanceWriterCleanupTTL int32 = 7 * 24 * 60 * 60

// ScheduleWriterCleanup is housekeeping for this private handle after a verified
// release. Uncertainty may retain extra evidence; it must not invent a failed credential
// operation or authorize deleting a reservation. No public cleanup endpoint.
func (r *MaintenanceReservation) ScheduleWriterCleanup(ctx context.Context) error {
	if r == nil || r.store == nil || r.uid == "" {
		return ErrInvalid
	}
	if r.writer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	status, err := r.store.Inspect(ctx)
	if err != nil {
		return err
	}
	if status.Present && (status.UID == string(r.uid) || status.Writer == nil || status.Writer.JobUID == r.writer.JobUID) {
		return ErrConflict
	}
	job := &batchv1.Job{}
	if err := r.store.direct.reader.Get(ctx, types.NamespacedName{Namespace: r.store.key.Namespace, Name: r.writer.JobName}, job); err != nil {
		return authorityUnavailable(ctx, err)
	}
	cleanupOwner := r.owner
	if r.cleanupOwner != "" {
		cleanupOwner = r.cleanupOwner
	}
	if job.Annotations[maintenanceExecutionAnnotation] != cleanupOwner || string(job.UID) != r.writer.JobUID || job.Namespace != r.store.key.Namespace || job.ResourceVersion == "" || job.DeletionTimestamp != nil || !singleRunJob(job) || !writerPodSpec(job.Spec.Template.Spec, r.writer.Container) {
		return ErrConflict
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		if *job.Spec.TTLSecondsAfterFinished == maintenanceWriterCleanupTTL {
			return nil
		}
		return ErrConflict
	}
	ttl := maintenanceWriterCleanupTTL
	job.Spec.TTLSecondsAfterFinished = &ttl
	// Update carries both the inspected Job UID and resource version. An API
	// conflict retains evidence; never refresh and overwrite a replacement.
	if err := r.store.direct.writer.Update(ctx, job); err != nil {
		return authorityError(ctx, err)
	}
	confirmed := &batchv1.Job{}
	if err := r.store.direct.reader.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, confirmed); err != nil {
		return authorityUnavailable(ctx, err)
	}
	if confirmed.UID != job.UID || confirmed.Spec.TTLSecondsAfterFinished == nil || *confirmed.Spec.TTLSecondsAfterFinished != ttl {
		return ErrUnavailable
	}
	return nil
}

// Cleanup cannot change the authoritative release outcome. Unconfirmed cleanup
// may retain extra evidence and emits a value-free operational diagnostic.
func (r *MaintenanceReservation) released(ctx context.Context) error {
	if r.originalWriter != nil {
		original := *r
		original.writer = r.originalWriter
		original.cleanupOwner = r.owner
		original.originalWriter = nil
		if err := original.ScheduleWriterCleanup(ctx); err != nil {
			slog.Warn("Credential maintenance released; original writer cleanup was not confirmed", "job", original.writer.JobName, "jobUID", original.writer.JobUID)
		}
	}
	if err := r.ScheduleWriterCleanup(ctx); err != nil {
		slog.Warn("Credential maintenance released; writer evidence cleanup was not confirmed", "job", r.writer.JobName, "jobUID", r.writer.JobUID)
	}
	return nil
}
