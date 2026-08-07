package agent

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func newJobsTestAgent(name string, jobs []kyberv1.AgentJob) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kyber",
			UID:       k8stypes.UID("test-uid-" + name),
		},
		Spec: kyberv1.AgentSpec{
			Machine: "test-machine",
			Runtime: "claude-code",
			Resources: kyberv1.AgentResources{
				CPU:    resource.MustParse("1"),
				Memory: resource.MustParse("2Gi"),
				Disk:   resource.MustParse("50Gi"),
			},
			Scaling: kyberv1.AgentScalingWarm,
			Secrets: kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
			Model:   "claude-sonnet-4-6",
			Jobs:    jobs,
		},
	}
}

func TestRenderJobsConfigMapData_NoJobs(t *testing.T) {
	agent := newJobsTestAgent("chewie", nil)
	data := RenderJobsConfigMapData(agent)

	if _, ok := data["crontab"]; !ok {
		t.Fatal("expected crontab key even with no jobs, got nothing")
	}
	if len(data) != 1 {
		t.Fatalf("expected exactly 1 key (crontab) with no jobs, got %d: %v", len(data), keys(data))
	}
	if !strings.Contains(data["crontab"], "SHELL=/bin/bash") {
		t.Errorf("expected SHELL header in empty crontab, got:\n%s", data["crontab"])
	}
}

func TestRenderJobsConfigMapData_HeaderInjectsDispatcherEnv(t *testing.T) {
	// Cron strips the pod env, so kyber-job-dispatch's AGENT_NAME check and
	// control-plane event POST both fail unless the renderer re-injects the
	// relevant vars as crontab KEY=VALUE header lines.
	t.Setenv("KYBER_CONTROL_PLANE_INTERNAL_URL", "http://kyber-razer-control-plane.kyber-system:8082")

	agent := newJobsTestAgent("chewie", []kyberv1.AgentJob{
		{Name: "joke", Schedule: "*/5 * * * *", Prompt: "Tell me a joke"},
	})
	crontab := RenderJobsConfigMapData(agent)["crontab"]

	if !strings.Contains(crontab, "\nAGENT_NAME=chewie\n") {
		t.Errorf("crontab missing AGENT_NAME header — cron fires will hit the AGENT_NAME_unset branch and exit. Got:\n%s", crontab)
	}
	if !strings.Contains(crontab, "\nKYBER_CONTROL_PLANE_INTERNAL_URL=http://kyber-razer-control-plane.kyber-system:8082\n") {
		t.Errorf("crontab missing KYBER_CONTROL_PLANE_INTERNAL_URL header — job-event POST silently no-ops. Got:\n%s", crontab)
	}

	// Header env lines must precede the job lines — cron only applies
	// KEY=VALUE lines that appear before the schedule lines.
	envIdx := strings.Index(crontab, "AGENT_NAME=chewie")
	jobIdx := strings.Index(crontab, "*/5 * * * * kyber")
	if envIdx < 0 || jobIdx < 0 || envIdx > jobIdx {
		t.Errorf("AGENT_NAME env line must come before job lines; envIdx=%d jobIdx=%d", envIdx, jobIdx)
	}
}

func TestRenderJobsConfigMapData_CronTZ_RenderedWhenSet(t *testing.T) {
	// kyber#177: CRON_TZ line must be present and precede schedule lines so
	// every "0 7 * * *"-style job is interpreted in the operator's TZ rather
	// than UTC. Cron only honors KEY=VALUE lines that appear ABOVE schedules.
	t.Setenv("KYBER_AGENT_TIMEZONE", "America/Denver")

	agent := newJobsTestAgent("chewie", []kyberv1.AgentJob{
		{Name: "morning", Schedule: "0 7 * * *", Prompt: "Good morning"},
	})
	crontab := RenderJobsConfigMapData(agent)["crontab"]

	if !strings.Contains(crontab, "\nCRON_TZ=America/Denver\n") {
		t.Errorf("crontab missing CRON_TZ line; got:\n%s", crontab)
	}
	tzIdx := strings.Index(crontab, "CRON_TZ=")
	jobIdx := strings.Index(crontab, "0 7 * * * kyber")
	if tzIdx < 0 || jobIdx < 0 || tzIdx > jobIdx {
		t.Errorf("CRON_TZ must precede schedule lines; tzIdx=%d jobIdx=%d", tzIdx, jobIdx)
	}
}

func TestRenderJobsConfigMapData_CronTZ_OmittedWhenUnset(t *testing.T) {
	// When KYBER_AGENT_TIMEZONE is not set, the renderer must not emit a
	// CRON_TZ line — keeps backward-compat with chart deployments that
	// haven't bumped to a values file with runtime.timezone, and keeps unit
	// tests that don't set the env var unaffected.
	t.Setenv("KYBER_AGENT_TIMEZONE", "")

	agent := newJobsTestAgent("chewie", []kyberv1.AgentJob{
		{Name: "morning", Schedule: "0 7 * * *", Prompt: "Good morning"},
	})
	crontab := RenderJobsConfigMapData(agent)["crontab"]

	if strings.Contains(crontab, "CRON_TZ=") {
		t.Errorf("crontab should not contain CRON_TZ when env unset; got:\n%s", crontab)
	}
}

func TestRenderJobsConfigMapData_MultipleJobs_SortedAndEscapeSafe(t *testing.T) {
	// Purposely unsorted + jobs with tricky prompt content (quotes, newlines,
	// shell metachars) to catch any escaping mistakes. These prompts never
	// touch the crontab line — they live in prompt-<name> files.
	jobs := []kyberv1.AgentJob{
		{Name: "zebra", Schedule: "0 9 * * *", Prompt: "echo $HOME"},
		{Name: "alpha", Schedule: "*/5 * * * *", Prompt: "line1\n\"quoted\"\n`tick`"},
		{Name: "mid", Schedule: "30 8 * * 1-5", Prompt: "plain text", Exclusive: true},
	}
	agent := newJobsTestAgent("chewie", jobs)

	data := RenderJobsConfigMapData(agent)

	crontab := data["crontab"]
	if !strings.Contains(crontab, "*/5 * * * * kyber /usr/local/bin/kyber-job-dispatch alpha\n") {
		t.Errorf("expected alpha line in crontab, got:\n%s", crontab)
	}
	if !strings.Contains(crontab, "30 8 * * 1-5 kyber /usr/local/bin/kyber-job-dispatch --exclusive mid\n") {
		t.Errorf("expected mid line (exclusive) in crontab, got:\n%s", crontab)
	}
	if !strings.Contains(crontab, "0 9 * * * kyber /usr/local/bin/kyber-job-dispatch zebra\n") {
		t.Errorf("expected zebra line in crontab, got:\n%s", crontab)
	}

	// Sort stability: alpha must come before mid must come before zebra.
	// mid has --exclusive, so the substring needs to account for the flag
	// position relative to the name.
	alphaIdx := strings.Index(crontab, "/kyber-job-dispatch alpha")
	midIdx := strings.Index(crontab, "--exclusive mid")
	zebraIdx := strings.Index(crontab, "/kyber-job-dispatch zebra")
	if !(alphaIdx < midIdx && midIdx < zebraIdx) {
		t.Errorf("jobs not sorted by name; got indices alpha=%d mid=%d zebra=%d", alphaIdx, midIdx, zebraIdx)
	}

	// Prompts land verbatim in per-job keys — no escaping mutation.
	if got, want := data["prompt-alpha"], "line1\n\"quoted\"\n`tick`"; got != want {
		t.Errorf("prompt-alpha = %q, want %q", got, want)
	}
	if got, want := data["prompt-zebra"], "echo $HOME"; got != want {
		t.Errorf("prompt-zebra = %q, want %q", got, want)
	}

	// Crontab must NOT contain the raw prompt — that's the whole point of
	// putting it in a side file. A regression here is a shell-injection risk.
	if strings.Contains(crontab, "echo $HOME") {
		t.Error("crontab leaked raw prompt text — prompts must live in prompt-<name> files only")
	}
}

func TestRenderJobsConfigMapData_StableAcrossCalls(t *testing.T) {
	// Controller reconciliation compares live vs. desired Data; any
	// nondeterminism here causes infinite no-op patching.
	jobs := []kyberv1.AgentJob{
		{Name: "b", Schedule: "0 0 * * *", Prompt: "B"},
		{Name: "a", Schedule: "0 0 * * *", Prompt: "A"},
	}
	agent := newJobsTestAgent("chewie", jobs)

	first := RenderJobsConfigMapData(agent)
	for i := 0; i < 5; i++ {
		got := RenderJobsConfigMapData(agent)
		if got["crontab"] != first["crontab"] {
			t.Fatalf("render %d drifted from first render\nfirst:\n%s\ngot:\n%s",
				i, first["crontab"], got["crontab"])
		}
	}
}

func TestReconcileJobsConfigMap_CreatesThenUpdatesThenNoops(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = kyberv1.AddToScheme(scheme)

	agent := newJobsTestAgent("chewie", []kyberv1.AgentJob{
		{Name: "morning", Schedule: "0 9 * * *", Prompt: "good morning"},
	})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(agent).
		Build()

	r := &AgentReconciler{Client: fakeClient, Scheme: scheme}

	// ---- First pass: create ----
	if err := r.reconcileJobsConfigMap(context.Background(), agent); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	cm := &corev1.ConfigMap{}
	key := k8stypes.NamespacedName{Name: "chewie-jobs", Namespace: "kyber"}
	if err := fakeClient.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("ConfigMap not created: %v", err)
	}
	if cm.Data["prompt-morning"] != "good morning" {
		t.Errorf("prompt-morning = %q, want %q", cm.Data["prompt-morning"], "good morning")
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Name != "chewie" {
		t.Errorf("expected owner reference to agent chewie, got %v", cm.OwnerReferences)
	}
	initialResourceVersion := cm.ResourceVersion

	// ---- Second pass: no changes, must not patch (resourceVersion stable) ----
	if err := r.reconcileJobsConfigMap(context.Background(), agent); err != nil {
		t.Fatalf("noop reconcile: %v", err)
	}
	if err := fakeClient.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("ConfigMap disappeared: %v", err)
	}
	if cm.ResourceVersion != initialResourceVersion {
		t.Errorf("reconcile patched on no-op; resourceVersion %s → %s", initialResourceVersion, cm.ResourceVersion)
	}

	// ---- Third pass: mutate spec, must patch ----
	agent.Spec.Jobs[0].Prompt = "good afternoon"
	if err := fakeClient.Update(context.Background(), agent); err != nil {
		t.Fatalf("updating agent: %v", err)
	}
	if err := r.reconcileJobsConfigMap(context.Background(), agent); err != nil {
		t.Fatalf("mutating reconcile: %v", err)
	}
	if err := fakeClient.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("ConfigMap missing after patch: %v", err)
	}
	if cm.Data["prompt-morning"] != "good afternoon" {
		t.Errorf("after patch: prompt-morning = %q, want %q", cm.Data["prompt-morning"], "good afternoon")
	}
}

func TestReconcileJobsConfigMap_RemovesStalePromptKeys(t *testing.T) {
	// Deleting a job from spec.jobs must remove its prompt-<name> key from
	// the ConfigMap. Regression risk: a merge-style patch that only adds keys
	// would leave stale prompts around forever.
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = kyberv1.AddToScheme(scheme)

	agent := newJobsTestAgent("chewie", []kyberv1.AgentJob{
		{Name: "morning", Schedule: "0 9 * * *", Prompt: "dawn"},
		{Name: "evening", Schedule: "0 18 * * *", Prompt: "dusk"},
	})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(agent).
		Build()
	r := &AgentReconciler{Client: fakeClient, Scheme: scheme}

	if err := r.reconcileJobsConfigMap(context.Background(), agent); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	// Drop 'evening'
	agent.Spec.Jobs = agent.Spec.Jobs[:1]
	if err := r.reconcileJobsConfigMap(context.Background(), agent); err != nil {
		t.Fatalf("post-delete reconcile: %v", err)
	}

	cm := &corev1.ConfigMap{}
	_ = fakeClient.Get(context.Background(), k8stypes.NamespacedName{Name: "chewie-jobs", Namespace: "kyber"}, cm)
	if _, present := cm.Data["prompt-evening"]; present {
		t.Error("stale prompt-evening key survived job deletion — reconcile must replace Data, not merge")
	}
	if _, present := cm.Data["prompt-morning"]; !present {
		t.Error("prompt-morning removed by reconcile that should have kept it")
	}
}

var _ client.Client = (client.Client)(nil) // guard against unused-import on lint

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
