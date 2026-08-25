package agent

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// agentJobTimezone returns the IANA timezone (e.g. "America/Denver") used
// to interpret cron schedules in the rendered crontab. Sourced from the
// KYBER_AGENT_TIMEZONE env var, which the Helm chart sets from
// values.runtime.timezone. Empty string means "leave it to the pod's
// /etc/localtime" — which falls back to UTC in stock k8s images and is the
// bug this knob fixes.
//
// When set, the controller emits a literal `CRON_TZ=...` line at the top of
// the crontab. Linux cron interprets every schedule below it in that TZ and
// honors DST automatically — no manual UTC-equivalent math each season.
func agentJobTimezone() string {
	return os.Getenv("KYBER_AGENT_TIMEZONE")
}

// JobsConfigMapName returns the conventional name of the per-agent jobs
// ConfigMap: <agent>-jobs. The pod's volumeMount at /kyber/jobs-src pulls
// from this ConfigMap; entrypoint.sh copies crontab to /persist/cron/cron.d/
// and the dispatcher reads the prompt-<name> files by job name.
func JobsConfigMapName(agentName string) string {
	return agentName + "-jobs"
}

// JobsConfigMapLabels returns the labels applied to the per-agent jobs
// ConfigMap. "secret-kind" is reused from the user-secrets convention so
// label-selector code doesn't need a second path.
func JobsConfigMapLabels(agentName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "kyber-controller",
		"kyber.io/agent":               agentName,
		"kyber.io/configmap-kind":      "agent-jobs",
	}
}

// RenderJobsConfigMapData produces the ConfigMap.Data map for the given
// agent. The resulting map contains exactly one "crontab" key plus one
// "prompt-<name>" key per job. When spec.jobs is empty the returned map is
// still populated with an empty crontab — this keeps the ConfigMap present
// across transitions (job added then removed) so the pod's volumeMount path
// stays stable and doesn't trip MountVolume errors.
//
// The crontab format targets /etc/cron.d (six fields: schedule + user +
// command), which requires the explicit user field. All lines run as the
// "kyber" user — the agent's non-root runtime user. The command invokes
// kyber-job-dispatch with the job name only; the dispatcher resolves the
// prompt from /kyber/jobs-src/prompt-<name>, so we never shell-escape
// operator-supplied text into the crontab line.
func RenderJobsConfigMapData(agent *kyberv1.Agent) map[string]string {
	data := map[string]string{}

	// Sort jobs by name for a stable render order — controller's reconcile
	// loop compares ConfigMap contents and must not churn on map iteration
	// ordering.
	jobs := append([]kyberv1.AgentJob(nil), agent.Spec.Jobs...)
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })

	var crontab strings.Builder
	crontab.WriteString("# Rendered by kyber-controller from Agent " + agent.Namespace + "/" + agent.Name + ".\n")
	crontab.WriteString("# Managed file — edits will be overwritten on the next reconcile.\n")
	crontab.WriteString("SHELL=/bin/bash\n")
	crontab.WriteString("PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n")
	// CRON_TZ controls how every schedule below is interpreted (Linux cron
	// honors per-file CRON_TZ). Without it cron uses the pod's localtime,
	// which is UTC by default in stock k8s images — so "0 7 * * *" fires at
	// 07:00 UTC, not 07:00 in the operator's local timezone, and silently
	// shifts an hour twice a year across DST. See kyber#177.
	if tz := agentJobTimezone(); tz != "" {
		crontab.WriteString("CRON_TZ=" + tz + "\n")
	}
	// Cron strips the pod env; re-inject the vars kyber-job-dispatch needs.
	// AGENT_NAME gates the dispatcher (pod_builder.go also sets this on the
	// pod, but cron doesn't inherit that). KYBER_CONTROL_PLANE_INTERNAL_URL
	// is how the dispatcher POSTs status.jobs[] events back to the CR; empty
	// value is tolerated by the script (it skips the POST) for local dev.
	crontab.WriteString("AGENT_NAME=" + agent.Name + "\n")
	crontab.WriteString("KYBER_CONTROL_PLANE_INTERNAL_URL=" + controlPlaneInternalURL() + "\n")
	crontab.WriteString("\n")

	for _, j := range jobs {
		// Flags precede the job name; the dispatcher stops flag parsing at the
		// first non-flag argument.
		args := ""
		if j.Exclusive {
			args += "--exclusive "
		}
		if j.ClearContextAfter {
			args += "--clear-context-after "
		}
		args += j.Name
		crontab.WriteString(fmt.Sprintf(
			"%s kyber /usr/local/bin/kyber-job-dispatch %s\n",
			j.Schedule, args,
		))
		data["prompt-"+j.Name] = j.Prompt
	}
	// /etc/cron.d entries require a trailing newline after the final line or
	// Debian cron will silently drop it. Builder handles this via the \n on
	// every line above; the blank line between header and jobs covers the
	// no-jobs case as well.

	data["crontab"] = crontab.String()
	return data
}

// reconcileJobsConfigMap upserts the <agent>-jobs ConfigMap with the rendered
// contents of spec.jobs. The ConfigMap is created on first reconcile and
// patched whenever the rendered Data differs from the live state.
//
// The controller owns this ConfigMap via SetControllerReference — it's
// garbage-collected when the Agent is deleted, and never needs manual
// cleanup in the finalizer.
func (r *AgentReconciler) reconcileJobsConfigMap(ctx context.Context, agent *kyberv1.Agent) error {
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      JobsConfigMapName(agent.Name),
			Namespace: agent.Namespace,
			Labels:    JobsConfigMapLabels(agent.Name),
		},
		Data: RenderJobsConfigMapData(agent),
	}
	if err := ctrl.SetControllerReference(agent, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting jobs ConfigMap owner reference: %w", err)
	}

	existing := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}
	err := r.Get(ctx, key, existing)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("fetching jobs ConfigMap: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("creating jobs ConfigMap: %w", err)
		}
		return nil
	}

	// Patch only when Data or Labels drifted. Avoids a reconcile storm of
	// no-op PATCHes on clusters where the CM is touched by other controllers.
	if equality.Semantic.DeepEqual(existing.Data, desired.Data) &&
		equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = desired.Data
	existing.Labels = desired.Labels
	if err := r.Patch(ctx, existing, patch); err != nil {
		return fmt.Errorf("patching jobs ConfigMap: %w", err)
	}
	return nil
}
