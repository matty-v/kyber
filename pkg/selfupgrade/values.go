package selfupgrade

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// PinnedImagePaths returns the dotted paths of any image tag or digest the
// operator has set in their own values.
//
// This is a fail-closed guard, not a style check. Under the pull model the
// chart version IS the version: chart 1.0.2 carries the 1.0.2 image tags in
// its defaults. An operator values file that also pins `image.controlPlane.tag`
// overrides those defaults, so `helm upgrade` to a new chart would rewrite the
// templates, report success, and leave every container running the OLD build.
// The release history would say 1.0.2 and /api/v1/version would say 1.0.1 —
// the exact "half-works" failure the whole design is trying to avoid.
//
// This is not hypothetical: it is the shape of every cluster ArgoCD manages
// today (kyber-deploy pins all eight images to `v1.0.1@sha256:…`), which is
// correct for a repo-driven deploy and wrong for a self-upgrading one. Those
// clusters must have the pins removed as part of adoption, and this guard is
// what makes that a visible precondition instead of a silent no-op.
func PinnedImagePaths(valuesYAML string) ([]string, error) {
	if strings.TrimSpace(valuesYAML) == "" {
		return nil, nil
	}
	var root map[string]any
	if err := yaml.Unmarshal([]byte(valuesYAML), &root); err != nil {
		return nil, fmt.Errorf("parse values: %w", err)
	}
	image, ok := root["image"].(map[string]any)
	if !ok {
		return nil, nil
	}

	var found []string
	for component, raw := range image {
		spec, ok := raw.(map[string]any)
		if !ok {
			// `image.pullPolicy` and friends are scalars at this level.
			continue
		}
		for _, field := range []string{"tag", "digest"} {
			v, present := spec[field]
			if !present {
				continue
			}
			// An explicitly empty tag is how a values file says "take the
			// chart default", so it is not a pin.
			if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" {
				continue
			}
			found = append(found, fmt.Sprintf("image.%s.%s", component, field))
		}
	}
	sort.Strings(found)
	return found, nil
}

// PinnedImageError is the operator-facing explanation for a refused upgrade.
// Written as instructions rather than a diagnosis: whoever reads this is
// looking at a Job log and needs to know what to change.
func PinnedImageError(paths []string) error {
	return fmt.Errorf(
		"refusing to upgrade: this release's values pin image versions (%s), which override the chart's. "+
			"The upgrade would install the new chart and keep running the old images — Helm would report success "+
			"while nothing actually changed. Remove those keys from your values so the chart version decides what runs, "+
			"then upgrade again",
		strings.Join(paths, ", "))
}
