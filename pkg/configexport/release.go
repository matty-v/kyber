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

// kyberChartName is the chart this control plane belongs to. Used to ignore
// other Helm releases sharing the namespace.
const kyberChartName = "kyber"

// maxReleasePayload bounds the uncompressed release JSON. Helm inlines the
// chart sources and rendered manifest, so this is generous; exceeding it is
// reported rather than silently truncated.
const maxReleasePayload = 32 << 20

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
			Name    string `json:"name"`
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
	// Narrowed by Helm's own `owner=helm` label.
	//
	// NOTE, corrected: this is NOT a server-side guarantee. The client here is
	// the manager's cache-backed client, whose informer already LIST/WATCHes
	// every Secret in the namespace, so the selector is applied in-process.
	// The incremental exposure is nil (the reconciler caches Secrets anyway —
	// see the secrets rule in clusterrole.yaml), but an earlier version of
	// this comment claimed the credentials were kept out of the process
	// entirely, which is false and exactly the kind of guarantee a later
	// change would lean on. The label is a correctness filter, not a security
	// boundary.
	//
	// If Helm ever stopped setting the label a real release would read as "not
	// a Helm release"; the type check below is the authoritative filter.
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

// newestRelease picks the live Kyber release.
//
// Three filters, each closing a way of exporting the wrong thing:
//
//  1. Type — only Helm release Secrets.
//  2. Chart name — only THIS chart. Any other Helm release co-installed in the
//     namespace also carries owner=helm and the same Secret type; picking by
//     revision alone would return that chart's values (labelled with its own
//     release name), and an operator following the export would rebuild
//     something else entirely.
//  3. Status — prefer `deployed`. Helm retains failed and superseded
//     revisions, so after a failed `helm upgrade` the highest revision is the
//     one that did NOT take while the cluster still runs the previous one.
//     Exporting the failed revision hands back config the cluster is not
//     running.
//
// Only after those does revision ordering decide.
func newestRelease(items []corev1.Secret) (*helmRelease, error) {
	type candidate struct {
		revision int
		rel      *helmRelease
	}
	var deployed, others []candidate
	for _, s := range items {
		if string(s.Type) != helmReleaseSecretType {
			continue
		}
		rel, err := decodeRelease(s)
		if err != nil {
			// One unreadable revision must not sink the export — an older
			// revision may still answer the question.
			continue
		}
		if rel.Chart.Metadata.Name != "" && rel.Chart.Metadata.Name != kyberChartName {
			continue
		}
		c := candidate{revision: revisionFromName(s.Name), rel: rel}
		if rel.Info.Status == "deployed" {
			deployed = append(deployed, c)
		} else {
			others = append(others, c)
		}
	}
	pick := deployed
	if len(pick) == 0 {
		// No revision is marked deployed (an older Helm, or a release
		// mid-operation). Fall back to newest-overall rather than reporting
		// "not a Helm release", which would be a worse lie.
		pick = others
	}
	if len(pick) == 0 {
		return nil, nil
	}
	sort.Slice(pick, func(i, j int) bool { return pick[i].revision > pick[j].revision })
	return pick[0].rel, nil
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
		// Read one byte past the cap so hitting it is DETECTABLE. io.LimitReader
		// returns no error at the limit, so the previous version truncated
		// silently and surfaced "unexpected end of JSON input" — a misleading
		// error for a legitimate install. Helm's release JSON inlines the chart
		// sources and the rendered manifest, so a large chart can genuinely
		// approach this.
		plain, err := io.ReadAll(io.LimitReader(gz, maxReleasePayload+1))
		if err != nil {
			return nil, fmt.Errorf("gunzip: %w", err)
		}
		if len(plain) > maxReleasePayload {
			return nil, fmt.Errorf("release payload exceeds %d bytes uncompressed — refusing to export a truncated config", maxReleasePayload)
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
//
// Walks with the SAME traversal RedactTree uses — including descending into
// slices. An earlier version only recursed into maps, so any override shaped
// as a list of objects was redacted in the values but never listed here: the
// operator rebuilding would not be told they had to re-supply it, which is the
// precise failure this field exists to prevent.
func collectRedactedPaths(in map[string]any) []string {
	var found []string
	walkTree(in, "", func(path string, _ any) { found = append(found, path) })
	sort.Strings(found)
	return found
}

// marshalYAML renders the redacted tree. Kept as a seam so the renderer can
// change without touching Load.
func marshalYAML(v any) ([]byte, error) {
	return yaml.Marshal(v)
}
