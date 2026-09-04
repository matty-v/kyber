package chart

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// scientificNotation matches a decimal rendered in exponent form, e.g.
// "2.62144e+07". Helm parses every YAML number as a float64, and Go prints a
// float64 that large in exponent form — so a plain `{{ .Values.x | quote }}`
// silently turns 26214400 into "2.62144e+07".
//
// Note this only happens for numbers that arrive as YAML: `--set x=26214400`
// is parsed as an int64 and renders correctly either way. Every test here that
// needs to exercise the bug must therefore go through a values FILE, which is
// also how real clusters are configured.
var scientificNotation = regexp.MustCompile(`^-?\d+(\.\d+)?[eE][+-]?\d+$`)

// TestByteLimitsRenderAsIntegers is the regression guard for the bug that
// shipped in v1.4.0 and made the release uninstallable on every cluster.
//
// api.durableTasks.maxFileBytes (26214400) and maxTaskFileBytes (104857600)
// rendered into the control-plane ConfigMap as "2.62144e+07" and
// "1.048576e+08". The control plane parses them with strconv.ParseInt at
// startup (cmd/control-plane/task_config.go), fails, and calls os.Exit(1)
// *before* it looks at KYBER_TASKS_ENABLED — so every cluster crash-looped its
// new control plane whether or not durable tasks or A2A were switched on.
//
// The defect only became reachable in v1.4.0: #205 wired these ConfigMap keys
// into the container for the first time, so nothing had ever parsed them.
//
// The assertion is deliberately GENERAL. It sweeps every KYBER_* value the
// chart renders rather than naming the two keys that broke, because the next
// setting somebody adds above ten million would reproduce this exactly.
func TestByteLimitsRenderAsIntegers(t *testing.T) {
	assertNoScientificNotation(t, "chart defaults", helmTemplate(t))
}

// TestOperatorRaisedLimitsRenderAsIntegers covers what the default-values sweep
// cannot see: a limit that is small in values.yaml but which an operator raises
// past the point where float64 formatting switches to exponent form. Every
// numeric limit is settable, so any of them can reach that range.
//
// It goes through a values file rather than --set on purpose: --set values are
// already int64 by the time the template sees them and would render correctly
// even against the broken template, so a --set version of this test could
// never fail.
func TestOperatorRaisedLimitsRenderAsIntegers(t *testing.T) {
	rendered := helmTemplateWithValues(t, `
api:
  agentRequests:
    maxPromptBytes: 15000000
    maxResponseBytes: 15000000
  durableTasks:
    maxRetained: 50000000
    maxTextPartBytes: 20000000
    maxJSONPartBytes: 20000000
    maxFileBytes: 53687091200
    maxTaskFileBytes: 107374182400
`)
	assertNoScientificNotation(t, "operator-raised limits", rendered)
}

// assertNoScientificNotation fails if any KYBER_* ConfigMap value the chart
// renders is in exponent form, and reports every offending key at once so a
// single run names the whole blast radius.
func assertNoScientificNotation(t *testing.T, scenario, rendered string) {
	t.Helper()

	var offenders []string
	checked := 0
	forEachDoc(t, rendered, func(doc map[string]any) {
		if kind, _ := doc["kind"].(string); kind != "ConfigMap" {
			return
		}
		metadata, _ := doc["metadata"].(map[string]any)
		name, _ := metadata["name"].(string)
		data, _ := doc["data"].(map[string]any)
		for key, raw := range data {
			if !strings.HasPrefix(key, "KYBER_") {
				continue
			}
			value, ok := raw.(string)
			if !ok {
				continue
			}
			checked++
			if scientificNotation.MatchString(value) {
				offenders = append(offenders, fmt.Sprintf("%s/%s = %q", name, key, value))
			}
		}
	})

	if checked == 0 {
		t.Fatalf("%s: no KYBER_* ConfigMap values found — has the chart moved?", scenario)
	}
	if len(offenders) > 0 {
		t.Errorf("%s: %d ConfigMap value(s) render in exponent form, which the control plane "+
			"cannot parse as an integer and exits on at startup. Pipe them through `int64` before "+
			"`quote` in the template:\n  %s",
			scenario, len(offenders), strings.Join(offenders, "\n  "))
	}
}

// TestTaskByteLimitsMatchTheirChartDefaults pins the two keys whose exponent
// rendering broke v1.4.0, so the fix has a named test and not only a sweep. It
// reads the chart defaults — the values that actually shipped broken — and
// parses them the way the control plane does.
func TestTaskByteLimitsMatchTheirChartDefaults(t *testing.T) {
	rendered := helmTemplate(t)

	want := map[string]int64{
		"KYBER_TASKS_MAX_FILE_BYTES":      26214400,
		"KYBER_TASKS_MAX_TASK_FILE_BYTES": 104857600,
	}

	got := map[string]string{}
	forEachDoc(t, rendered, func(doc map[string]any) {
		if kind, _ := doc["kind"].(string); kind != "ConfigMap" || !isControlPlaneComponent(doc) {
			return
		}
		data, _ := doc["data"].(map[string]any)
		for key := range want {
			if value, ok := data[key].(string); ok {
				got[key] = value
			}
		}
	})

	for key, expected := range want {
		value, present := got[key]
		if !present {
			t.Errorf("%s is not rendered into the control-plane ConfigMap", key)
			continue
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			t.Errorf("%s = %q does not parse as an integer, which is what the control plane does with it: %v", key, value, err)
			continue
		}
		if parsed != expected {
			t.Errorf("%s = %d, want %d", key, parsed, expected)
		}
	}
}

// helmTemplateWithValues renders the chart with the given YAML merged in
// through -f, so the values reach the template as YAML-parsed floats the way an
// operator's own values file does. Mirrors helmTemplate's required --set pins.
func helmTemplateWithValues(t *testing.T, valuesYAML string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("helm not on PATH: %v", err)
	}
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(valuesYAML), 0o600); err != nil {
		t.Fatalf("write values file: %v", err)
	}
	args := []string{
		"template", "kyber", chartDir(t),
		"--set", "api.apiKey=test123",
		"--set", "api.webhookSecret=webhook123",
		"--set", "k3s.joinToken=K10abc",
		"--set", "k3s.serverUrl=https://10.0.0.1:6443",
		"--set", "image.controlPlane.tag=test",
		"--set", "image.nodeAgent.tag=test",
		"--set", "image.statusSidecar.tag=test",
		"--set", "image.claudeCode.tag=test",
		"-f", path,
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
