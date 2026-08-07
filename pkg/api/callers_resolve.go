package api

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CallerRejection records one keyFrom caller dropped by ResolveScopedCallers,
// for the startup log: the caller name and the reference (names only — never
// a Secret value; the never-log discipline is asserted in tests).
type CallerRejection struct {
	Caller string
	Secret string
	Key    string
	Err    error
}

// ResolveScopedCallers fills in the Key value for every keyFrom entry by
// reading the referenced Secret data key (kyber#557). Inline-key entries pass
// through untouched, with zero k8s reads.
//
// References resolve ONLY in the control plane's own namespace. That pin is
// load-bearing, not cosmetic: the cp's ClusterRole covers Secrets cluster-wide,
// so this application-level restriction is what prevents a callers-doc write
// from designating arbitrary cross-namespace Secrets as API credentials.
//
// Per-caller fail-closed isolation (the cmd/control-plane parse-block pattern):
// a missing Secret, missing data key, or empty value drops exactly that caller —
// returned in rejected for the caller to log loudly — while every other caller
// and the legacy key proceed. Nothing is ever silently granted.
func ResolveScopedCallers(ctx context.Context, c client.Client, namespace string, callers []ScopedCaller) (resolved []ScopedCaller, rejected []CallerRejection) {
	for _, caller := range callers {
		if caller.KeyFrom == nil {
			resolved = append(resolved, caller)
			continue
		}
		ref := *caller.KeyFrom
		value, err := readSecretKey(ctx, c, namespace, ref)
		if err != nil {
			rejected = append(rejected, CallerRejection{Caller: caller.Name, Secret: ref.Secret, Key: ref.Key, Err: err})
			continue
		}
		caller.Key = value
		resolved = append(resolved, caller)
	}
	return resolved, rejected
}

// readSecretKey reads one Secret data key in the given namespace. Error text
// carries names only — never the Secret's contents.
func readSecretKey(ctx context.Context, c client.Client, namespace string, ref SecretKeyRef) (string, error) {
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Secret}, &secret); err != nil {
		return "", fmt.Errorf("reading Secret %s/%s: %w", namespace, ref.Secret, err)
	}
	raw, ok := secret.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("Secret %s/%s has no data key %q", namespace, ref.Secret, ref.Key)
	}
	// Trim like file-mounting consumers do, so a stringData value with a
	// trailing newline yields the same bytes on both sides of the credential.
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("Secret %s/%s data key %q is empty", namespace, ref.Secret, ref.Key)
	}
	return value, nil
}
