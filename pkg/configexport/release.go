package configexport

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// helmReleaseSecretType is the type Helm stamps on its release Secrets. Listing
// by type rather than by name prefix means we find the release whatever it is
// called, which matters because the release name is the cluster name and
// differs per install (kyber-razer, kyber-falcon, ...).
const helmReleaseSecretType = "helm.sh/release.v1"

// Export is the answer to "how do I recreate this cluster?".
type Export struct {
	// Available is false when this install is not a Helm release. That is the
	// normal state of an ArgoCD-managed cluster, where the values live in the
	// deploy repo and Helm has never run — see Reason.
	Available bool `json:"available"`
	// Reason explains an unavailable export in terms an operator can act on.
	Reason string `json:"reason,omitempty"`

	ReleaseName  string `json:"releaseName,omitempty"`
	Revision     int    `json:"revision,omitempty"`
	ChartVersion string `json:"chartVersion,omitempty"`

	// ValuesYAML is the operator-supplied values, secrets removed, ready to
	// commit. Empty when Available is false.
	ValuesYAML string `json:"valuesYaml,omitempty"`

	// RedactedPaths lists what was removed, so an operator knows exactly what
	// they must supply themselves rather than discovering it on restore.
	RedactedPaths []string `json:"redactedPaths,omitempty"`
}

// helmRelease is the subset of Helm's stored release object we read.
type helmRelease struct {
	Name    string         `json:"name"`
	Version int            `json:"version"`
	Config  map[string]any `json:"config"`
	Chart   struct {
		Metadata struct {
			Version string `json:"version"`
		} `json:"metadata"`
	} `json:"chart"`
	Info struct {
		Status string `json:"status"`
	} `json:"info"`
}

// Reader loads the Helm release for this install.
type Reader struct {
	Client    client.Client
	Namespace string
}

// Load returns the export for the current install.
//
// Not being a Helm release is NOT an error. Every one of Matt's clusters is in
// that state today: ArgoCD renders the chart and applies the manifests, so
// there is no `sh.helm.release.v1.*` Secret at all and the values of record
// live in the deploy repo. Returning a clear "here is where your config
// actually is" beats a 500 the operator cannot act on.
func (r *Reader) Load(ctx context.Context) (Export, error) {
	// Narrowed by Helm's own `owner=helm` label rather than listing every
	// Secret in the namespace. The namespace is full of credentials this
	// feature has no business reading into memory — agent OAuth tokens,
	// Telegram tokens, tunnel creds. Filtering server-side keeps them out of
	// the process entirely.
	//
	// Tradeoff: if Helm ever stopped setting that label, a real release would
	// read as "not a Helm release". The type check below is the authoritative
	// filter; the label is a narrowing hint that has been stable across Helm 3.
	var secrets corev1.SecretList
	if err := r.Client.List(ctx, &secrets,
		client.InNamespace(r.Namespace),
		client.MatchingLabels{"owner": "helm"},
	); err != nil {
		return Export{}, fmt.Errorf("list secrets: %w", err)
	}

	latest, err := newestRelease(secrets.Items)
	if err != nil {
		return Export{}, err
	}
	if latest == nil {
		return Export{
			Available: false,
			Reason: "This install is not a Helm release — nothing here was installed by Helm, so there are no stored values to export. " +
				"On an ArgoCD-managed cluster the values of record live in your deploy repo; that is still the source of truth.",
		}, nil
	}

	redacted := RedactTree(latest.Config)
	yamlBytes, err := marshalYAML(redacted)
	if err != nil {
		return Export{}, fmt.Errorf("render values: %w", err)
	}

	return Export{
		Available:     true,
		ReleaseName:   latest.Name,
		Revision:      latest.Version,
		ChartVersion:  latest.Chart.Metadata.Version,
		ValuesYAML:    string(yamlBytes),
		RedactedPaths: collectRedactedPaths(latest.Config),
	}, nil
}

// newestRelease picks the highest-revision release Secret. Helm keeps every
// revision, so the newest is the live one; taking the first found would export
// whatever ordering the API server happened to return.
func newestRelease(items []corev1.Secret) (*helmRelease, error) {
	type candidate struct {
		revision int
		secret   corev1.Secret
	}
	var candidates []candidate
	for _, s := range items {
		if string(s.Type) != helmReleaseSecretType {
			continue
		}
		candidates = append(candidates, candidate{revision: revisionFromName(s.Name), secret: s})
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].revision > candidates[j].revision })

	rel, err := decodeRelease(candidates[0].secret)
	if err != nil {
		return nil, fmt.Errorf("decode release %s: %w", candidates[0].secret.Name, err)
	}
	return rel, nil
}

// revisionFromName pulls N out of "sh.helm.release.v1.<name>.vN". Returns 0
// when the name does not parse, which sorts it last — a malformed name should
// never win over a well-formed one.
func revisionFromName(name string) int {
	i := strings.LastIndex(name, ".v")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(name[i+2:])
	if err != nil {
		return 0
	}
	return n
}

// decodeRelease unwraps Helm's storage format: the Secret's `release` value is
// base64(gzip(json)). Note the k8s client has already undone the Secret's own
// base64, so exactly one base64 layer remains.
func decodeRelease(s corev1.Secret) (*helmRelease, error) {
	raw, ok := s.Data["release"]
	if !ok {
		return nil, fmt.Errorf("secret has no `release` key")
	}
	decoded, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil {
		// Older Helm versions stored the payload ungzipped and unwrapped.
		// Fall back rather than failing the whole export.
		decoded = raw
	}
	if gz, err := gzip.NewReader(bytes.NewReader(decoded)); err == nil {
		defer gz.Close()
		plain, err := io.ReadAll(io.LimitReader(gz, 8<<20))
		if err != nil {
			return nil, fmt.Errorf("gunzip: %w", err)
		}
		decoded = plain
	}

	var rel helmRelease
	if err := json.Unmarshal(decoded, &rel); err != nil {
		return nil, fmt.Errorf("parse release json: %w", err)
	}
	if rel.Config == nil {
		// A release installed with no overrides. Legitimate — the export is
		// then an empty values file, which is the correct answer.
		rel.Config = map[string]any{}
	}
	return &rel, nil
}

// collectRedactedPaths reports which paths were removed, sorted, so the
// operator sees exactly what they must supply on restore.
func collectRedactedPaths(in map[string]any) []string {
	var found []string
	var walk func(node map[string]any, prefix string)
	walk = func(node map[string]any, prefix string) {
		for k, v := range node {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			if IsSecretPath(path) {
				found = append(found, path)
				continue
			}
			if child, ok := v.(map[string]any); ok {
				walk(child, path)
			}
		}
	}
	walk(in, "")
	sort.Strings(found)
	return found
}

// marshalYAML renders the redacted tree. Kept as a seam so the renderer can
// change without touching Load.
func marshalYAML(v any) ([]byte, error) {
	return yaml.Marshal(v)
}
