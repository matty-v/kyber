package selfupgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrMaintenanceReleaseUnverified = errors.New("current release and credential authority could not be verified for recovery")

type maintenanceAuthorityReader interface {
	Load(context.Context) (operatorcredentials.Snapshot, error)
}
type maintenanceHelmReader interface {
	Output(context.Context, string, ...string) (string, error)
}

// MaintenanceReleaseVerifier performs reads only. It never upgrades, rolls back,
// rewrites credentials or treats an unavailable release as safe to unlock.
type MaintenanceReleaseVerifier struct {
	namespace string
	authority maintenanceAuthorityReader
	reader    client.Reader
	helm      maintenanceHelmReader
}

func NewMaintenanceReleaseVerifier(config *rest.Config, namespace string) (*MaintenanceReleaseVerifier, error) {
	authority, err := operatorcredentials.NewAuthorityStore(config, namespace, operatorcredentials.DefaultSecretName, operatorcredentials.DefaultAuthoritySecretName)
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion, appsv1.SchemeGroupVersion})
	for _, kind := range []string{"Secret", "Pod"} {
		mapper.Add(corev1.SchemeGroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	for _, kind := range []string{"Deployment", "ReplicaSet"} {
		mapper.Add(appsv1.SchemeGroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	bounded := rest.CopyConfig(config)
	if bounded.Timeout == 0 || bounded.Timeout > 2*time.Second {
		bounded.Timeout = 2 * time.Second
	}
	reader, err := client.New(bounded, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		return nil, ErrMaintenanceReleaseUnverified
	}
	return &MaintenanceReleaseVerifier{namespace: namespace, authority: authority, reader: reader, helm: boundedMaintenanceHelm{}}, nil
}
func (v *MaintenanceReleaseVerifier) Namespace() string {
	if v == nil {
		return ""
	}
	return v.namespace
}
func (v *MaintenanceReleaseVerifier) Verify(ctx context.Context, status operatorcredentials.MaintenanceStatus) error {
	if v == nil || v.namespace == "" || v.authority == nil || v.reader == nil || v.helm == nil || !status.Present || status.Kind != operatorcredentials.MaintenanceUpgrade || status.Writer == nil {
		return ErrMaintenanceReleaseUnverified
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	selected, uid, err := v.currentAuthority(ctx)
	if err != nil {
		return ErrMaintenanceReleaseUnverified
	}
	checkpoint := status.Upgrade
	if checkpoint != nil && checkpoint.Validate() != nil {
		return ErrMaintenanceReleaseUnverified
	}
	if checkpoint == nil {
		// No upgrade effects were admitted. PrepareRecovery must still fence late
		// checkpoint writes and prove the original supervised writer stopped.
		return nil
	}
	if (checkpoint.Authority == "registry") != selected || selected && checkpoint.RegistryUID != uid {
		return ErrMaintenanceReleaseUnverified
	}
	current, err := v.releaseStatus(ctx, checkpoint.Release)
	if err != nil || current.Version < checkpoint.InitialRevision {
		return ErrMaintenanceReleaseUnverified
	}
	manifest, err := v.helm.Output(ctx, "/usr/local/bin/helm", "get", "manifest", checkpoint.Release, "--namespace", v.namespace, "--revision", strconv.Itoa(current.Version))
	if err != nil || len(manifest) > maxCompatibilityManifestBytes {
		return ErrMaintenanceReleaseUnverified
	}
	var expected *appsv1.Deployment
	if selected {
		expected, err = credentialCompatibleDeployment(manifest, v.namespace, checkpoint.ControlPlaneDeployment)
	} else {
		expected, err = maintenanceDeployment(manifest, v.namespace, checkpoint.ControlPlaneDeployment)
	}
	if err != nil {
		return ErrMaintenanceReleaseUnverified
	}
	live := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: v.namespace, Name: checkpoint.ControlPlaneDeployment}
	if err := v.reader.Get(ctx, key, live); err != nil {
		return ErrMaintenanceReleaseUnverified
	}
	if selected {
		live.APIVersion = "apps/v1"
		live.Kind = "Deployment"
		encoded, _ := json.Marshal(live)
		if _, err := credentialCompatibleDeployment(string(encoded), v.namespace, live.Name); err != nil {
			return ErrMaintenanceReleaseUnverified
		}
	}
	desired := int32(1)
	if live.Spec.Replicas != nil {
		desired = *live.Spec.Replicas
	}
	if live.UID == "" || live.ResourceVersion == "" || live.DeletionTimestamp != nil || desired < 1 || desired > 128 || live.Status.ObservedGeneration != live.Generation || live.Status.Replicas != desired || live.Status.UpdatedReplicas != desired || live.Status.ReadyReplicas != desired || live.Status.AvailableReplicas != desired || live.Status.UnavailableReplicas != 0 || controlPlaneImage(live) == "" || controlPlaneImage(live) != controlPlaneImage(expected) || live.Spec.Selector == nil {
		return ErrMaintenanceReleaseUnverified
	}
	selector, err := metav1.LabelSelectorAsSelector(live.Spec.Selector)
	if err != nil {
		return ErrMaintenanceReleaseUnverified
	}
	pods := &corev1.PodList{}
	if err := v.reader.List(ctx, pods, client.InNamespace(v.namespace), client.MatchingLabelsSelector{Selector: selector}, client.Limit(257)); err != nil || pods.Continue != "" || len(pods.Items) != int(desired) {
		return ErrMaintenanceReleaseUnverified
	}
	replicas := map[string]bool{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "ReplicaSet" || owner.UID == "" {
			return ErrMaintenanceReleaseUnverified
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		image := ""
		for _, container := range pod.Spec.Containers {
			if container.Name == "control-plane" {
				if image != "" {
					return ErrMaintenanceReleaseUnverified
				}
				image = container.Image
			}
		}
		if !ready || image != controlPlaneImage(live) {
			return ErrMaintenanceReleaseUnverified
		}
		if !replicas[string(owner.UID)] {
			rs := &appsv1.ReplicaSet{}
			if err := v.reader.Get(ctx, client.ObjectKey{Namespace: v.namespace, Name: owner.Name}, rs); err != nil {
				return ErrMaintenanceReleaseUnverified
			}
			deploymentOwner := metav1.GetControllerOf(rs)
			if rs.UID != owner.UID || deploymentOwner == nil || deploymentOwner.APIVersion != "apps/v1" || deploymentOwner.Kind != "Deployment" || deploymentOwner.UID != live.UID || deploymentOwner.Name != live.Name {
				return ErrMaintenanceReleaseUnverified
			}
			replicas[string(owner.UID)] = true
		}
	}
	after := &appsv1.Deployment{}
	if err := v.reader.Get(ctx, key, after); err != nil || after.UID != live.UID || after.ResourceVersion != live.ResourceVersion {
		return ErrMaintenanceReleaseUnverified
	}
	final, err := v.releaseStatus(ctx, checkpoint.Release)
	if err != nil || final.Version != current.Version {
		return ErrMaintenanceReleaseUnverified
	}
	stillSelected, stillUID, err := v.currentAuthority(ctx)
	if err != nil || stillSelected != selected || stillUID != uid {
		return ErrMaintenanceReleaseUnverified
	}
	return nil
}
func (v *MaintenanceReleaseVerifier) currentAuthority(ctx context.Context) (bool, string, error) {
	snapshot, err := v.authority.Load(ctx)
	if errors.Is(err, operatorcredentials.ErrUninitialized) {
		return false, "", nil
	}
	if err != nil || snapshot.UID == "" {
		return false, "", ErrMaintenanceReleaseUnverified
	}
	return true, snapshot.UID, nil
}
func (v *MaintenanceReleaseVerifier) releaseStatus(ctx context.Context, release string) (releaseStatus, error) {
	output, err := v.helm.Output(ctx, "/usr/local/bin/helm", "status", release, "--namespace", v.namespace, "--output", "json")
	var status releaseStatus
	if err != nil || len(output) > maxCompatibilityManifestBytes || json.Unmarshal([]byte(output), &status) != nil || status.Name != release || status.Version < 1 || status.Info.Status != "deployed" {
		return releaseStatus{}, ErrMaintenanceReleaseUnverified
	}
	return status, nil
}
func maintenanceDeployment(manifest, namespace, name string) (*appsv1.Deployment, error) {
	decoder := yaml.NewDecoder(strings.NewReader(manifest))
	var found *appsv1.Deployment
	for {
		var object map[string]any
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, ErrMaintenanceReleaseUnverified
		}
		for key := range object {
			if strings.EqualFold(key, "kind") && key != "kind" {
				return nil, ErrMaintenanceReleaseUnverified
			}
		}
		if object["kind"] == "List" {
			return nil, ErrMaintenanceReleaseUnverified
		}
		if object["kind"] != "Deployment" {
			continue
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, ErrMaintenanceReleaseUnverified
		}
		var deployment appsv1.Deployment
		if json.Unmarshal(encoded, &deployment) != nil {
			return nil, ErrMaintenanceReleaseUnverified
		}
		if deployment.Name != name && deployment.Labels["app.kubernetes.io/component"] != "control-plane" {
			continue
		}
		if found != nil || deployment.APIVersion != "apps/v1" || deployment.Name != name || deployment.Namespace != namespace || deployment.Labels["app.kubernetes.io/component"] != "control-plane" {
			return nil, ErrMaintenanceReleaseUnverified
		}
		found = &deployment
	}
	if found == nil {
		return nil, ErrMaintenanceReleaseUnverified
	}
	return found, nil
}

type boundedMaintenanceHelm struct{}
type maintenanceOutput struct{ bytes.Buffer }

func (b *maintenanceOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxCompatibilityManifestBytes {
		return 0, ErrMaintenanceReleaseUnverified
	}
	return b.Buffer.Write(p)
}
func (boundedMaintenanceHelm) Output(ctx context.Context, name string, args ...string) (string, error) {
	if name != "/usr/local/bin/helm" {
		return "", ErrMaintenanceReleaseUnverified
	}
	command := exec.CommandContext(ctx, name, args...)
	command.WaitDelay = time.Second
	var output maintenanceOutput
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return "", ErrMaintenanceReleaseUnverified
	}
	return output.String(), nil
}
