package operatorcredentials

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSupervisedJobQuiescenceRequiresExactTerminalPods(t *testing.T) {
	for _, scenario := range []string{"stopped", "running", "missing-pod", "node-lost", "extra-running"} {
		t.Run(scenario, func(t *testing.T) {
			_, c, pod, job := writerFixture(t)
			ctx := context.Background()
			if scenario != "running" {
				stopWriterFixture(t, c, pod, job)
			}
			switch scenario {
			case "missing-pod":
				if c.Delete(ctx, pod) != nil {
					t.Fatal("fixture deletion failed")
				}
			case "node-lost":
				pod.Status.Reason = "NodeLost"
				if c.Status().Update(ctx, pod) != nil {
					t.Fatal("fixture status failed")
				}
			case "extra-running":
				extra := pod.DeepCopy()
				extra.Name = "another-pod"
				extra.ResourceVersion = ""
				extra.UID = "11111111-1111-4111-8111-111111111111"
				extra.Status = corev1.PodStatus{Phase: corev1.PodRunning}
				if c.Create(ctx, extra) != nil {
					t.Fatal("fixture extra pod failed")
				}
			}
			current := &batchv1.Job{}
			if c.Get(ctx, client.ObjectKeyFromObject(job), current) != nil {
				t.Fatal("fixture Job unavailable")
			}
			stopped, err := SupervisedJobStopped(ctx, c, *current, "upgrade")
			if err != nil || stopped != (scenario == "stopped") {
				t.Fatal("incorrect quiescence evidence", scenario, err)
			}
		})
	}
}
