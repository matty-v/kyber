package chart

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Compare the packaging loop against both the chart's image catalog and actual
// release builds. Rendering alone misses optional channels with empty tags.
func TestReleaseStampsEveryBuiltChartImage(t *testing.T) {
	root := filepath.Join(chartDir(t), "..", "..", "..")
	workflow, err := os.ReadFile(filepath.Join(root, ".github/workflows/release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	values, err := os.ReadFile(filepath.Join(chartDir(t), "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Image map[string]struct {
			Repository string `yaml:"repository"`
		} `yaml:"image"`
	}
	if err := yaml.Unmarshal(values, &chart); err != nil {
		t.Fatal(err)
	}
	var release struct {
		Jobs map[string]struct {
			With struct {
				Image string `yaml:"image"`
			} `yaml:"with"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &release); err != nil {
		t.Fatal(err)
	}
	built := map[string]bool{}
	for _, job := range release.Jobs {
		if job.With.Image != "" {
			built[job.With.Image] = true
		}
	}
	loop := regexp.MustCompile(`for c in ([A-Za-z ]+); do\s+yq -i "\.image\.\$\{c\}\.tag`).FindSubmatch(workflow)
	if len(loop) != 2 {
		t.Fatal("release image stamping loop missing")
	}
	stamped := map[string]bool{}
	for _, key := range strings.Fields(string(loop[1])) {
		stamped[key] = true
	}
	for key, image := range chart.Image {
		name := filepath.Base(image.Repository)
		if built[name] && !stamped[key] {
			t.Errorf("built image %s (%s) missing from packaged release tags", name, key)
		}
		if stamped[key] && !built[name] {
			t.Errorf("stamped image %s (%s) has no release build", name, key)
		}
	}
	for key := range stamped {
		if _, ok := chart.Image[key]; !ok {
			t.Errorf("stamping unknown chart image %s", key)
		}
	}
}

// Main publishes a canary chart after every merge. Keep its retag and chart
// stamping loops in lockstep with the complete image catalog: an omitted
// optional sidecar otherwise produces a healthy-looking upgrade whose control
// plane can save channel config but can never inject the channel container.
func TestMainBuildPublishesEveryChartImage(t *testing.T) {
	root := filepath.Join(chartDir(t), "..", "..", "..")
	workflow, err := os.ReadFile(filepath.Join(root, ".github/workflows/build.yml"))
	if err != nil {
		t.Fatal(err)
	}
	values, err := os.ReadFile(filepath.Join(chartDir(t), "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Image map[string]struct {
			Repository string `yaml:"repository"`
		} `yaml:"image"`
	}
	if err := yaml.Unmarshal(values, &chart); err != nil {
		t.Fatal(err)
	}

	retagLoop := regexp.MustCompile(`for repo in ([A-Za-z0-9\\\\\n -]+); do`).FindSubmatch(workflow)
	if len(retagLoop) != 2 {
		t.Fatal("main image retagging loop missing")
	}
	retagged := map[string]bool{}
	for _, name := range strings.Fields(strings.ReplaceAll(string(retagLoop[1]), `\`, "")) {
		retagged[name] = true
	}
	stampLoop := regexp.MustCompile(`for c in ([A-Za-z ]+); do\s+yq -i "\.image\.\$\{c\}\.tag`).FindSubmatch(workflow)
	if len(stampLoop) != 2 {
		t.Fatal("main image stamping loop missing")
	}
	stamped := map[string]bool{}
	for _, key := range strings.Fields(string(stampLoop[1])) {
		stamped[key] = true
	}

	for key, image := range chart.Image {
		name := filepath.Base(image.Repository)
		if !retagged[name] {
			t.Errorf("chart image %s (%s) missing from main version tags", name, key)
		}
		if !stamped[key] {
			t.Errorf("chart image %s (%s) missing from packaged main chart tags", name, key)
		}
	}
}
