package api

import (
	"context"
	"reflect"
	"time"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/securityaudit"
	"github.com/matty-v/kyber/pkg/selfupgrade"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newAdoptionNativeClient(config *rest.Config) (client.Client, error) {
	if config == nil {
		return nil, operatorcredentials.ErrUnavailable
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion, appsv1.SchemeGroupVersion, batchv1.SchemeGroupVersion})
	for _, kind := range []string{"Secret", "Pod"} {
		mapper.Add(corev1.SchemeGroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	for _, kind := range []string{"Deployment", "ReplicaSet"} {
		mapper.Add(appsv1.SchemeGroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	mapper.Add(batchv1.SchemeGroupVersion.WithKind("Job"), meta.RESTScopeNamespace)
	bounded := rest.CopyConfig(config)
	if bounded.Timeout == 0 || bounded.Timeout > 2*time.Second {
		bounded.Timeout = 2 * time.Second
	}
	result, err := client.New(bounded, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		return nil, operatorcredentials.ErrUnavailable
	}
	return result, nil
}

type nativeAdoptionPreflight struct {
	reader    client.Client
	sources   *credentialAdoptionSourceReader
	release   *selfupgrade.MaintenanceReleaseVerifier
	probe     *credentialAdoptionReplicaProbe
	intent    securityaudit.CredentialIntent
	ownJobUID string
}

func newNativeAdoptionPreflight(config *rest.Config, intent securityaudit.CredentialIntent, ownJobUID string) (*nativeAdoptionPreflight, error) {
	if intent.Validate() != nil || intent.Admission.ResourceKind != "credential-adoption" {
		return nil, operatorcredentials.ErrInvalid
	}
	reader, err := newAdoptionNativeClient(config)
	if err != nil {
		return nil, err
	}
	sources, err := newCredentialAdoptionSourceReader(config, intent.Admission.Namespace, intent.Adoption.RootSourceName)
	if err != nil {
		return nil, operatorcredentials.ErrUnavailable
	}
	release, err := selfupgrade.NewMaintenanceReleaseVerifier(config, intent.Admission.Namespace)
	if err != nil {
		return nil, operatorcredentials.ErrUnavailable
	}
	return &nativeAdoptionPreflight{reader: reader, sources: sources, release: release, probe: newCredentialAdoptionReplicaProbe(), intent: intent, ownJobUID: ownJobUID}, nil
}
func (p *nativeAdoptionPreflight) verify(ctx context.Context) (*credentialAdoptionSources, error) {
	if p == nil || p.sources == nil || p.release == nil || p.probe == nil {
		return nil, operatorcredentials.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	admission := p.intent.Adoption
	snapshot, err := p.release.VerifyAdoptionRelease(ctx, admission.ReleaseName, admission.DeploymentName, admission.DeploymentUID)
	if err != nil {
		return nil, operatorcredentials.ErrUnavailable
	}
	source, err := p.sources.Read(ctx)
	if err != nil || !sameAdoptionSources(source.admissionMetadata(), *admission) {
		return nil, operatorcredentials.ErrUnavailable
	}
	port := int32(0)
	for _, declared := range snapshot.Deployment.Spec.Template.Spec.Containers[0].Ports {
		if declared.Name == "http" {
			if port != 0 {
				return nil, operatorcredentials.ErrUnavailable
			}
			port = declared.ContainerPort
		}
	}
	if port < 1 || port > 65535 {
		return nil, operatorcredentials.ErrUnavailable
	}
	for _, pod := range snapshot.Pods {
		if p.probe.verify(ctx, source, pod, int(port)) != nil {
			return nil, operatorcredentials.ErrUnavailable
		}
	}
	if p.quiescentWriters(ctx) != nil || source.Verify(ctx) != nil {
		return nil, operatorcredentials.ErrUnavailable
	}
	// Recheck the release after key probes and writer observations. Ordinary
	// namespace controllers may replace Pods, but no unsupported replica may be
	// silently omitted or substituted for a previously probed UID.
	final, err := p.release.VerifyAdoptionRelease(ctx, admission.ReleaseName, admission.DeploymentName, admission.DeploymentUID)
	if err != nil || final.Revision != snapshot.Revision || final.Deployment.UID != snapshot.Deployment.UID || !reflect.DeepEqual(final.Deployment.Spec, snapshot.Deployment.Spec) || !sameAdoptionPods(snapshot.Pods, final.Pods) {
		return nil, operatorcredentials.ErrUnavailable
	}
	return source, nil
}
func sameAdoptionPods(before, after []corev1.Pod) bool {
	if len(before) != len(after) {
		return false
	}
	ids := map[string]string{}
	for _, pod := range before {
		ids[string(pod.UID)] = pod.Status.PodIP
	}
	for _, pod := range after {
		if value, found := ids[string(pod.UID)]; !found || value != pod.Status.PodIP {
			return false
		}
	}
	return true
}
func (p *nativeAdoptionPreflight) quiescentWriters(ctx context.Context) error {
	for component, container := range map[string]string{"self-upgrade": "upgrade", "credential-adoption": "adopt-credentials"} {
		jobs := &batchv1.JobList{}
		if p.reader.List(ctx, jobs, client.InNamespace(p.intent.Admission.Namespace), client.MatchingLabels{"app.kubernetes.io/component": component}, client.Limit(257)) != nil || len(jobs.Items) > 256 || jobs.Continue != "" {
			return operatorcredentials.ErrUnavailable
		}
		known := map[string]bool{}
		for _, job := range jobs.Items {
			known[string(job.UID)] = true
			if p.ownJobUID != "" && string(job.UID) == p.ownJobUID {
				continue
			}
			stopped, err := operatorcredentials.SupervisedJobStopped(ctx, p.reader, job, container)
			if err != nil || !stopped {
				return operatorcredentials.ErrUnavailable
			}
		}
		pods := &corev1.PodList{}
		if p.reader.List(ctx, pods, client.InNamespace(p.intent.Admission.Namespace), client.MatchingLabels{"app.kubernetes.io/component": component}, client.Limit(257)) != nil || len(pods.Items) > 256 || pods.Continue != "" {
			return operatorcredentials.ErrUnavailable
		}
		for _, pod := range pods.Items {
			owner := metav1.GetControllerOf(&pod)
			if owner == nil || owner.APIVersion != "batch/v1" || owner.Kind != "Job" || !known[string(owner.UID)] {
				return operatorcredentials.ErrUnavailable
			}
		}
	}
	return nil
}
