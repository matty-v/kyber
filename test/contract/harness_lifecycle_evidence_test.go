package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
)

type lifecycleCatalog struct {
	SchemaVersion   int                 `json:"schemaVersion"`
	ContractVersion string              `json:"contractVersion"`
	Scenarios       []lifecycleScenario `json:"scenarios"`
}

type lifecycleScenario struct {
	ID               string              `json:"id"`
	Requirements     []string            `json:"requirements"`
	Runtimes         []string            `json:"runtimes"`
	Guarantee        string              `json:"guarantee"`
	Automated        []automatedEvidence `json:"automated,omitempty"`
	Staging          *stagingEvidence    `json:"staging,omitempty"`
	ResidualBoundary string              `json:"residualBoundary,omitempty"`
}

type automatedEvidence struct {
	File string `json:"file"`
	Test string `json:"test"`
}

type stagingEvidence struct {
	Reason    string `json:"reason"`
	Procedure string `json:"procedure"`
}

func TestHarnessLifecycleEvidenceCatalog(t *testing.T) {
	const catalogPath = "harness_lifecycle_evidence.json"
	raw, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var catalog lifecycleCatalog
	if err := decoder.Decode(&catalog); err != nil {
		t.Fatalf("decode evidence catalog: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("evidence catalog contains trailing JSON: %v", err)
	}
	for _, marker := range []string{"-----BEGIN PRIVATE KEY-----", `"password":`, `"token":`, "sk-proj-", "ghp_"} {
		if bytes.Contains(raw, []byte(marker)) {
			t.Errorf("evidence catalog contains forbidden credential-like material %q", marker)
		}
	}
	if catalog.SchemaVersion != 1 || catalog.ContractVersion != "1.0" {
		t.Fatalf("catalog versions = schema %d contract %q", catalog.SchemaVersion, catalog.ContractVersion)
	}

	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	required := map[string]bool{}
	for i := 1; i <= 10; i++ {
		required[fmt.Sprintf("HC-%02d", i)] = false
	}
	seen := map[string]bool{}
	for _, scenario := range catalog.Scenarios {
		if scenario.ID == "" || seen[scenario.ID] {
			t.Errorf("missing or duplicate scenario id %q", scenario.ID)
		}
		seen[scenario.ID] = true
		if scenario.Guarantee == "" || len(scenario.Requirements) == 0 || len(scenario.Runtimes) == 0 {
			t.Errorf("scenario %q lacks guarantee, requirements, or runtime scope", scenario.ID)
		}
		if got := sortedCopy(scenario.Runtimes); strings.Join(got, ",") != "claude-code,codex" {
			t.Errorf("scenario %q runtime scope = %v, want both production runtimes", scenario.ID, got)
		}
		if len(scenario.Automated) == 0 && scenario.Staging == nil {
			t.Errorf("scenario %q has no automated or staging evidence", scenario.ID)
		}
		for _, requirement := range scenario.Requirements {
			if _, ok := required[requirement]; !ok {
				t.Errorf("scenario %q has unknown requirement %q", scenario.ID, requirement)
			} else {
				required[requirement] = true
			}
		}
		for _, evidence := range scenario.Automated {
			validateAutomatedEvidence(t, repositoryRoot, scenario.ID, evidence)
		}
		if scenario.Staging != nil {
			if scenario.Staging.Reason == "" || scenario.Staging.Procedure == "" {
				t.Errorf("scenario %q has incomplete staging evidence", scenario.ID)
			}
			parts := strings.SplitN(scenario.Staging.Procedure, "#", 2)
			if len(parts) != 2 || parts[1] == "" {
				t.Errorf("scenario %q staging procedure must name a document section", scenario.ID)
			} else {
				validateStagingProcedure(t, repositoryRoot, scenario.ID, parts[0], parts[1])
			}
		}
	}
	for requirement, covered := range required {
		if !covered {
			t.Errorf("contract requirement %s has no lifecycle evidence mapping", requirement)
		}
	}
}

func validateStagingProcedure(t *testing.T, root, scenarioID, file, anchor string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		t.Errorf("scenario %q staging procedure: %v", scenarioID, err)
		return
	}
	for _, line := range strings.Split(string(body), "\n") {
		heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
		if markdownAnchor(heading) == anchor {
			return
		}
	}
	t.Errorf("scenario %q staging procedure anchor %q does not resolve in %s", scenarioID, anchor, file)
}

func markdownAnchor(heading string) string {
	var out strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			out.WriteRune(r)
			lastHyphen = false
		case r == ' ' || r == '-':
			if out.Len() > 0 && !lastHyphen {
				out.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	return strings.TrimRight(out.String(), "-")
}

func validateAutomatedEvidence(t *testing.T, root, scenarioID string, evidence automatedEvidence) {
	t.Helper()
	if evidence.File == "" || evidence.Test == "" || strings.Contains(evidence.File, "..") || filepath.IsAbs(evidence.File) {
		t.Errorf("scenario %q has invalid automated reference %+v", scenarioID, evidence)
		return
	}
	if !regexp.MustCompile(`^Test[A-Za-z0-9_]+$`).MatchString(evidence.Test) {
		t.Errorf("scenario %q has invalid test name %q", scenarioID, evidence.Test)
		return
	}
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(evidence.File)))
	if err != nil {
		t.Errorf("scenario %q reference %s: %v", scenarioID, evidence.File, err)
		return
	}
	pattern := regexp.MustCompile(`(?m)^func ` + regexp.QuoteMeta(evidence.Test) + `\(t \*testing\.T\)`)
	if !pattern.Match(body) {
		t.Errorf("scenario %q reference %s#%s does not resolve", scenarioID, evidence.File, evidence.Test)
	}
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
