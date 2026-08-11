package updates

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ManagedBy names whatever is responsible for this cluster's Kyber resources.
type ManagedBy string

const (
	// ManagedByHelm — Kyber can upgrade itself here.
	ManagedByHelm ManagedBy = "helm"
	// ManagedByArgoCD — Kyber must NOT upgrade itself here. ArgoCD reverts an
	// in-cluster change on its next sync, so a self-upgrade would appear to
	// work and then silently undo itself. The operator bumps their deploy repo
	// instead.
	ManagedByArgoCD ManagedBy = "argocd"
	// ManagedByUnknown — detection could not run (RBAC, missing Deployment).
	// Treated as "do not act" by callers: unknown ownership is not a licence
	// to start mutating.
	ManagedByUnknown ManagedBy = "unknown"
)

// argoTrackingAnnotation is what ArgoCD stamps on every resource it applies.
// This is the marker that actually distinguishes an ArgoCD-applied resource
// from a Helm release member.
//
// Note what is NOT a reliable signal: the `app.kubernetes.io/managed-by: Helm`
// label. Kyber's chart renders that label on every resource, so it is present
// even on clusters where ArgoCD templated the chart and applied the manifests
// itself and no Helm release exists at all. Verified on razer and falcon
// 2026-08-10 — both carried the Helm label, neither had a
// `sh.helm.release.v1.*` Secret. Trusting that label would have told Kyber it
// was safe to self-upgrade on exactly the clusters where it is not.
const argoTrackingAnnotation = "argocd.argoproj.io/tracking-id"

// helmReleaseAnnotation is Helm's own ownership marker. Present only on
// resources belonging to a real Helm release.
const helmReleaseAnnotation = "meta.helm.sh/release-name"

// DetectManagedBy inspects the control-plane Deployment — the resource Kyber
// would have to replace to upgrade itself — and reports what owns it.
//
// ArgoCD wins over Helm when both markers are present: a cluster mid-migration
// can carry Helm ownership annotations while ArgoCD is still reconciling, and
// in that window the reconciler is still the thing that would revert us.
func DetectManagedBy(ctx context.Context, c client.Client, namespace, deploymentName string) ManagedBy {
	if c == nil || namespace == "" || deploymentName == "" {
		return ManagedByUnknown
	}
	dep := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: namespace, Name: deploymentName}
	if err := c.Get(ctx, key, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return ManagedByUnknown
		}
		return ManagedByUnknown
	}
	anns := dep.GetAnnotations()
	if _, ok := anns[argoTrackingAnnotation]; ok {
		return ManagedByArgoCD
	}
	if _, ok := anns[helmReleaseAnnotation]; ok {
		return ManagedByHelm
	}
	return ManagedByUnknown
}

// CanSelfUpgrade reports whether Kyber may apply an update itself.
//
// Only a real Helm release qualifies. Unknown ownership deliberately does not:
// the cost of refusing when we could have acted is an operator running one
// command, and the cost of acting when we should not have is fighting a
// reconciler over a live cluster.
func (m ManagedBy) CanSelfUpgrade() bool { return m == ManagedByHelm }

// Reason explains a refusal in the operator's terms. Empty when self-upgrade
// is allowed.
func (m ManagedBy) Reason() string {
	switch m {
	case ManagedByArgoCD:
		return "This cluster is managed by ArgoCD. It would revert the upgrade on its next sync, so Kyber won't install it — update the chart version in your deploy repo instead."
	case ManagedByUnknown:
		return "Kyber can't tell what manages this cluster's resources, so it won't install an update it might not own. Upgrade with Helm directly."
	default:
		return ""
	}
}
