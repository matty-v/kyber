package updates

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func controlPlaneDeployment(annotations map[string]string, image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "kyber-control-plane",
			Namespace:   "kyber-system",
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "control-plane", Image: image}}},
			},
		},
	}
}

func newApplier(objs ...client.Object) *Applier {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()
	return &Applier{
		Client:                 c,
		Namespace:              "kyber-system",
		ControlPlaneDeployment: "kyber-control-plane",
		ReleaseName:            "kyber-canary",
		ChartRef:               "oci://ghcr.io/matty-v/charts/kyber",
		ServiceAccount:         "kyber-self-upgrade",
		HealthURL:              "http://kyber-control-plane:8080/healthz",
	}
}

const helmOwned = "meta.helm.sh/release-name"
const argoOwned = "argocd.argoproj.io/tracking-id"

func TestApplier_Configured(t *testing.T) {
	a := newApplier()
	if !a.Configured() {
		t.Error("Configured() = false on a fully-populated applier")
	}
	a.ServiceAccount = ""
	if a.Configured() {
		t.Error("Configured() = true with no ServiceAccount; a partial configuration must read as off")
	}
	var nilApplier *Applier
	if nilApplier.Configured() {
		t.Error("Configured() = true on a nil applier")
	}
}

// The clamp that protects ArgoCD clusters. Self-upgrading there would appear to
// work and then be reverted on the next sync.
func TestApplier_RefusesOnArgoCDManagedCluster(t *testing.T) {
	a := newApplier(controlPlaneDeployment(map[string]string{argoOwned: "kyber:apps/Deployment:kyber-system/cp"}, "ghcr.io/x:1.0.1"))

	_, err := a.Start(context.Background(), "1.0.2", DefaultPolicy())
	if err == nil {
		t.Fatal("Start() = nil, want a refusal on an ArgoCD-managed cluster")
	}
	if !strings.Contains(err.Error(), "ArgoCD") {
		t.Errorf("error should explain ArgoCD, got: %v", err)
	}
	assertNoJobs(t, a)
}

// Unknown ownership is not a licence to start mutating.
func TestApplier_RefusesWhenOwnershipUnknown(t *testing.T) {
	a := newApplier(controlPlaneDeployment(nil, "ghcr.io/x:1.0.1"))

	if _, err := a.Start(context.Background(), "1.0.2", DefaultPolicy()); err == nil {
		t.Fatal("Start() = nil, want a refusal when ownership cannot be determined")
	}
	assertNoJobs(t, a)
}

func TestApplier_RefusesWhenPinnedToAnotherVersion(t *testing.T) {
	a := newApplier(controlPlaneDeployment(map[string]string{helmOwned: "kyber-canary"}, "ghcr.io/x:1.0.1"))
	policy := DefaultPolicy()
	policy.PinnedVersion = "1.0.1"

	_, err := a.Start(context.Background(), "1.0.2", policy)
	if err == nil {
		t.Fatal("Start() = nil, want a refusal while pinned")
	}
	if !strings.Contains(err.Error(), "pinned to 1.0.1") {
		t.Errorf("error should name the pin, got: %v", err)
	}
	assertNoJobs(t, a)
}

// Installing exactly what you are pinned to is not a contradiction — it is how
// a cluster held at a version gets repaired onto it.
func TestApplier_AllowsInstallingThePinnedVersion(t *testing.T) {
	a := newApplier(controlPlaneDeployment(map[string]string{helmOwned: "kyber-canary"}, "ghcr.io/x:1.0.1"))
	policy := DefaultPolicy()
	policy.PinnedVersion = "1.0.2"

	if _, err := a.Start(context.Background(), "1.0.2", policy); err != nil {
		t.Fatalf("Start() = %v, want nil when installing the pinned version", err)
	}
}

func TestApplier_RefusesAnUnparseableVersion(t *testing.T) {
	a := newApplier(controlPlaneDeployment(map[string]string{helmOwned: "kyber-canary"}, "ghcr.io/x:1.0.1"))

	if _, err := a.Start(context.Background(), "1.0.1-22-gabe5dbe", DefaultPolicy()); err == nil {
		t.Fatal("Start() = nil, want a refusal for a git-describe string")
	}
	assertNoJobs(t, a)
}

func TestApplier_JobRunsTheLiveControlPlaneImage(t *testing.T) {
	const running = "ghcr.io/matty-v/kyber-control-plane:1.0.1"
	a := newApplier(controlPlaneDeployment(map[string]string{helmOwned: "kyber-canary"}, running))

	run, err := a.Start(context.Background(), "1.0.2", DefaultPolicy())
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if run.Phase != PhasePending {
		t.Errorf("Phase = %q, want %q", run.Phase, PhasePending)
	}

	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: "kyber-system", Name: run.JobName}
	if err := a.Client.Get(context.Background(), key, job); err != nil {
		t.Fatalf("upgrade job not created: %v", err)
	}
	container := job.Spec.Template.Spec.Containers[0]
	// The CURRENT image, not the target's: the target image is not on this
	// cluster yet, and an unverified binary should not be running the upgrade.
	if container.Image != running {
		t.Errorf("job image = %q, want the running control-plane image %q", container.Image, running)
	}
	if container.Command[0] != "/usr/local/bin/kyber-upgrade" {
		t.Errorf("job command = %v, want the kyber-upgrade entrypoint", container.Command)
	}
	if job.Spec.Template.Spec.ServiceAccountName != "kyber-self-upgrade" {
		t.Errorf("job serviceAccountName = %q", job.Spec.Template.Spec.ServiceAccountName)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("job must not retry: a failed upgrade has already rolled back")
	}

	env := map[string]string{}
	for _, e := range container.Env {
		env[e.Name] = e.Value
	}
	for k, want := range map[string]string{
		"KYBER_UPGRADE_RELEASE":                  "kyber-canary",
		"KYBER_UPGRADE_TARGET_VERSION":           "1.0.2",
		"KYBER_UPGRADE_CHART_REF":                "oci://ghcr.io/matty-v/charts/kyber",
		"KYBER_UPGRADE_CONTROL_PLANE_DEPLOYMENT": "kyber-control-plane",
	} {
		if env[k] != want {
			t.Errorf("env %s = %q, want %q", k, env[k], want)
		}
	}
	// helm writes under $HOME; the pod is non-root with a read-only root
	// filesystem, so without these every helm command fails before doing
	// anything.
	for _, k := range []string{"HOME", "HELM_CACHE_HOME", "HELM_CONFIG_HOME", "HELM_DATA_HOME"} {
		if env[k] == "" {
			t.Errorf("env %s is unset; helm has nowhere writable to work", k)
		}
	}
}

// Two concurrent `helm upgrade`s on one release is a way to corrupt the release
// history.
func TestApplier_RefusesASecondUpgradeWhileOneIsRunning(t *testing.T) {
	running := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kyber-canary-upgrade-1-0-2",
			Namespace: "kyber-system",
			Labels: map[string]string{
				upgradeComponentLabel: upgradeComponent,
				upgradeTargetLabel:    "1.0.2",
			},
		},
		Status: batchv1.JobStatus{StartTime: &metav1.Time{Time: metav1.Now().Time}},
	}
	a := newApplier(controlPlaneDeployment(map[string]string{helmOwned: "kyber-canary"}, "ghcr.io/x:1.0.1"), running)

	_, err := a.Start(context.Background(), "1.0.3", DefaultPolicy())
	if err == nil {
		t.Fatal("Start() = nil, want a refusal while another upgrade runs")
	}
	if !strings.Contains(err.Error(), "already") {
		t.Errorf("error should say one is already in flight, got: %v", err)
	}
}

func TestJobName(t *testing.T) {
	if got := jobName("kyber-canary", "1.0.2"); got != "kyber-canary-upgrade-1-0-2" {
		t.Errorf("jobName() = %q", got)
	}
	long := jobName(strings.Repeat("a", 70), "1.0.2")
	if len(long) > 63 {
		t.Errorf("jobName() = %d chars, must fit the 63-char limit", len(long))
	}
	if strings.HasSuffix(long, "-") {
		t.Errorf("jobName() = %q, must not end in a dash", long)
	}
}

func TestRunFromJob_ReportsPhases(t *testing.T) {
	base := func() *batchv1.Job {
		return &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kyber-upgrade-1-0-2",
				Namespace: "kyber-system",
				Labels:    map[string]string{upgradeTargetLabel: "1.0.2"},
			},
		}
	}
	if got := runFromJob(base()).Phase; got != PhasePending {
		t.Errorf("fresh job phase = %q, want %q", got, PhasePending)
	}

	started := base()
	started.Status.StartTime = &metav1.Time{Time: metav1.Now().Time}
	if got := runFromJob(started).Phase; got != PhaseRunning {
		t.Errorf("started job phase = %q, want %q", got, PhaseRunning)
	}

	ok := base()
	ok.Status.StartTime = &metav1.Time{Time: metav1.Now().Time}
	ok.Status.Succeeded = 1
	if got := runFromJob(ok).Phase; got != PhaseSucceeded {
		t.Errorf("succeeded job phase = %q, want %q", got, PhaseSucceeded)
	}

	bad := base()
	bad.Status.StartTime = &metav1.Time{Time: metav1.Now().Time}
	bad.Status.Failed = 1
	run := runFromJob(bad)
	if run.Phase != PhaseFailed {
		t.Errorf("failed job phase = %q, want %q", run.Phase, PhaseFailed)
	}
	// The Job log is the upgrade record, so a failure must point at it.
	if !strings.Contains(run.Message, "kubectl logs") {
		t.Errorf("failure message should point at the job log, got: %q", run.Message)
	}
}

func TestApplier_LatestIsNilBeforeAnyRun(t *testing.T) {
	a := newApplier(controlPlaneDeployment(map[string]string{helmOwned: "kyber-canary"}, "ghcr.io/x:1.0.1"))
	run, err := a.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest() = %v", err)
	}
	if run != nil {
		t.Errorf("Latest() = %+v, want nil before any upgrade has run", run)
	}
}

func assertNoJobs(t *testing.T, a *Applier) {
	t.Helper()
	jobs := &batchv1.JobList{}
	if err := a.Client.List(context.Background(), jobs, client.InNamespace(a.Namespace)); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("a refusal still created %d job(s)", len(jobs.Items))
	}
}
