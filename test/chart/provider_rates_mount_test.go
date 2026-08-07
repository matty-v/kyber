// Package chart tests the Helm chart's rendered output.
// These tests shell out to `helm template` and assert structural properties
// of the rendered manifests. They are skipped when the `helm` binary is not
// on PATH, but CI installs it before running the suite.
package chart

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// chartDir returns the path to deploy/helm/kyber relative to this test file.
func chartDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "helm", "kyber")
}

// helmTemplate runs `helm template` with the given extra --set args and returns the rendered YAML.
// Skips the test if helm is unavailable.
func helmTemplate(t *testing.T, extraSet ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("helm not on PATH: %v", err)
	}
	args := []string{
		"template", "kyber", chartDir(t),
		"--set", "api.apiKey=test123",
		"--set", "api.webhookSecret=webhook123",
		"--set", "k3s.joinToken=K10abc",
		"--set", "k3s.serverUrl=https://10.0.0.1:6443",
		// Image tags are required at render time (no Chart.AppVersion fallback;
		// see image_tag_required_test.go and kyber#358). Pin them so chart tests
		// exercise a realistic, renderable configuration. Extra --set args from
		// callers come after these and win on conflict.
		"--set", "image.controlPlane.tag=test",
		"--set", "image.nodeAgent.tag=test",
		"--set", "image.statusSidecar.tag=test",
		"--set", "image.claudeCode.tag=test",
	}
	for _, s := range extraSet {
		args = append(args, "--set", s)
	}
	cmd := exec.Command("helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template failed: %v\nstderr: %s", err, stderr.String())
	}
	return stdout.String()
}

// findControlPlaneDeployment extracts the control-plane Deployment from a multi-doc
// helm-rendered manifest stream and unmarshals it into a generic map.
func findControlPlaneDeployment(t *testing.T, rendered string) map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc == nil {
			continue
		}
		if doc["kind"] != "Deployment" {
			continue
		}
		meta, _ := doc["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if strings.HasSuffix(name, "-control-plane") {
			return doc
		}
	}
	t.Fatal("control-plane Deployment not found in rendered manifest")
	return nil
}

// container extracts the first container from a Deployment doc.
func container(t *testing.T, deploy map[string]any) map[string]any {
	t.Helper()
	spec, _ := deploy["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tpl["spec"].(map[string]any)
	containers, _ := podSpec["containers"].([]any)
	if len(containers) == 0 {
		t.Fatal("no containers in control-plane Deployment")
	}
	c, _ := containers[0].(map[string]any)
	return c
}

// podSpec extracts the pod spec from a Deployment doc.
func podSpec(t *testing.T, deploy map[string]any) map[string]any {
	t.Helper()
	spec, _ := deploy["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	ps, _ := tpl["spec"].(map[string]any)
	return ps
}

// envNames returns the set of env var names on a container.
func envNames(c map[string]any) map[string]string {
	out := map[string]string{}
	envs, _ := c["env"].([]any)
	for _, e := range envs {
		em, _ := e.(map[string]any)
		name, _ := em["name"].(string)
		val, _ := em["value"].(string)
		out[name] = val
	}
	return out
}

// volumeNames returns the set of volume names on a pod spec.
func volumeNames(ps map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	vols, _ := ps["volumes"].([]any)
	for _, v := range vols {
		vm, _ := v.(map[string]any)
		name, _ := vm["name"].(string)
		out[name] = vm
	}
	return out
}

// volumeMountNames returns the set of volumeMount names on a container.
func volumeMountNames(c map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	mounts, _ := c["volumeMounts"].([]any)
	for _, m := range mounts {
		mm, _ := m.(map[string]any)
		name, _ := mm["name"].(string)
		out[name] = mm
	}
	return out
}

// TestProviderRatesMount_DefaultValues asserts the volume / volumeMount / env-var
// triple is present when metrics.tokenRatesConfigMapName is set (the default).
func TestProviderRatesMount_DefaultValues(t *testing.T) {
	rendered := helmTemplate(t)
	deploy := findControlPlaneDeployment(t, rendered)
	c := container(t, deploy)
	ps := podSpec(t, deploy)

	// 1. Volume sourced from the ConfigMap.
	vols := volumeNames(ps)
	vol, ok := vols["provider-rates"]
	if !ok {
		t.Fatalf("expected pod volume named 'provider-rates'; volumes present: %v", keysOf(vols))
	}
	cm, _ := vol["configMap"].(map[string]any)
	if cm == nil {
		t.Fatalf("provider-rates volume must source from a configMap; got: %v", vol)
	}
	if got, want := cm["name"], "kyber-provider-rates"; got != want {
		t.Errorf("provider-rates volume configMap.name = %q, want %q", got, want)
	}

	// 2. VolumeMount on the control-plane container, read-only.
	mounts := volumeMountNames(c)
	mnt, ok := mounts["provider-rates"]
	if !ok {
		t.Fatalf("expected volumeMount named 'provider-rates' on control-plane container; mounts present: %v", keysOf(mounts))
	}
	if ro, _ := mnt["readOnly"].(bool); !ro {
		t.Errorf("provider-rates volumeMount must be readOnly: true; got: %v", mnt["readOnly"])
	}
	mountPath, _ := mnt["mountPath"].(string)
	if mountPath == "" {
		t.Fatal("provider-rates volumeMount must set mountPath")
	}
	if mountPath == "/etc/kyber" {
		t.Errorf("provider-rates mountPath %q collides with chart-version mount at /etc/kyber", mountPath)
	}

	// 3. KYBER_METRICS_TOKEN_RATES_PATH env var points to the rates file under the mount.
	envs := envNames(c)
	ratesPath, ok := envs["KYBER_METRICS_TOKEN_RATES_PATH"]
	if !ok {
		t.Fatalf("expected env KYBER_METRICS_TOKEN_RATES_PATH on control-plane container; env names: %v", keysOf(envs))
	}
	if !strings.HasPrefix(ratesPath, mountPath+"/") {
		t.Errorf("KYBER_METRICS_TOKEN_RATES_PATH = %q must point inside mountPath %q", ratesPath, mountPath)
	}
	if !strings.HasSuffix(ratesPath, "provider-rates.yaml") {
		t.Errorf("KYBER_METRICS_TOKEN_RATES_PATH = %q must end in provider-rates.yaml (the ConfigMap key)", ratesPath)
	}
}

// TestProviderRatesMount_EmptyConfigMapName asserts the triple is absent when
// metrics.tokenRatesConfigMapName is empty — preserving fail-soft for operators
// who intentionally disable cost computation.
func TestProviderRatesMount_EmptyConfigMapName(t *testing.T) {
	rendered := helmTemplate(t, "metrics.tokenRatesConfigMapName=")
	deploy := findControlPlaneDeployment(t, rendered)
	c := container(t, deploy)
	ps := podSpec(t, deploy)

	if _, ok := volumeNames(ps)["provider-rates"]; ok {
		t.Error("provider-rates volume must NOT be present when metrics.tokenRatesConfigMapName is empty")
	}
	if _, ok := volumeMountNames(c)["provider-rates"]; ok {
		t.Error("provider-rates volumeMount must NOT be present when metrics.tokenRatesConfigMapName is empty")
	}
	if _, ok := envNames(c)["KYBER_METRICS_TOKEN_RATES_PATH"]; ok {
		t.Error("KYBER_METRICS_TOKEN_RATES_PATH env var must NOT be present when metrics.tokenRatesConfigMapName is empty")
	}
}

// TestChartVersionMount_Preserved asserts the existing chart-version volume /
// volumeMount at /etc/kyber is unaffected — guards against accidental collision
// with the new provider-rates mount.
func TestChartVersionMount_Preserved(t *testing.T) {
	rendered := helmTemplate(t)
	deploy := findControlPlaneDeployment(t, rendered)
	c := container(t, deploy)
	ps := podSpec(t, deploy)

	if _, ok := volumeNames(ps)["chart-version"]; !ok {
		t.Fatal("chart-version volume regressed; expected it to remain mounted from the version ConfigMap")
	}
	mnt, ok := volumeMountNames(c)["chart-version"]
	if !ok {
		t.Fatal("chart-version volumeMount regressed; expected it to remain on the control-plane container")
	}
	if got := mnt["mountPath"]; got != "/etc/kyber" {
		t.Errorf("chart-version mountPath = %v, want /etc/kyber", got)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
