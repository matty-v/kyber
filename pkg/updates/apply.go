package updates

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Labels identifying upgrade Jobs. The component label is how Status finds
// previous runs; the target label records what each one was trying to install.
const (
	upgradeComponentLabel = "app.kubernetes.io/component"
	upgradeComponent      = "self-upgrade"
	upgradeTargetLabel    = "kyber.io/upgrade-target"
)

// upgradeJobTTL keeps a finished Job (and its log) around long enough to be
// read. The log IS the upgrade record, so this is deliberately generous.
const upgradeJobTTL int32 = 7 * 24 * 60 * 60

// Phase is where an upgrade run has got to.
type Phase string

const (
	PhasePending   Phase = "pending"
	PhaseRunning   Phase = "running"
	PhaseSucceeded Phase = "succeeded"
	PhaseFailed    Phase = "failed"
)

// Run is the operator-facing state of one upgrade attempt.
type Run struct {
	JobName       string     `json:"jobName"`
	TargetVersion string     `json:"targetVersion"`
	Phase         Phase      `json:"phase"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`

	// Message explains a failure in the operator's terms. The detail lives in
	// the Job's log; this says where to look.
	Message string `json:"message,omitempty"`
}

// Applier starts and reports on self-upgrades. It never runs Helm itself — it
// creates a Job that does, for the reason the whole design rests on: the
// control plane is the process being replaced.
type Applier struct {
	Client    client.Client
	Namespace string

	// ControlPlaneDeployment is both what ownership detection inspects and
	// where the Job's image comes from — the Job runs the image the control
	// plane is running right now, not the target's. The target image does not
	// exist on this cluster yet, and pulling an unknown image to run the
	// upgrade would put an unverified binary in charge of the upgrade.
	ControlPlaneDeployment string

	// ReleaseName and ChartRef identify what to upgrade and where from.
	ReleaseName string
	ChartRef    string

	// ServiceAccount is the identity the Job runs as. It needs broad rights —
	// a Helm upgrade rewrites every resource in the chart, including CRDs and
	// cluster-scoped RBAC — so the chart only creates it when self-upgrade is
	// explicitly enabled.
	ServiceAccount string

	// HealthURL is the in-cluster control-plane health endpoint the Job probes
	// after the rollout.
	HealthURL string

	Logger *slog.Logger
}

func (a *Applier) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

// Configured reports whether this control plane can start an upgrade at all.
// Surfaced as `applySupported` so the PWA renders an honest affordance instead
// of discovering a 503 behind a button.
func (a *Applier) Configured() bool {
	return a != nil && a.Client != nil && a.Namespace != "" &&
		a.ControlPlaneDeployment != "" && a.ReleaseName != "" &&
		a.ChartRef != "" && a.ServiceAccount != ""
}

// Start creates the upgrade Job for targetVersion.
//
// The guards run in the order an operator would want them explained: is this
// cluster allowed to self-upgrade, is the version sane, and is another upgrade
// already in flight.
func (a *Applier) Start(ctx context.Context, targetVersion string, policy Policy) (*Run, error) {
	if !a.Configured() {
		return nil, fmt.Errorf("self-upgrade is not configured on this control plane")
	}

	owner := DetectManagedBy(ctx, a.Client, a.Namespace, a.ControlPlaneDeployment)
	if !owner.CanSelfUpgrade() {
		return nil, fmt.Errorf("%s", owner.Reason())
	}
	if policy.PinnedVersion != "" && policy.PinnedVersion != targetVersion {
		return nil, fmt.Errorf("this cluster is pinned to %s; clear the pin before installing %s",
			policy.PinnedVersion, targetVersion)
	}
	version, err := ParseVersion(targetVersion)
	if err != nil {
		return nil, fmt.Errorf("target version %q is not a version this cluster can install: %w", targetVersion, err)
	}
	// The channel decides what may be installed here, not the parser — a
	// pre-release is a legitimate target on the canary and must never be one
	// on a cluster tracking releases. Without this, naming a main build
	// explicitly in the request body would bypass the channel entirely.
	if !policy.Channel.Accepts(version) {
		return nil, fmt.Errorf(
			"%s is a head-of-main build and this cluster follows the %q channel, which takes published releases only. "+
				"Switch the channel to %q if you meant to track main",
			version, policy.Channel, ChannelMain)
	}
	target := version.String()

	// Node-capability preflight (kyber#78). Deliberately after the pin and
	// channel checks — those are about what the operator asked for; this is
	// about whether the cluster can carry it — and BEFORE the Job is created,
	// because the whole point is to refuse rather than to land and break every
	// agent at once. See preflight.go for why an unreadable node never blocks.
	if err := CheckNodeCapability(ctx, a.Client); err != nil {
		return nil, err
	}

	// One at a time. Two concurrent `helm upgrade`s on one release is a way to
	// corrupt the release history, and the second would be operating on values
	// the first is in the middle of replacing.
	if inflight, err := a.inFlight(ctx); err != nil {
		return nil, err
	} else if inflight != nil {
		return inflight, fmt.Errorf("an upgrade to %s is already %s (job %s)",
			inflight.TargetVersion, inflight.Phase, inflight.JobName)
	}

	image, err := a.controlPlaneImage(ctx)
	if err != nil {
		return nil, err
	}

	job := a.buildJob(target, image)
	if err := a.Client.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// A previous attempt at this exact version left its Job behind.
			// Report it rather than silently reusing a finished run's result.
			return nil, fmt.Errorf("a Job for %s already exists (%s); delete it to retry",
				target, job.Name)
		}
		return nil, fmt.Errorf("create upgrade job: %w", err)
	}
	a.logger().Info("self-upgrade job created",
		"job", job.Name, "targetVersion", target, "image", image, "chart", a.ChartRef)

	return &Run{JobName: job.Name, TargetVersion: target, Phase: PhasePending}, nil
}

// Latest returns the most recent upgrade run, or nil when there has never been
// one.
func (a *Applier) Latest(ctx context.Context) (*Run, error) {
	if !a.Configured() {
		return nil, nil
	}
	runs, err := a.list(ctx)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return runs[0], nil
}

// inFlight returns a run that has not finished, if there is one.
func (a *Applier) inFlight(ctx context.Context) (*Run, error) {
	runs, err := a.list(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		if r.Phase == PhasePending || r.Phase == PhaseRunning {
			return r, nil
		}
	}
	return nil, nil
}

// list returns every upgrade run, newest first.
func (a *Applier) list(ctx context.Context) ([]*Run, error) {
	jobs := &batchv1.JobList{}
	err := a.Client.List(ctx, jobs,
		client.InNamespace(a.Namespace),
		client.MatchingLabels{upgradeComponentLabel: upgradeComponent})
	if err != nil {
		return nil, fmt.Errorf("list upgrade jobs: %w", err)
	}
	out := make([]*Run, 0, len(jobs.Items))
	for i := range jobs.Items {
		out = append(out, runFromJob(&jobs.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool {
		return jobCreatedAfter(jobs.Items, out[i].JobName, out[j].JobName)
	})
	return out, nil
}

func jobCreatedAfter(items []batchv1.Job, a, b string) bool {
	var ta, tb time.Time
	for i := range items {
		switch items[i].Name {
		case a:
			ta = items[i].CreationTimestamp.Time
		case b:
			tb = items[i].CreationTimestamp.Time
		}
	}
	return ta.After(tb)
}

func runFromJob(job *batchv1.Job) *Run {
	r := &Run{
		JobName:       job.Name,
		TargetVersion: job.Labels[upgradeTargetLabel],
		Phase:         PhasePending,
	}
	if job.Status.StartTime != nil {
		t := job.Status.StartTime.Time
		r.StartedAt = &t
		r.Phase = PhaseRunning
	}
	switch {
	case job.Status.Succeeded > 0:
		r.Phase = PhaseSucceeded
	case job.Status.Failed > 0:
		r.Phase = PhaseFailed
		r.Message = fmt.Sprintf("The upgrade failed and was rolled back if it had got far enough to need it. "+
			"Read the job log for what happened: kubectl logs -n %s job/%s", job.Namespace, job.Name)
	}
	if job.Status.CompletionTime != nil {
		t := job.Status.CompletionTime.Time
		r.FinishedAt = &t
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			r.Phase = PhaseFailed
			if c.Message != "" {
				r.Message = c.Message + " — see: kubectl logs -n " + job.Namespace + " job/" + job.Name
			}
		}
	}
	return r
}

func (a *Applier) controlPlaneImage(ctx context.Context) (string, error) {
	dep := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: a.Namespace, Name: a.ControlPlaneDeployment}
	if err := a.Client.Get(ctx, key, dep); err != nil {
		return "", fmt.Errorf("read control-plane Deployment %s/%s: %w", a.Namespace, a.ControlPlaneDeployment, err)
	}
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return "", fmt.Errorf("control-plane Deployment %s has no containers", a.ControlPlaneDeployment)
	}
	return dep.Spec.Template.Spec.Containers[0].Image, nil
}

// jobName is deterministic per target version, which is what makes a duplicate
// request fail loudly instead of starting a second upgrade.
func jobName(release, target string) string {
	safe := strings.ReplaceAll(target, ".", "-")
	safe = strings.ReplaceAll(safe, "+", "-")
	name := fmt.Sprintf("%s-upgrade-%s", release, safe)
	if len(name) > 63 {
		name = name[:63]
		name = strings.TrimRight(name, "-")
	}
	return name
}

func (a *Applier) buildJob(target, image string) *batchv1.Job {
	backoff := int32(0) // never retry: a failed upgrade already rolled back.
	ttl := upgradeJobTTL
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(a.ReleaseName, target),
			Namespace: a.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kyber",
				"app.kubernetes.io/managed-by": "kyber-control-plane",
				upgradeComponentLabel:          upgradeComponent,
				upgradeTargetLabel:             target,
			},
		},
		Spec: batchv1.JobSpec{
			// No retries. The Job's own failure path rolls the release back,
			// so a second attempt would start from a cluster that is already
			// where it should be — and would race whoever is reading the log
			// to find out what went wrong.
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name": "kyber",
						upgradeComponentLabel:    upgradeComponent,
						upgradeTargetLabel:       target,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: a.ServiceAccount,
					Containers: []corev1.Container{{
						Name:    "upgrade",
						Image:   image,
						Command: []string{"/usr/local/bin/kyber-upgrade"},
						Env: []corev1.EnvVar{
							{Name: "KYBER_UPGRADE_RELEASE", Value: a.ReleaseName},
							{Name: "KYBER_UPGRADE_NAMESPACE", Value: a.Namespace},
							{Name: "KYBER_UPGRADE_CHART_REF", Value: a.ChartRef},
							{Name: "KYBER_UPGRADE_TARGET_VERSION", Value: target},
							{Name: "KYBER_UPGRADE_CONTROL_PLANE_DEPLOYMENT", Value: a.ControlPlaneDeployment},
							{Name: "KYBER_UPGRADE_HEALTH_URL", Value: a.HealthURL},
							{Name: "KYBER_UPGRADE_WORKDIR", Value: "/work"},
							// helm writes cache, config and repository state
							// under $HOME by default. The root filesystem is
							// read-only and the pod runs as a non-root uid with
							// no home directory, so point all three at the
							// emptyDir. Without this every helm command fails
							// on an unwritable path before it does anything.
							{Name: "HOME", Value: "/helm"},
							{Name: "HELM_CACHE_HOME", Value: "/helm/cache"},
							{Name: "HELM_CONFIG_HOME", Value: "/helm/config"},
							{Name: "HELM_DATA_HOME", Value: "/helm/data"},
						},
						VolumeMounts: []corev1.VolumeMount{
							// The root filesystem is read-only, so the pulled
							// chart and the captured values need somewhere to
							// live. Also /tmp, which helm writes to.
							{Name: "work", MountPath: "/work"},
							{Name: "tmp", MountPath: "/tmp"},
							{Name: "helm-home", MountPath: "/helm"},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false),
							ReadOnlyRootFilesystem:   ptr(true),
							RunAsNonRoot:             ptr(true),
							RunAsUser:                ptrInt64(65532),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "helm-home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}

func ptr(b bool) *bool        { return &b }
func ptrInt64(i int64) *int64 { return &i }
