package api

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// corev1Scheme returns a scheme with corev1 registered — Secrets are the only
// type the resolver touches. (The shared mustNewScheme helper lives in the
// external api_test package and isn't visible here.)
func corev1Scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}
	return scheme
}

func secretWith(name, namespace string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       data,
	}
}

func keyFromCaller(name, secret, dataKey string) ScopedCaller {
	return ScopedCaller{
		Name:    name,
		KeyFrom: &SecretKeyRef{Secret: secret, Key: dataKey},
		Scopes:  []string{"lifecycle:write"},
	}
}

func TestResolveScopedCallers_FillsKeyFromSecret(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(corev1Scheme(t)).WithObjects(
		secretWith("some-caller-api-key", "kyber-system", map[string][]byte{"api-key": []byte("the-key-value\n")}),
	).Build()

	resolved, rejected := ResolveScopedCallers(context.Background(), c, "kyber-system",
		[]ScopedCaller{keyFromCaller("some-caller", "some-caller-api-key", "api-key")})

	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %+v", rejected)
	}
	if len(resolved) != 1 || resolved[0].Key != "the-key-value" {
		t.Fatalf("key not filled (and trimmed) from Secret: %+v", resolved)
	}
}

func TestResolveScopedCallers_InlineEntriesPassThroughUntouched(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(corev1Scheme(t)).Build()
	in := []ScopedCaller{{Name: "inline", Key: "k1", Scopes: []string{"lifecycle:write"}}}

	resolved, rejected := ResolveScopedCallers(context.Background(), c, "kyber-system", in)

	if len(rejected) != 0 || len(resolved) != 1 || resolved[0].Key != "k1" {
		t.Fatalf("inline caller must pass through untouched (zero k8s reads), got resolved=%+v rejected=%+v", resolved, rejected)
	}
}

func TestResolveScopedCallers_MissingSecretDropsOnlyThatCaller(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(corev1Scheme(t)).WithObjects(
		secretWith("good-key", "kyber-system", map[string][]byte{"api-key": []byte("v-good")}),
	).Build()

	resolved, rejected := ResolveScopedCallers(context.Background(), c, "kyber-system", []ScopedCaller{
		keyFromCaller("bad", "does-not-exist", "api-key"),
		keyFromCaller("good", "good-key", "api-key"),
		{Name: "inline", Key: "k1", Scopes: []string{"lifecycle:write"}},
	})

	if len(resolved) != 2 {
		t.Fatalf("drop isolation: want the 2 healthy callers to survive, got %+v", resolved)
	}
	for _, r := range resolved {
		if r.Name == "bad" {
			t.Fatal("the unresolvable caller must be dropped, never granted")
		}
	}
	if len(rejected) != 1 || rejected[0].Caller != "bad" || rejected[0].Secret != "does-not-exist" {
		t.Fatalf("rejection must name the caller and the reference: %+v", rejected)
	}
}

func TestResolveScopedCallers_MissingDataKeyDrops(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(corev1Scheme(t)).WithObjects(
		secretWith("s", "kyber-system", map[string][]byte{"other-key": []byte("unrelated-value")}),
	).Build()

	resolved, rejected := ResolveScopedCallers(context.Background(), c, "kyber-system",
		[]ScopedCaller{keyFromCaller("x", "s", "api-key")})

	if len(resolved) != 0 || len(rejected) != 1 {
		t.Fatalf("missing data key must drop the caller: resolved=%+v rejected=%+v", resolved, rejected)
	}
}

func TestResolveScopedCallers_EmptyValueDrops(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(corev1Scheme(t)).WithObjects(
		secretWith("s", "kyber-system", map[string][]byte{"api-key": []byte("  \n")}),
	).Build()

	resolved, rejected := ResolveScopedCallers(context.Background(), c, "kyber-system",
		[]ScopedCaller{keyFromCaller("x", "s", "api-key")})

	if len(resolved) != 0 || len(rejected) != 1 {
		t.Fatalf("an empty (after trim) key value must drop the caller, never grant an empty Bearer: resolved=%+v rejected=%+v", resolved, rejected)
	}
}

// The app-level same-namespace pin is LOAD-BEARING (kyber#557, Ackbar's deploy
// review): the cp's ClusterRole is cluster-wide on Secrets, so this code-level
// restriction is the security boundary that keeps the callers doc from acting
// as a cross-namespace Secret-to-API-key oracle.
func TestResolveScopedCallers_NeverReadsOutsideOwnNamespace(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(corev1Scheme(t)).WithObjects(
		// The referenced name exists — but only in ANOTHER namespace.
		secretWith("tempting-secret", "other-namespace", map[string][]byte{"api-key": []byte("cross-ns-value")}),
	).Build()

	resolved, rejected := ResolveScopedCallers(context.Background(), c, "kyber-system",
		[]ScopedCaller{keyFromCaller("sneaky", "tempting-secret", "api-key")})

	if len(resolved) != 0 {
		t.Fatalf("a Secret outside the cp namespace must NEVER resolve: %+v", resolved)
	}
	if len(rejected) != 1 {
		t.Fatalf("want the cross-namespace reference rejected, got %+v", rejected)
	}
}

// Never-log discipline: rejection errors are what main logs at startup — they
// must name the reference (caller/secret/key names), never any Secret value.
func TestResolveScopedCallers_RejectionErrorsCarryNoSecretValues(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(corev1Scheme(t)).WithObjects(
		secretWith("s", "kyber-system", map[string][]byte{
			"present-key": []byte("super-secret-value-A"),
			"empty-key":   []byte(" \n"),
		}),
	).Build()

	_, rejected := ResolveScopedCallers(context.Background(), c, "kyber-system", []ScopedCaller{
		keyFromCaller("miss", "s", "absent-key"), // missing data key, Secret holds other values
		keyFromCaller("empty", "s", "empty-key"), // empty value
		keyFromCaller("gone", "nope", "k"),       // missing Secret
	})

	if len(rejected) != 3 {
		t.Fatalf("want 3 rejections, got %+v", rejected)
	}
	for _, rej := range rejected {
		msg := rej.Err.Error()
		if strings.Contains(msg, "super-secret-value-A") {
			t.Errorf("rejection error leaked a Secret value: %q", msg)
		}
		if rej.Caller == "" || rej.Secret == "" || rej.Key == "" {
			t.Errorf("rejection must carry the full reference for the setup log: %+v", rej)
		}
	}
}

// The rotation exercise (kyber#557 AC; the pending rotation-survival AC): swap the referenced Secret's value, re-resolve
// and rebuild the authenticator — the restart simulation, since resolution
// happens at process start — and the old key must 401 while the new key
// authenticates with the caller's scopes intact.
func TestResolveScopedCallers_RotationExercise(t *testing.T) {
	scheme := corev1Scheme(t)
	secret := secretWith("some-caller-api-key", "kyber-system", map[string][]byte{"api-key": []byte("key-v1")})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	callers := []ScopedCaller{keyFromCaller("some-caller", "some-caller-api-key", "api-key")}

	// Boot 1: v1 resolves and authenticates.
	resolved, rejected := ResolveScopedCallers(context.Background(), c, "kyber-system", callers)
	if len(rejected) != 0 {
		t.Fatalf("boot 1: %+v", rejected)
	}
	auth1 := NewAPIKeyAuthenticator("legacy", resolved...)
	caller, err := auth1.Authenticate(reqWithBearer("key-v1"))
	if err != nil || caller.Name != "some-caller" {
		t.Fatalf("boot 1: v1 must authenticate as some-caller, got %v / %v", caller, err)
	}

	// Rotate: update the referenced Secret to v2.
	secret.Data["api-key"] = []byte("key-v2")
	if err := c.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}

	// Boot 2 (the cp restart): re-resolve + rebuild.
	resolved, rejected = ResolveScopedCallers(context.Background(), c, "kyber-system", callers)
	if len(rejected) != 0 {
		t.Fatalf("boot 2: %+v", rejected)
	}
	auth2 := NewAPIKeyAuthenticator("legacy", resolved...)

	if _, err := auth2.Authenticate(reqWithBearer("key-v1")); err == nil {
		t.Fatal("rotated-out key must be rejected after restart")
	}
	caller, err = auth2.Authenticate(reqWithBearer("key-v2"))
	if err != nil {
		t.Fatalf("rotated-in key must authenticate: %v", err)
	}
	if caller.Name != "some-caller" || !caller.Scopes.Has(ScopeLifecycleWrite) {
		t.Fatalf("rotated caller must keep identity and scopes: %+v", caller)
	}
}
