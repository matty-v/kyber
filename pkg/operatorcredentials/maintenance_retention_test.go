package operatorcredentials

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestMaintenanceWriterRejectsAutomaticEvidenceExpiry(t *testing.T) {
	s, c, pod, job := writerFixture(t)
	ttl := int32(600)
	job.Spec.TTLSecondsAfterFinished = &ttl
	if err := c.Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireForPod(context.Background(), MaintenanceUpgrade, pod.Name, string(pod.UID)); !errors.Is(err, ErrUnsupported) {
		t.Fatal("expiring evidence accepted", err)
	}
	if status, err := s.Inspect(context.Background()); err != nil || status.Present {
		t.Fatal("refused writer acquired reservation", err)
	}
}
func TestMaintenanceWriterCleanupRequiresReleasedExactHandle(t *testing.T) {
	s, c, pod, job := writerFixture(t)
	ctx := context.Background()
	original, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, string(pod.UID))
	if err != nil {
		t.Fatal(err)
	}
	if err := original.ScheduleWriterCleanup(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal("blocked evidence scheduled for cleanup", err)
	}
	current := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(job), current); err != nil || current.Spec.TTLSecondsAfterFinished != nil {
		t.Fatal("blocked writer gained TTL", err)
	}
	if err := original.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(job), current); err != nil || current.Spec.TTLSecondsAfterFinished == nil || *current.Spec.TTLSecondsAfterFinished != maintenanceWriterCleanupTTL {
		t.Fatal("released evidence not scheduled", err)
	}
	if err := original.ScheduleWriterCleanup(ctx); err != nil {
		t.Fatal("same cleanup not idempotent", err)
	}
}
func TestMaintenanceRecoveryRetainsUntilExactReleaseThenSchedulesCleanup(t *testing.T) {
	s, c, _, status := stoppedRecoveryFixture(t)
	ctx := context.Background()
	operationID := uuid.NewString()
	handle, err := s.PrepareRecovery(ctx, status.UID, status.Version, operationID, "")
	if err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: s.Namespace(), Name: status.Writer.JobName}
	if err := c.Get(ctx, key, job); err != nil || job.Spec.TTLSecondsAfterFinished != nil {
		t.Fatal("pending recovery lost retention", err)
	}
	resumed, err := s.ResumeRecovery(ctx, operationID, handle.Proof())
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, job); err != nil || job.Spec.TTLSecondsAfterFinished == nil {
		t.Fatal("recovered evidence cleanup not scheduled", err)
	}
}
func TestMaintenanceCleanupNeverChangesReplacementJob(t *testing.T) {
	s, c, pod, job := writerFixture(t)
	ctx := context.Background()
	handle, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, string(pod.UID))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, job); err != nil {
		t.Fatal(err)
	}
	replacement := job.DeepCopy()
	replacement.UID = types.UID(uuid.NewString())
	replacement.ResourceVersion = ""
	replacement.CreationTimestamp = metav1.Time{}
	if err := c.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err := handle.Release(ctx); err != nil {
		t.Fatal("cleanup failure changed authoritative release", err)
	}
	current := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(job), current); err != nil || current.UID != replacement.UID || current.Spec.TTLSecondsAfterFinished != nil {
		t.Fatal("replacement job changed", err)
	}
	if err := handle.ScheduleWriterCleanup(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal("replacement cleanup accepted", err)
	}
}

type retentionUpdateFailureClient struct {
	client.Client
	unavailable bool
}

func (c retentionUpdateFailureClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if job, ok := obj.(*batchv1.Job); ok {
		if c.unavailable {
			return ErrUnavailable
		}
		current := &batchv1.Job{}
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(job), current); err != nil {
			return err
		}
		current.Annotations = map[string]string{"fixture": "concurrent-change"}
		if err := c.Client.Update(ctx, current); err != nil {
			return err
		}
	}
	return c.Client.Update(ctx, obj, opts...)
}
func TestMaintenanceCleanupUncertaintyRetainsEvidenceWithoutChangingRelease(t *testing.T) {
	for _, unavailable := range []bool{true, false} {
		s, c, pod, job := writerFixture(t)
		ctx := context.Background()
		handle, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, string(pod.UID))
		if err != nil {
			t.Fatal(err)
		}
		s.direct.writer = retentionUpdateFailureClient{Client: c, unavailable: unavailable}
		if err := handle.Release(ctx); err != nil {
			t.Fatal("housekeeping failure changed release", err)
		}
		status, err := s.Inspect(ctx)
		if err != nil || status.Present {
			t.Fatal("release not confirmed", err)
		}
		current := &batchv1.Job{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(job), current); err != nil || current.Spec.TTLSecondsAfterFinished != nil {
			t.Fatal("uncertain cleanup did not retain evidence", err)
		}
		if !unavailable && current.Annotations["fixture"] != "concurrent-change" {
			t.Fatal("concurrent metadata overwritten")
		}
	}
}
func TestMaintenanceSingleRunWriterCannotReacquireAfterRelease(t *testing.T) {
	s, c, pod, job := writerFixture(t)
	ctx := context.Background()
	old, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, string(pod.UID))
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Release(ctx); err != nil {
		t.Fatal(err)
	}
	// Even removing housekeeping TTL cannot make the same writer acquire again.
	current := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(job), current); err != nil {
		t.Fatal(err)
	}
	current.Spec.TTLSecondsAfterFinished = nil
	if err := c.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, string(pod.UID)); !errors.Is(err, ErrConflict) {
		t.Fatal("single-run writer acquired twice", err)
	}
	if status, err := s.Inspect(ctx); err != nil || status.Present {
		t.Fatal("second acquisition changed maintenance", err)
	}
}
func TestMaintenanceCheckpointHandleKeepsWriterCleanupOwnership(t *testing.T) {
	s, c, pod, job := writerFixture(t)
	ctx := context.Background()
	handle, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, string(pod.UID))
	if err != nil {
		t.Fatal(err)
	}
	next, err := handle.RecordUpgradeEffects(ctx, UpgradeEffectsCheckpoint{Release: "kyber", InitialRevision: 1, TargetVersion: "1.9.0", ControlPlaneDeployment: "kyber-control-plane", Authority: "unselected"})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Release(ctx); err != nil {
		t.Fatal(err)
	}
	current := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(job), current); err != nil || current.Spec.TTLSecondsAfterFinished == nil {
		t.Fatal("checkpoint discarded cleanup ownership", err)
	}
}

type retentionJobAdmissionClient struct {
	client.Client
	writes              int
	lostAck, failBefore bool
}

func (c *retentionJobAdmissionClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*batchv1.Job); ok {
		c.writes++
		if c.failBefore {
			return ErrUnavailable
		}
		if err := c.Client.Update(ctx, obj, opts...); err != nil {
			return err
		}
		if c.lostAck {
			return ErrUnavailable
		}
		return nil
	}
	return c.Client.Update(ctx, obj, opts...)
}
func TestMaintenanceJobAdmissionReconcilesOnlyOriginalAcknowledgement(t *testing.T) {
	for _, lostAck := range []bool{true, false} {
		s, c, pod, job := writerFixture(t)
		ctx := context.Background()
		writer := &retentionJobAdmissionClient{Client: s.direct.writer, lostAck: lostAck, failBefore: !lostAck}
		s.direct.writer = writer
		handle, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, string(pod.UID))
		if writer.writes != 1 {
			t.Fatal("Job admission retried")
		}
		status, readErr := s.Inspect(ctx)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if lostAck {
			if err != nil || handle == nil || !status.Present {
				t.Fatal("confirmed Job admission lost ownership", err)
			}
			current := &batchv1.Job{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(job), current); err != nil || current.Annotations[maintenanceExecutionAnnotation] != status.ExecutionID {
				t.Fatal("Job execution does not match reservation", err)
			}
		} else if !errors.Is(err, ErrUnavailable) || handle != nil || status.Present {
			t.Fatal("unconfirmed Job admission acquired maintenance", err)
		}
	}
}

func TestMaintenanceJobAdmissionConcurrentAttemptsHaveOneOwner(t *testing.T) {
	s, c, pod, job := writerFixture(t)
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, string(pod.UID))
			results <- err
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal("unexpected concurrent admission error", err)
		}
	}
	if successes != 1 {
		t.Fatal("Job did not have one owner", successes)
	}
	status, err := s.Inspect(ctx)
	if err != nil || !status.Present {
		t.Fatal("winner did not own maintenance", err)
	}
	current := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(job), current); err != nil || current.Annotations[maintenanceExecutionAnnotation] != status.ExecutionID {
		t.Fatal("concurrent Job owner mismatch", err)
	}
}
