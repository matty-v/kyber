package githubapp

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SecretName is the conventional name of the Secret that holds the App's
// credentials. Lives in the control plane namespace (typically kyber-system).
const SecretName = "kyber-github-app"

// Secret keys produced by the Phase 1 install docs walkthrough.
const (
	secretKeyAppID          = "app-id"
	secretKeyInstallationID = "installation-id"
	secretKeyPrivateKey     = "private-key.pem"
)

// LoadConfigFromSecret reads the kyber-github-app Secret from the given
// namespace and returns a Config ready to pass to NewClient. Missing or
// malformed fields produce an error — callers should fail startup rather
// than retry, since this Secret is operator-managed and doesn't self-heal.
func LoadConfigFromSecret(ctx context.Context, c client.Client, namespace string) (Config, error) {
	sec := &corev1.Secret{}
	key := types.NamespacedName{Name: SecretName, Namespace: namespace}
	if err := c.Get(ctx, key, sec); err != nil {
		return Config{}, fmt.Errorf("fetching %s/%s: %w", namespace, SecretName, err)
	}
	return configFromSecretData(sec.Data)
}

func configFromSecretData(data map[string][]byte) (Config, error) {
	if len(data) == 0 {
		return Config{}, errors.New("githubapp: Secret has no data")
	}

	appID, err := parseInt64Key(data, secretKeyAppID)
	if err != nil {
		return Config{}, err
	}
	installID, err := parseInt64Key(data, secretKeyInstallationID)
	if err != nil {
		return Config{}, err
	}

	keyBytes, ok := data[secretKeyPrivateKey]
	if !ok || len(keyBytes) == 0 {
		return Config{}, fmt.Errorf("githubapp: Secret missing key %q", secretKeyPrivateKey)
	}
	pk, err := ParsePrivateKey(keyBytes)
	if err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", secretKeyPrivateKey, err)
	}

	cfg := Config{AppID: appID, InstallationID: installID, PrivateKey: pk}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseInt64Key(data map[string][]byte, key string) (int64, error) {
	raw, ok := data[key]
	if !ok || len(raw) == 0 {
		return 0, fmt.Errorf("githubapp: Secret missing key %q", key)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", key, err)
	}
	if v == 0 {
		return 0, fmt.Errorf("githubapp: Secret key %q is zero", key)
	}
	return v, nil
}
