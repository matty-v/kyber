// Package contextwindowmap loads the operator-editable model →
// context-window override map used by the detection poller and the agent
// reconciler. See PR-D of kyber#374
// (docs/2026-05-29-runtime-model-management-design.md §5).
//
// The values live in a Kubernetes ConfigMap (one entry per model: key =
// model ID, value = decimal token count) seeded by the Helm chart
// (templates/control-plane/model-context-windows-configmap.yaml) and
// editable in-cluster by an operator (kubectl edit cm
// kyber-model-context-windows). The Resolver reads them lazily through a
// controller-runtime client with a short in-memory TTL so:
//
//   - hot-path callers (the detection poller, the pod-build path) stay
//     cheap (one Get per TTL window, not per call);
//   - an operator edit becomes visible without restarting the
//     control-plane (cache expires within ResolveCacheTTL).
//
// Unmapped model IDs return a 200K floor + Known=false so a brand-new 1M
// model under-reports usage but does not crash — operators fix it by
// adding the entry, no Kyber release required.
package contextwindowmap

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultContextWindowFloor is the safe floor returned for unmapped model
// IDs. Mirrors pkg/runtimedetect.DefaultContextWindowFloor; duplicated
// here so callers don't need to cross-import that package.
const DefaultContextWindowFloor int64 = 200_000

// ResolveCacheTTL bounds how long a previously-fetched ConfigMap snapshot
// is reused before the next caller triggers a fresh Get. Short enough
// that an operator edit takes effect "soon" (next read after the window
// expires), long enough that the detection poller doesn't hit the API
// server every cache lookup.
const ResolveCacheTTL = 30 * time.Second

// Entry describes a single context-window override.
type Entry struct {
	// ContextWindow is the total context-window size in tokens.
	ContextWindow int64
	// Known is true when ContextWindow came from the ConfigMap. False when
	// the model wasn't listed and the caller is reading the floor default.
	Known bool
}

// Resolver loads the override map from a ConfigMap with a small
// in-memory cache. Safe for concurrent use.
type Resolver struct {
	// Client reads the ConfigMap. Must be set.
	Client client.Reader
	// Namespace the ConfigMap lives in (kyber-system in prod). Must be set.
	Namespace string
	// ConfigMapName is the ConfigMap holding the overrides. When empty,
	// the resolver always returns the floor (no override map configured —
	// dev/test mode).
	ConfigMapName string
	// Now is overridable in tests; nil → time.Now.
	Now func() time.Time

	mu       sync.Mutex
	cached   map[string]int64
	cachedAt time.Time
	hasCache bool
}

// LookupOr returns (window, known). When the model has an override entry,
// window is the configured value and known=true. Otherwise window is the
// floor default and known=false. A nil resolver returns (floor, false) —
// safe to call when override-map support is not configured.
func (r *Resolver) LookupOr(ctx context.Context, modelID string) (int64, bool) {
	if r == nil || modelID == "" {
		return DefaultContextWindowFloor, false
	}
	m, err := r.load(ctx)
	if err != nil {
		// Soft failure: log-and-floor at the caller's level. Hot-path
		// callers (poller, pod build) treat the override map as
		// best-effort enrichment — never block on it.
		return DefaultContextWindowFloor, false
	}
	if v, ok := m[modelID]; ok {
		return v, true
	}
	return DefaultContextWindowFloor, false
}

// aliasWindows maps the family short-names Claude Code may emit in its JSONL
// transcripts ("opus"/"sonnet"/"haiku") to a built-in context window. These
// are deliberately NOT operator-tunable — the ConfigMap holds concrete model
// IDs and is authoritative for them; the aliases exist only so transcript
// parsing of an alias --model still resolves to a sensible window. Mirrors
// tokenreport.aliasLimits (kept in sync by hand; both are the same CC quirk).
var aliasWindows = map[string]int64{
	"opus":   1_000_000,
	"sonnet": 200_000,
	"haiku":  200_000,
}

// LookupNormalized resolves a model id the way the in-pod tokenreport.LimitFor
// used to (#396): it strips the "[1m]" opt-in suffix, consults the operator
// ConfigMap for the concrete id (authoritative), then falls back to the
// built-in family aliases, then the 200K floor. Returns (window, known) —
// known is true only when the value came from the ConfigMap or a known alias
// (i.e. a confident window), false when it fell through to the floor (the PWA
// renders that as an estimate). nil-safe: a nil resolver still resolves
// aliases and floors everything else.
func (r *Resolver) LookupNormalized(ctx context.Context, modelID string) (int64, bool) {
	base := strings.TrimSuffix(modelID, "[1m]")
	if w, known := r.LookupOr(ctx, base); known {
		return w, true
	}
	if w, ok := aliasWindows[base]; ok {
		return w, true
	}
	return DefaultContextWindowFloor, false
}

// All returns a copy of the current override map. Callers may mutate the
// returned map without affecting the resolver. Empty when the ConfigMap
// is absent OR carries no entries.
func (r *Resolver) All(ctx context.Context) (map[string]int64, error) {
	if r == nil {
		return map[string]int64{}, nil
	}
	m, err := r.load(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out, nil
}

// Invalidate clears the cache, forcing the next read to re-fetch. Called
// after a successful PUT to the ConfigMap so the writer sees their own
// update on the next read without waiting ResolveCacheTTL.
func (r *Resolver) Invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hasCache = false
}

// load returns the parsed override map, refreshing from the API server
// when the cache is stale or empty. A missing ConfigMap is treated as
// "no overrides configured" — returns an empty map, no error.
func (r *Resolver) load(ctx context.Context) (map[string]int64, error) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.hasCache && now().Sub(r.cachedAt) < ResolveCacheTTL {
		return r.cached, nil
	}

	out := map[string]int64{}
	if r.ConfigMapName == "" {
		r.cached = out
		r.cachedAt = now()
		r.hasCache = true
		return out, nil
	}

	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: r.Namespace, Name: r.ConfigMapName}
	if err := r.Client.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			r.cached = out
			r.cachedAt = now()
			r.hasCache = true
			return out, nil
		}
		return nil, fmt.Errorf("contextwindowmap: get %s/%s: %w", r.Namespace, r.ConfigMapName, err)
	}

	for id, raw := range cm.Data {
		// Skip malformed entries silently — an operator typo on one row
		// must not blank the rest of the map.
		if id == "" {
			continue
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		out[id] = n
	}
	r.cached = out
	r.cachedAt = now()
	r.hasCache = true
	return out, nil
}
