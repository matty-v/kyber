// Package fleetdefaults loads runtime-scoped cluster-wide defaults used as
// fallbacks when an individual Agent CR omits its model or runtime version.
// The original keys remain the Claude Code defaults for compatibility; Codex
// uses parallel codexDefault* keys. See PR-B of
// kyber#374 (docs/design/2026-05-29-runtime-model-management-design.md §2 +
// "Requested config" subsection).
//
// The values live in a Kubernetes ConfigMap (one key per field) seeded by
// the Helm chart (templates/control-plane/fleet-defaults-configmap.yaml)
// and writable by the PWA via PUT /api/v1/fleet-defaults. The Resolver
// reads them lazily through a controller-runtime client with a short
// in-memory TTL so:
//
//   - the reconciler hot path stays cheap (one Get per TTL window, not per
//     reconcile);
//   - a PWA write becomes visible without restarting the control-plane
//     (cache expires within ResolveCacheTTL).
package fleetdefaults

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Keys exposed by the ConfigMap. Stable contract — the Helm template,
// the API handler, and the PWA all reference these names.
const (
	KeyDefaultModel               = "defaultModel"
	KeyDefaultRuntimeVersion      = "defaultRuntimeVersion"
	KeyCodexDefaultModel          = "codexDefaultModel"
	KeyCodexDefaultRuntimeVersion = "codexDefaultRuntimeVersion"
)

// ResolveCacheTTL bounds how long a previously-fetched ConfigMap snapshot
// is reused before the next reconcile triggers a fresh Get. Short enough
// that a PWA edit takes effect "soon" (next reconcile after the window),
// long enough that bursty reconciles don't hammer the API server.
const ResolveCacheTTL = 30 * time.Second

// Defaults is the resolved snapshot returned by Resolve.
type Defaults struct {
	// Model is the fleet-default LLM model identifier injected as
	// CLAUDE_MODEL when an Agent's spec.model is empty. Empty means the
	// runtime should select its own default model.
	Model string

	// RuntimeVersion is the fleet-default Claude Code version. PR-B
	// plumbs but does not consume this field; PR-C wires it into the
	// runtime install path.
	RuntimeVersion string

	// CodexModel and CodexRuntimeVersion are the Codex-specific fallbacks.
	// The legacy fields above remain Claude Code defaults for API and
	// ConfigMap compatibility with existing installations.
	CodexModel          string
	CodexRuntimeVersion string
}

// Resolver loads Defaults from a ConfigMap with a small in-memory cache.
// Safe for concurrent use.
type Resolver struct {
	// Client reads the ConfigMap. Must be set.
	Client client.Reader
	// Namespace the ConfigMap lives in (kyber-system in prod). Must be set.
	Namespace string
	// ConfigMapName is the name of the ConfigMap holding the defaults. Must be set.
	ConfigMapName string
	// Now is overridable in tests; nil → time.Now.
	Now func() time.Time

	mu       sync.Mutex
	cached   Defaults
	cachedAt time.Time
	hasCache bool
}

// Resolve returns the current Defaults, refreshing from the API server
// when the cache is stale or empty.
//
// A missing ConfigMap is treated as "no defaults configured" — returns an
// empty Defaults with no error. Operators see the error path on the
// reconciler side when both spec.model and the default are empty.
func (r *Resolver) Resolve(ctx context.Context) (Defaults, error) {
	if r == nil {
		return Defaults{}, nil
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.hasCache && now().Sub(r.cachedAt) < ResolveCacheTTL {
		return r.cached, nil
	}

	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: r.Namespace, Name: r.ConfigMapName}
	err := r.Client.Get(ctx, key, &cm)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.cached = Defaults{}
			r.cachedAt = now()
			r.hasCache = true
			return r.cached, nil
		}
		return Defaults{}, fmt.Errorf("fleetdefaults: get %s/%s: %w", r.Namespace, r.ConfigMapName, err)
	}

	r.cached = Defaults{
		Model:               cm.Data[KeyDefaultModel],
		RuntimeVersion:      cm.Data[KeyDefaultRuntimeVersion],
		CodexModel:          cm.Data[KeyCodexDefaultModel],
		CodexRuntimeVersion: cm.Data[KeyCodexDefaultRuntimeVersion],
	}
	r.cachedAt = now()
	r.hasCache = true
	return r.cached, nil
}

// Invalidate clears the cache, forcing the next Resolve to re-fetch.
// Called after a successful PUT /api/v1/fleet-defaults so the writer
// sees their own update on the next read without waiting ResolveCacheTTL.
func (r *Resolver) Invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hasCache = false
}
