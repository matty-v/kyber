package runtimedetect

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// KeySource returns the operator-supplied Anthropic API key (or an empty
// string when no key is configured). The poller calls this once per cycle
// so a rotated key is picked up on the next poll without a control-plane
// restart. An empty string is not an error — the poller treats "no key" as
// a soft failure mode (cache continues to serve, /available shows "models
// detection unavailable" via an empty list).
type KeySource func() (string, error)

// FileKeySource reads the API key from a file path. The expected mount
// pattern is a K8s Secret mounted at /etc/kyber/anthropic-key/api-key.
// Kubelet refreshes mounted Secret volumes within ~60s, so a rotation via
// the write API (PUT /api/v1/settings/anthropic-key) propagates to the
// next poll cycle without a pod restart.
//
// Returns "" (no error) when the file is absent — operator hasn't entered
// a key yet. Returns "" + error only when the file exists but is
// unreadable (e.g., permission error) so the caller can log the
// misconfiguration.
func FileKeySource(path string) KeySource {
	return func() (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", nil
			}
			return "", fmt.Errorf("reading anthropic key file %q: %w", path, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
}

// EnvKeySource reads the key from an env var. Used in dev/test installs
// where mounting a Secret would be overkill. Empty env → empty key, no
// error.
func EnvKeySource(envVar string) KeySource {
	return func() (string, error) {
		return strings.TrimSpace(os.Getenv(envVar)), nil
	}
}

// MultiKeySource tries each source in order and returns the first non-empty
// value. Used to layer the env-var fallback below the file mount — file is
// the production path, env is the dev/test override.
func MultiKeySource(sources ...KeySource) KeySource {
	return func() (string, error) {
		var firstErr error
		for _, s := range sources {
			if s == nil {
				continue
			}
			v, err := s()
			if err != nil && firstErr == nil {
				firstErr = err
				continue
			}
			if v != "" {
				return v, nil
			}
		}
		return "", firstErr
	}
}
