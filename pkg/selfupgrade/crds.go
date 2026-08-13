package selfupgrade

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// crdFieldOwner identifies this tool in server-side-apply conflict messages.
// A distinct owner is deliberate: if something else ever starts managing the
// CRDs, the conflict surfaces here rather than being silently overwritten.
const crdFieldOwner = client.FieldOwner("kyber-upgrade")

// LoadCRDs reads every CustomResourceDefinition out of a chart's crds/
// directory, in a stable order.
//
// Non-CRD documents are an error rather than something to skip. Helm treats
// crds/ as a special directory whose contents are applied verbatim and never
// templated; anything else in there is a packaging mistake, and quietly
// ignoring it would let a real manifest go permanently unapplied.
func LoadCRDs(dir string) ([]*unstructured.Unstructured, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read crds directory %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".yaml", ".yml":
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var out []*unstructured.Unstructured
	for _, name := range names {
		path := filepath.Join(dir, name)
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", path, readErr)
		}
		docs, splitErr := decodeDocuments(raw)
		if splitErr != nil {
			return nil, fmt.Errorf("parse %s: %w", path, splitErr)
		}
		for _, doc := range docs {
			if doc.GetKind() != "CustomResourceDefinition" {
				return nil, fmt.Errorf("%s contains a %s named %q; crds/ may only contain CustomResourceDefinitions",
					path, doc.GetKind(), doc.GetName())
			}
			out = append(out, doc)
		}
	}
	return out, nil
}

// decodeDocuments splits a multi-document YAML stream into objects, dropping
// empty documents (a trailing `---` is common and harmless).
func decodeDocuments(raw []byte) ([]*unstructured.Unstructured, error) {
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var out []*unstructured.Unstructured
	for {
		obj := &unstructured.Unstructured{}
		err := dec.Decode(obj)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if len(obj.Object) == 0 {
			continue
		}
		if obj.GetKind() == "" {
			return nil, fmt.Errorf("document has no kind")
		}
		out = append(out, obj)
	}
}

// ApplyCRDs server-side-applies each CRD and returns the names it applied.
//
// This step exists because `helm upgrade` does not upgrade CRDs — Helm installs
// crds/ once at install time and never touches them again, by design. Every
// other part of a self-upgrade is Helm's job; this part is nobody's, which on
// these clusters is literally true: the Kyber CRDs carry no ArgoCD tracking-id
// and no Helm release annotations, so nothing has ever upgraded them.
//
// The caller must treat any error here as fatal and skip the upgrade. Shipping
// new templates against an old CRD schema is the single most likely way to
// half-break a cluster: the controller starts rejecting fields the new chart
// writes, and the failure shows up later, somewhere else.
func ApplyCRDs(ctx context.Context, c client.Client, crds []*unstructured.Unstructured) ([]string, error) {
	applied := make([]string, 0, len(crds))
	for _, crd := range crds {
		obj := crd.DeepCopy()
		// Server-side apply needs a clean object: resourceVersion from a file
		// is meaningless and is rejected outright on an apply patch.
		obj.SetResourceVersion("")
		if err := c.Patch(ctx, obj, client.Apply, crdFieldOwner, client.ForceOwnership); err != nil {
			return applied, fmt.Errorf("apply CRD %s: %w", crd.GetName(), err)
		}
		applied = append(applied, crd.GetName())
	}
	return applied, nil
}
