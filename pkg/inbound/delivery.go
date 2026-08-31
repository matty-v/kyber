package inbound

import (
	"context"
	"time"
)

const maxOutcomePersistenceReserve = 5 * time.Second

// WaitAgentFunc waits for the job's target agent to become Running.
type WaitAgentFunc func(ctx context.Context, job Job, timeout time.Duration) error

// DeliverJobFunc sends one envelope into the target agent runtime.
type DeliverJobFunc func(ctx context.Context, job Job) error

// NewDeliveryHandler adapts the existing queue to both webhook and internal
// request jobs. Webhooks retain their established behavior: after a bounded
// Running-phase wait they still attempt delivery. Request jobs instead stop at
// their immutable expiry and report an explicit delivery outcome.
func NewDeliveryHandler(waitAgent WaitAgentFunc, deliver DeliverJobFunc, webhookWait, deliveryTimeout time.Duration) Handler {
	return func(ctx context.Context, job Job) {
		waitFor := webhookWait
		processingDeadline := job.DeliverBefore
		if job.Kind == JobKindRequest || job.Kind == JobKindTask {
			waitFor = time.Until(job.DeliverBefore)
			if waitFor <= 0 {
				notifyDelivery(ctx, job, DeliveryAgentUnavailable)
				return
			}
			// Reserve the final five seconds—or half of a shorter remaining
			// lifetime—for persisting the delivery outcome before the store's
			// immutable TTL. Waiting or execing through DeliverBefore would make
			// the callback race physical expiry and turn a stable failure into a
			// 404 for the polling caller.
			reserve := waitFor / 2
			if reserve > maxOutcomePersistenceReserve {
				reserve = maxOutcomePersistenceReserve
			}
			processingDeadline = job.DeliverBefore.Add(-reserve)
			waitFor = time.Until(processingDeadline)
			if waitFor <= 0 {
				notifyDelivery(ctx, job, DeliveryAgentUnavailable)
				return
			}
		}

		waitCtx, cancelWait := context.WithTimeout(ctx, waitFor)
		waitErr := waitAgent(waitCtx, job, waitFor)
		cancelWait()
		if waitErr != nil && (job.Kind == JobKindRequest || job.Kind == JobKindTask) {
			notifyDelivery(ctx, job, DeliveryAgentUnavailable)
			return
		}

		deliverFor := deliveryTimeout
		if job.Kind == JobKindRequest || job.Kind == JobKindTask {
			remaining := time.Until(processingDeadline)
			if remaining <= 0 {
				notifyDelivery(ctx, job, DeliveryAgentUnavailable)
				return
			}
			if remaining < deliverFor {
				deliverFor = remaining
			}
		}
		if job.BeforeDelivery != nil {
			if err := job.BeforeDelivery(ctx); err != nil {
				notifyDelivery(ctx, job, DeliveryFailed)
				return
			}
		}
		deliverCtx, cancelDeliver := context.WithTimeout(ctx, deliverFor)
		err := deliver(deliverCtx, job)
		cancelDeliver()
		if err != nil {
			notifyDelivery(ctx, job, DeliveryFailed)
			return
		}
		notifyDelivery(ctx, job, DeliveryDispatched)
	}
}

func notifyDelivery(ctx context.Context, job Job, outcome DeliveryOutcome) {
	if (job.Kind == JobKindRequest || job.Kind == JobKindTask) && job.OnDelivery != nil {
		job.OnDelivery(ctx, outcome)
	}
}
