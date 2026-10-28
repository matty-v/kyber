package runtimedetect

import (
	"fmt"
	"regexp"
)

// ReasoningEffortEvidence is an advertisement from a particular native client,
// not proof of provider execution or a grant to configure the Agent. Omission
// means unknown; an explicitly empty Levels means the catalog offers none.
// Current Agent identity, version, model and route must be checked separately
// before this evidence can drive any setting. No UI capability is enabled here.
type ReasoningEffortEvidence struct {
	Source         string   `json:"source"`
	HarnessVersion string   `json:"harnessVersion"`
	Levels         []string `json:"levels"`
}

var effortLevelPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
var effortVersionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]{0,63}$`)

// ValidateEffortLevels bounds native advertised values without assuming today's
// effort vocabulary is universal. Values remain data, never shell fragments.
func ValidateEffortLevels(levels []string) error {
	if len(levels) > 16 {
		return fmt.Errorf("too many reasoning effort levels")
	}
	seen := make(map[string]bool, len(levels))
	for _, level := range levels {
		if !effortLevelPattern.MatchString(level) || seen[level] {
			return fmt.Errorf("invalid or duplicate reasoning effort level")
		}
		seen[level] = true
	}
	return nil
}

func (e *ReasoningEffortEvidence) Validate(runtime string) error {
	if e == nil {
		return nil
	}
	if runtime != "codex" || e.Source != "codex-model-list" {
		return fmt.Errorf("unsupported reasoning effort evidence source")
	}
	if !effortVersionPattern.MatchString(e.HarnessVersion) {
		return fmt.Errorf("invalid reasoning effort harness version")
	}
	if e.Levels == nil {
		return fmt.Errorf("missing reasoning effort levels")
	}
	return ValidateEffortLevels(e.Levels)
}

func cloneModels(models []Model) []Model {
	out := append([]Model(nil), models...)
	for i := range out {
		if e := out[i].ReasoningEffort; e != nil {
			copy := *e
			if e.Levels != nil {
				copy.Levels = append([]string{}, e.Levels...)
			}
			out[i].ReasoningEffort = &copy
		}
	}
	return out
}
