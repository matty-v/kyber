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
// float64 above about a million in exponent form — so a plain
// `{{ .Values.x | quote }}` silently turns 26214400 into "2.62144e+07".
//
// Note this only happens for numbers that arrive as YAML: `--set x=26214400`
// is parsed as an int64 and renders correctly either way. Every test here that
// needs to exercise the bug must therefore go through a values FILE, which is
// also how real clusters are configured.
var scientificNotation = regexp.MustCompile(`^-?\d+(\.\d+)?[eE][+-]?\d+$`)

// TestNumericEnvRendersAsIntegers is the regression guard for the bug that
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
// setting somebody adds above the threshold would reproduce this exactly.
func TestNumericEnvRendersAsIntegers(t *testing.T) {
	assertNoScientificNotation(t, "chart defaults", helmTemplate(t))
}

// TestOperatorRaisedLimitsRenderAsIntegers covers what the default-values sweep
// cannot see: a setting that is small in values.yaml but which an operator
// raises past the point where float64 formatting switches to exponent form.
// Every numeric setting is settable, so any of them can reach that range.
//
// metrics.redisRetentionSeconds is the reason this test is not optional. Its
// default (604800) is under the threshold, so the defaults sweep says nothing —
// but an operator asking for 30-day retention renders "2.592e+06",
// cmd/control-plane/main.go:371 fails to parse it, drops it silently, and keeps
// the compiled 7-day default. No error is logged anywhere and points are
// evicted 23 days early.
//
// It goes through a values file rather than --set on purpose: --set values are
// already int64 by the time the template sees them and would render correctly
// even against a broken template, so a --set version of this test could never
// fail.
func TestOperatorRaisedLimitsRenderAsIntegers(t *testing.T) {
	rendered := helmTemplateWithValues(t, `
metrics:
  redisRetentionSeconds: 2592000
transcripts:
  retention:
    maxAgeDays: 30000000
    maxBytesPerAgent: 21474836480
    pruneIntervalMinutes: 10000000
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

// TestUnsetNumericValuesStayEmpty guards the regression the first fix for this
// bug introduced. Piping a value straight through `int64` maps an unset key to
// 0, and `loadTaskLimits` rejects 0 (`v <= 0`) and exits — reproducing exactly
// the unconditional crash-loop this whole change exists to remove.
//
// Every consumer of these keys treats "" as "use the compiled default", which
// is what a bare `quote` produced for an absent value. Nulling a key is Helm's
// normal way to unset one, so it has to keep working.
func TestUnsetNumericValuesStayEmpty(t *testing.T) {
	rendered := helmTemplateWithValues(t, `
api:
  durableTasks:
    maxFileBytes: null
    maxRetained: null
  agentRequests:
    maxPromptBytes: null
metrics:
  redisRetentionSeconds: null
`)

	data := controlPlaneConfigMapData(t, rendered)
	for _, key := range []string{
		"KYBER_TASKS_MAX_FILE_BYTES",
		"KYBER_TASKS_MAX_RETAINED",
		"KYBER_AGENT_REQUESTS_MAX_PROMPT_BYTES",
		"KYBER_METRICS_RETENTION_SECONDS",
	} {
		value, present := data[key]
		if !present {
			continue // omitting the key entirely is also "unset"
		}
		if value != "" {
			t.Errorf("%s = %q for an unset value; want \"\" so the process falls back to its compiled default. "+
				"%q is parsed as a real setting and 0 is rejected outright.", key, value, value)
		}
	}
}

// TestQuantityStringsArePreserved pins the other half of the helper's contract.
// transcripts.retention.maxBytesPerAgent is read with resource.ParseQuantity
// (cmd/control-plane/main.go:561), which the chart documents as accepting a
// Kubernetes quantity. Coercing values through int64 unconditionally would turn
// "200Mi" into 0 and silently disable the byte-based pruning limit.
func TestQuantityStringsArePreserved(t *testing.T) {
	rendered := helmTemplateWithValues(t, `
transcripts:
  retention:
    maxBytesPerAgent: 200Mi
`)

	got := controlPlaneContainerEnvLiterals(t, rendered)["KYBER_TRANSCRIPT_RETENTION_MAX_BYTES_PER_AGENT"]
	if got != "200Mi" {
		t.Errorf("KYBER_TRANSCRIPT_RETENTION_MAX_BYTES_PER_AGENT = %q, want %q — a quantity string must pass through untouched", got, "200Mi")
	}
}

// TestTaskByteLimitsMatchTheirChartDefaults pins the two keys whose exponent
// rendering broke v1.4.0, so the fix has a named test and not only a sweep. It
// reads the chart defaults — the values that actually shipped broken — and
// parses them the way the control plane does.
func TestTaskByteLimitsMatchTheirChartDefaults(t *testing.T) {
	data := controlPlaneConfigMapData(t, helmTemplate(t))

	want := map[string]int64{
		"KYBER_TASKS_MAX_FILE_BYTES":      26214400,
		"KYBER_TASKS_MAX_TASK_FILE_BYTES": 104857600,
	}

	for key, expected := range want {
		value, present := data[key]
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

// assertNoScientificNotation fails if any KYBER_* value the chart renders is in
// exponent form, and reports every offending key at once so a single run names
// the whole blast radius.
//
// It covers both ConfigMap data and env values written literally on a workload
// container. Sweeping only ConfigMaps is what let metrics.redisRetentionSeconds
// and the transcript-retention keys sit outside the guard.
func assertNoScientificNotation(t *testing.T, scenario, rendered string) {
	t.Helper()

	var offenders []string
	checked := 0
	check := func(source, key, value string) {
		if !strings.HasPrefix(key, "KYBER_") {
			return
		}
		checked++
		if scientificNotation.MatchString(value) {
			offenders = append(offenders, fmt.Sprintf("%s/%s = %q", source, key, value))
		}
	}

	forEachDoc(t, rendered, func(doc map[string]any) {
		metadata, _ := doc["metadata"].(map[string]any)
		name, _ := metadata["name"].(string)
		kind, _ := doc["kind"].(string)

		if kind == "ConfigMap" {
			data, _ := doc["data"].(map[string]any)
			for key, raw := range data {
				if value, ok := raw.(string); ok {
					check(name, key, value)
				}
			}
			return
		}

		for _, container := range podContainers(doc) {
			env, _ := container["env"].([]any)
			for _, e := range env {
				entry, _ := e.(map[string]any)
				key, _ := entry["name"].(string)
				if value, ok := entry["value"].(string); ok {
					check(kind+"/"+name, key, value)
				}
			}
		}
	})

	if checked == 0 {
		t.Fatalf("%s: no KYBER_* values found — has the chart moved?", scenario)
	}
	if len(offenders) > 0 {
		t.Errorf("%s: %d value(s) render in exponent form. Every consumer parses these with strconv "+
			"or resource.ParseQuantity, so the value is either rejected (the control plane exits) or "+
			"silently dropped for a compiled default. Render them through the "+
			"`kyber.numericValue` helper:\n  %s",
			scenario, len(offenders), strings.Join(offenders, "\n  "))
	}
}

// podContainers returns the containers of any workload document that has a pod
// template, plus bare Pods. Anything without one yields nothing.
func podContainers(doc map[string]any) []map[string]any {
	spec, _ := doc["spec"].(map[string]any)
	if spec == nil {
		return nil
	}
	podSpec, _ := spec["template"].(map[string]any)
	if podSpec != nil {
		podSpec, _ = podSpec["spec"].(map[string]any)
	} else {
		podSpec = spec // a bare Pod
	}
	if podSpec == nil {
		return nil
	}
	var out []map[string]any
	for _, field := range []string{"containers", "initContainers"} {
		list, _ := podSpec[field].([]any)
		for _, raw := range list {
			if container, ok := raw.(map[string]any); ok {
				out = append(out, container)
			}
		}
	}
	return out
}

// controlPlaneConfigMapData returns the control-plane ConfigMap's string data.
func controlPlaneConfigMapData(t *testing.T, rendered string) map[string]string {
	t.Helper()
	out := map[string]string{}
	forEachDoc(t, rendered, func(doc map[string]any) {
		if kind, _ := doc["kind"].(string); kind != "ConfigMap" || !isControlPlaneComponent(doc) {
			return
		}
		data, _ := doc["data"].(map[string]any)
		for key, raw := range data {
			if value, ok := raw.(string); ok {
				out[key] = value
			}
		}
	})
	if len(out) == 0 {
		t.Fatal("control-plane ConfigMap not found or empty")
	}
	return out
}

// controlPlaneContainerEnvLiterals returns the env vars set as literal values
// (not configMapKeyRef) on the control-plane container.
func controlPlaneContainerEnvLiterals(t *testing.T, rendered string) map[string]string {
	t.Helper()
	out := map[string]string{}
	forEachDoc(t, rendered, func(doc map[string]any) {
		if kind, _ := doc["kind"].(string); kind != "Deployment" || !isControlPlaneComponent(doc) {
			return
		}
		for _, container := range podContainers(doc) {
			if name, _ := container["name"].(string); name != "control-plane" {
				continue
			}
			env, _ := container["env"].([]any)
			for _, e := range env {
				entry, _ := e.(map[string]any)
				key, _ := entry["name"].(string)
				if value, ok := entry["value"].(string); ok {
					out[key] = value
				}
			}
		}
	})
	if len(out) == 0 {
		t.Fatal("control-plane container has no literal env values")
	}
	return out
}

// helmTemplateWithValues renders the chart with the given YAML merged in
// through -f, so the values reach the template as YAML-parsed floats the way an
// operator's own values file does.
func helmTemplateWithValues(t *testing.T, valuesYAML string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("helm not on PATH: %v", err)
	}
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(valuesYAML), 0o600); err != nil {
		t.Fatalf("write values file: %v", err)
	}
	// Reuse helmTemplate's required --set pins rather than copying them, so a
	// newly-required value (as in kyber#358) cannot leave this path stale.
	args := append(helmTemplateArgs(t), "-f", path)
	cmd := exec.Command("helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template failed: %v\nstderr: %s", err, stderr.String())
	}
	return stdout.String()
}
