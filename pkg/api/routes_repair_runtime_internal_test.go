package api

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

type delayedRepairPodClient struct {
	client.Client
	repairPodName string
	missed        bool
}

func (c *delayedRepairPodClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key.Name == c.repairPodName {
		if !c.missed {
			c.missed = true
			return k8serrors.NewNotFound(schema.GroupResource{Resource: "pods"}, key.Name)
		}
		if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		obj.(*corev1.Pod).Status.Phase = corev1.PodSucceeded
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func repairRunnerFixture(t *testing.T, extra ...runtime.Object) (*kubernetesRuntimeRepairRunner, *kyberv1.Agent, RuntimeRepairPlan) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &kyberv1.Agent{
		TypeMeta:   metav1.TypeMeta{APIVersion: kyberv1.GroupVersion.String(), Kind: "Agent"},
		ObjectMeta: metav1.ObjectMeta{Name: "repair-me", Namespace: "kyber-system", UID: "agent-uid"},
		Spec:       kyberv1.AgentSpec{Machine: "worker-1", Runtime: "codex"},
	}
	active := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-repair-me", Namespace: "kyber-system"},
		Spec:       corev1.PodSpec{NodeName: "worker-node", Containers: []corev1.Container{{Name: "agent", Image: "old"}}},
	}
	objects := []runtime.Object{agent, active}
	objects = append(objects, extra...)
	s := &Server{K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build(), Namespace: "kyber-system"}
	plan := RuntimeRepairPlan{
		Image:          "kyber/codex:test",
		PackageName:    "@openai/codex",
		BinaryName:     "codex",
		Version:        "0.150.1",
		PackagePath:    "/usr/lib/node_modules/@openai/codex",
		ExecutablePath: "/usr/bin/codex",
	}
	return &kubernetesRuntimeRepairRunner{server: s}, agent, plan
}

func TestBuildRuntimeRepairPodUsesAgentPVCAndNode(t *testing.T) {
	runner, agent, plan := repairRunnerFixture(t)
	pod, err := runner.buildPod(context.Background(), agent, plan)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.NodeName != "worker-node" || pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("repair scheduling = node %q restart %q", pod.Spec.NodeName, pod.Spec.RestartPolicy)
	}
	if got := pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; got != "agent-repair-me-pv" {
		t.Fatalf("repair PVC = %q", got)
	}
	container := pod.Spec.Containers[0]
	if container.Image != plan.Image || container.Command[0] != "/usr/local/bin/kyber-runtime-repair" {
		t.Fatalf("repair container = %+v", container)
	}
	if container.Args[1] != plan.PackageName || container.Args[5] != plan.ExecutablePath {
		t.Fatalf("repair args = %v", container.Args)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("repair pod must not receive a service-account token")
	}
	if got := container.Resources.Limits.Memory().String(); got != "1Gi" {
		t.Fatalf("repair memory limit = %q, want 1Gi", got)
	}
}

func TestRuntimeRepairExistingPodReturnsConflict(t *testing.T) {
	existing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: runtimeRepairPodName("repair-me"), Namespace: "kyber-system"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "repair", Image: "existing"}}},
	}
	runner, agent, plan := repairRunnerFixture(t, existing)
	_, err := runner.Run(context.Background(), agent, plan)
	if !errors.Is(err, ErrRuntimeRepairInProgress) {
		t.Fatalf("Run() error = %v, want ErrRuntimeRepairInProgress", err)
	}
}

func TestRuntimeRepairToleratesCacheMissImmediatelyAfterCreate(t *testing.T) {
	runner, agent, plan := repairRunnerFixture(t)
	base := runner.server.K8sClient
	runner.server.K8sClient = &delayedRepairPodClient{
		Client:        base,
		repairPodName: runtimeRepairPodName(agent.Name),
	}
	output, err := runner.Run(context.Background(), agent, plan)
	if err != nil {
		t.Fatalf("Run() after transient cache miss: %v", err)
	}
	if output != "repair completed and executable verified" {
		t.Fatalf("Run() output = %q", output)
	}
}

func TestBuildRuntimeRepairPodFallsBackToMachineWhenAgentPodAbsent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &kyberv1.Agent{
		TypeMeta:   metav1.TypeMeta{APIVersion: kyberv1.GroupVersion.String(), Kind: "Agent"},
		ObjectMeta: metav1.ObjectMeta{Name: "podless", Namespace: "kyber-system", UID: "podless-uid"},
		Spec:       kyberv1.AgentSpec{Machine: "worker-1", Runtime: "codex"},
	}
	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"},
		Status:     kyberv1.MachineStatus{NodeName: "fallback-node"},
	}
	s := &Server{K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agent, machine).Build(), Namespace: "kyber-system"}
	runner := &kubernetesRuntimeRepairRunner{server: s}
	pod, err := runner.buildPod(context.Background(), agent, RuntimeRepairPlan{
		Image: "kyber/codex:test", PackageName: "@openai/codex", BinaryName: "codex",
		PackagePath: "/usr/lib/node_modules/@openai/codex", ExecutablePath: "/usr/bin/codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.NodeName != "fallback-node" {
		t.Fatalf("repair pod node = %q, want fallback-node", pod.Spec.NodeName)
	}
}
