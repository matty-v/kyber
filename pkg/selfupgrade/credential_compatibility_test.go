package selfupgrade

import (
	"errors"
	"strings"
	"testing"
)

const credentialCompatibleManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: kyber-control-plane
  namespace: kyber-system
  labels:
    app.kubernetes.io/component: control-plane
spec:
  template:
    metadata:
      annotations:
        kyber.io/operator-credential-authority-protocol: "1"
        kyber.io/operator-credential-release-image: ghcr.io/matty-v/kyber-control-plane:v1.9.0
    spec:
      containers:
      - name: control-plane
        image: ghcr.io/matty-v/kyber-control-plane:v1.9.0
        args: [--metrics-bind-address=:8080, --leader-elect=true]
        volumeMounts:
        - name: version
          mountPath: /etc/kyber
`

func TestCredentialCompatibilityEffectiveWorkload(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"supported":        func(s string) string { return s },
		"old protocol":     func(s string) string { return strings.Replace(s, `protocol: "1"`, `protocol: "0"`, 1) },
		"unknown protocol": func(s string) string { return strings.Replace(s, `protocol: "1"`, `protocol: "2"`, 1) },
		"overridden image": func(s string) string {
			return strings.Replace(s, "        image: ghcr.io/matty-v/kyber-control-plane:v1.9.0", "        image: custom.invalid/control-plane:v1.9.0", 1)
		},
		"floating image": func(s string) string { return strings.ReplaceAll(s, "v1.9.0", "latest") },
		"custom release declaration": func(s string) string {
			return strings.ReplaceAll(s, "ghcr.io/matty-v/kyber-control-plane", "custom.invalid/control-plane")
		},
		"missing declaration": func(s string) string {
			return strings.Replace(s, "kyber.io/operator-credential-release-image:", "ignored:", 1)
		},
		"entrypoint replacement": func(s string) string {
			return strings.Replace(s, "        args:", "        command: [/bin/sh]\n        args:", 1)
		},
		"auth flag": func(s string) string { return strings.Replace(s, "--leader-elect=true", "--auth-disabled=true", 1) },
		"binary mount": func(s string) string {
			return strings.Replace(s, "mountPath: /etc/kyber", "mountPath: /usr/local/bin", 1)
		},
		"namespace mount": func(s string) string {
			return strings.Replace(s, "mountPath: /etc/kyber", "mountPath: /var/run/secrets/kubernetes.io/serviceaccount", 1)
		},
		"different namespace":  func(s string) string { return strings.Replace(s, "namespace: kyber-system", "namespace: other", 1) },
		"different deployment": func(s string) string { return strings.Replace(s, "name: kyber-control-plane", "name: other", 1) },
		"duplicate deployment": func(s string) string { return s + "---\n" + s },
		"list":                 func(s string) string { return s + "---\nkind: List\nitems: []\n" },
		"ambiguous kind":       func(s string) string { return s + "---\nKind: Deployment\n" },
	} {
		t.Run(name, func(t *testing.T) {
			manifest := mutate(credentialCompatibleManifest)
			got, err := credentialCompatibleDeployment(manifest, "kyber-system", "kyber-control-plane")
			if name == "supported" {
				if err != nil || got == nil {
					t.Fatal("supported deployment rejected", err)
				}
				return
			}
			if !errors.Is(err, errCredentialCompatibility) || got != nil {
				t.Fatal("unsupported workload accepted")
			}
		})
	}
}

func TestCredentialCompatibilityPrivateParseFailures(t *testing.T) {
	private := "private-installation-value"
	for _, bad := range []string{
		"kind: Secret\ndata: [" + private,
		"kind: Deployment\nkind: " + private,
		strings.Repeat(" ", maxCompatibilityManifestBytes+1),
		"kind: Secret\nstringData:\n  api-key: " + private,
	} {
		_, err := credentialCompatibleDeployment(bad, "kyber-system", "kyber-control-plane")
		if !errors.Is(err, errCredentialCompatibility) || strings.Contains(err.Error(), private) {
			t.Fatal("parse failure accepted or exposed input")
		}
	}
	withSecret := "kind: Secret\nstringData:\n  api-key: " + private + "\n---\n" + credentialCompatibleManifest
	if _, err := credentialCompatibleDeployment(withSecret, "kyber-system", "kyber-control-plane"); err != nil {
		t.Fatal("unrelated Secret affected selection", err)
	}
}

func TestCredentialCompatibilityAcceptsOnlyIndependentlyAssertedMainSHA(t *testing.T) {
	sha := strings.Repeat("a", 40)
	manifest := strings.ReplaceAll(credentialCompatibleManifest, "v1.9.0", sha)
	if _, err := credentialCompatibleDeployment(manifest, "kyber-system", "kyber-control-plane"); err != nil {
		t.Fatal("publisher-pinned main image rejected")
	}
	mismatched := strings.Replace(manifest, "image: ghcr.io/matty-v/kyber-control-plane:"+sha, "image: ghcr.io/matty-v/kyber-control-plane:"+strings.Repeat("b", 40), 1)
	if _, err := credentialCompatibleDeployment(mismatched, "kyber-system", "kyber-control-plane"); err == nil {
		t.Fatal("image override inferred compatible")
	}
	for _, tag := range []string{strings.Repeat("A", 40), strings.Repeat("z", 40), "latest", sha + "-local"} {
		if _, err := credentialCompatibleDeployment(strings.ReplaceAll(credentialCompatibleManifest, "v1.9.0", tag), "kyber-system", "kyber-control-plane"); err == nil {
			t.Fatal("unpublished/ambiguous image accepted")
		}
	}
}
