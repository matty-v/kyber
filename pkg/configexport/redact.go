// Package configexport turns a live Kyber install back into the values file
// that recreates it.
//
// This is the artifact the infra-as-code story rests on once ArgoCD is retired:
// a cluster's config lives in the cluster, and this is how an operator gets it
// back out and into version control. See dave-agent spec
// 2026-08-10-kyber-owns-its-deployment.md §5/§9.
//
// SECURITY POSTURE — read before changing the classification rules.
//
// An export is meant to be safe to paste into a chat, a ticket, or a git repo.
// That is most of its value: an export you have to handle like a credential is
// one nobody will actually commit, which defeats the point. So secrets are
// removed, and the rules are built to FAIL CLOSED:
//
//   - Known secret-bearing paths are redacted explicitly.
//   - Anything whose key name looks like a credential is redacted as a
//     catch-all, so a new secret added to the chart is covered by default.
//   - Reference-style keys (the NAME of a Secret, not its contents) are kept,
//     because dropping them would produce a values file that cannot recreate
//     the cluster.
//   - A test walks the chart's own values.yaml and fails when a new
//     secret-looking path appears that is neither redacted nor explicitly
//     classified as a reference. That is what stops this list from rotting
//     into a snapshot of what was secret in 2026.
//
// KNOWN LIMIT, stated because the guard looks more complete than it is: the
// rot guard only catches credential-LOOKING names. A future secret with an
// innocuous key name passes both the pattern and the guard. `callers` — the
// scoped-API-key document — is exactly that shape; it lives on the api-key
// Secret rather than in chart values today, so it cannot reach an export, and
// it is listed defensively below in case that ever changes. Adding a secret
// under a bland name is the failure mode this package cannot detect for you.
package configexport

import (
	"strings"
)

// Redacted replaces a removed value. Deliberately not an empty string: an
// operator reading the export must be able to tell "this was removed for
// safety" from "this was never set", because the two need different actions
// when rebuilding.
const Redacted = "<redacted — provide this yourself>"

// secretPaths are exact dotted value paths known to carry credentials.
//
// Every entry here is a value an operator supplies, not a name Kyber
// generates. Sourced by reading deploy/helm/kyber/values.yaml, not from
// memory; the classification test keeps it honest.
var secretPaths = map[string]bool{
	"api.apiKey":                    true,
	"k3s.joinToken":                 true,
	"minio.rootPassword":            true,
	"postgresql.auth.password":      true,
	"runtimeDetect.anthropicApiKey": true,
}

// defensiveSecretPaths are redacted but are NOT chart values today. They exist
// so that if the value is ever surfaced through values.yaml it is already
// covered, rather than leaking until someone notices.
//
// Exempt from the "still exists in the chart" test by design — they are
// expected to be absent. Each needs a reason.
var defensiveSecretPaths = map[string]bool{
	// Scoped API keys. Supplied via the `callers` JSON document on the
	// api-key Secret (see pkg/api/auth.go), never through values. The key
	// name matches no credential pattern, so nothing else would catch it.
	"api.callers": true,
}

// referenceKeys are key names that NAME a resource rather than contain its
// contents. They match the credential pattern below but must be kept — an
// export missing `existingSecret` would not recreate the cluster.
var referenceKeys = map[string]bool{
	"existingsecret":      true,
	"imagepullsecrets":    true, // a LIST of Secret names
	"githubappsecretname": true,
	"devapikeysecretname": true,
	// A NAMESPACE name that happens to contain "apikey". Found by the rot
	// guard once it was made non-vacuous — redacting it breaks the preview
	// bootstrap's source lookup.
	"devapikeysourcenamespace":  true,
	"copygithubappsecret":       true, // a bool toggle, not material
	"existingcredentialssecret": true,
	"credentialssecret":         true,
	"credentialssecretname":     true,
	"postgressecretname":        true,
	"signingkeysecretname":      true,
	"signingkeysecretkey":       true, // the KEY WITHIN the Secret, not the material
	"apikeysecretname":          true,
	"secretname":                true,
	"bootstrapsecrets":          true, // a bool toggle
	"tokenratesconfigmapname":   true,
	// The release-feed token is referenced by Secret name + key, never held
	// in values. Caught by the rot guard the moment #43 merged — a live
	// demonstration of it flagging a credential-looking value added by a
	// DIFFERENT change, which is exactly what it is for.
	"tokensecretname": true,
	"tokensecretkey":  true,
}

// credentialSubstrings catch new secret-bearing keys by name. Lowercased
// comparison; a key containing any of these is redacted unless it is a known
// reference key.
var credentialSubstrings = []string{
	"password", "token", "secret", "credential", "apikey", "privatekey", "passphrase",
}

// IsSecretPath reports whether a dotted value path should be redacted, given
// the value at that path.
//
// The value matters, not just the name. Redaction replaces a value with a
// non-empty STRING, so redacting a bool silently changes what the chart does:
// `preview.copyGithubAppSecret: false` became a non-empty string, which is
// truthy in Go templates, and reinstalling from the export ENABLED a
// secret-copying Job and its RBAC that the operator had deliberately turned
// off. A credential is never a bool or a number, so the name-pattern catch-all
// only ever fires on strings. Explicit paths still redact whatever they find.
//
// Pass nil for value when only the name is known; the catch-all then applies
// as before.
func IsSecretPath(path string, value any) bool {
	if secretPaths[path] || defensiveSecretPaths[path] {
		return true
	}
	leaf := path
	if i := strings.LastIndex(path, "."); i >= 0 {
		leaf = path[i+1:]
	}
	lower := strings.ToLower(leaf)
	if referenceKeys[lower] {
		return false
	}
	if !patternRedactable(value) {
		return false
	}
	return matchesCredentialPattern(lower)
}

// matchesCredentialPattern is the name half of the catch-all, split out so the
// classification test can ask the question without also asking IsSecretPath —
// which is what made the previous version of that test a tautology.
func matchesCredentialPattern(lowerLeaf string) bool {
	for _, sub := range credentialSubstrings {
		if strings.Contains(lowerLeaf, sub) {
			return true
		}
	}
	return false
}

// patternRedactable reports whether a value is the KIND of thing the name
// pattern may redact. Strings can hold credentials; bools and numbers cannot,
// and rewriting them to a string changes chart semantics. An unknown/nil value
// is treated as redactable so name-only callers keep failing closed.
func patternRedactable(value any) bool {
	switch value.(type) {
	case bool, int, int32, int64, float32, float64:
		return false
	default:
		return true
	}
}

// ExplicitlyClassified reports whether a path is classified by NAME — either
// declared secret or declared a reference. Used by the classification test to
// find chart values that only the catch-all covers.
func ExplicitlyClassified(path string) bool {
	if secretPaths[path] || defensiveSecretPaths[path] {
		return true
	}
	leaf := path
	if i := strings.LastIndex(path, "."); i >= 0 {
		leaf = path[i+1:]
	}
	return referenceKeys[strings.ToLower(leaf)]
}

// MatchesCredentialPatternForTest exposes the name-pattern check to tests.
func MatchesCredentialPatternForTest(lowerLeaf string) bool {
	return matchesCredentialPattern(lowerLeaf)
}

// RedactTree walks a decoded values tree and replaces every secret-bearing
// leaf with Redacted, returning a new tree. The input is not mutated.
//
// Maps are walked by key; slices are walked by index but their PATH does not
// include the index, so `api.callers` classifies as a unit rather than
// requiring `api.callers.0.key` to be enumerated. A secret path that resolves
// to a container redacts the whole container — losing the shape of a
// credential list is the correct trade.
func RedactTree(in map[string]any) map[string]any {
	return redactMap(in, "")
}

// walkTree visits every SECRET path in the tree, using the same traversal as
// redactMap/redactValue. Shared so the redactor and the "what was removed"
// report can never disagree about what counts as redacted.
func walkTree(in map[string]any, prefix string, visit func(path string, value any)) {
	for k, v := range in {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if IsSecretPath(path, v) {
			visit(path, v)
			continue
		}
		walkTreeValue(v, path, visit)
	}
}

func walkTreeValue(v any, path string, visit func(path string, value any)) {
	switch t := v.(type) {
	case map[string]any:
		walkTree(t, path, visit)
	case []any:
		for _, item := range t {
			walkTreeValue(item, path, visit)
		}
	}
}

func redactMap(in map[string]any, prefix string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if IsSecretPath(path, v) {
			out[k] = Redacted
			continue
		}
		out[k] = redactValue(v, path)
	}
	return out
}

func redactValue(v any, path string) any {
	switch t := v.(type) {
	case map[string]any:
		return redactMap(t, path)
	case []any:
		outs := make([]any, 0, len(t))
		for _, item := range t {
			// Slice elements inherit the parent path — see RedactTree.
			outs = append(outs, redactValue(item, path))
		}
		return outs
	default:
		return v
	}
}

// SecretPathsForTest exposes the explicit path set to the classification test
// in this package's test binary. Not part of the public contract. Excludes
// defensiveSecretPaths, which are deliberately absent from the chart.
func SecretPathsForTest() map[string]bool { return secretPaths }

// ReferenceKeysForTest exposes the reference-key set to the classification
// test. Not part of the public contract.
func ReferenceKeysForTest() map[string]bool { return referenceKeys }
