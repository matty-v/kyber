package inbound

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"unicode/utf8"

	"k8s.io/client-go/util/jsonpath"

	apiv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// Drop reasons used by Decide. Mirrored by the API layer when recording
// AgentInboundRun ring-buffer entries.
const (
	DropReasonUnmatchedEvent = "unmatched-event"
	DropReasonFilterRejected = "filter-rejected"
)

// Decision is the outcome of evaluating one binding against one inbound
// request. When Match is true, Envelope holds the rendered prompt body to
// dispatch. When Match is false, DropReason explains why.
type Decision struct {
	Match      bool
	DropReason string
	Envelope   string
}

// FilterResult records what one filter clause did during DecideDebug. Used
// by the operator-facing /api/v1/inbound-debug endpoint to expose exactly
// which path extracted what value and why a binding accepted or rejected
// a synthetic payload.
type FilterResult struct {
	JsonPath       string `json:"jsonPath"`
	Passed         bool   `json:"passed"`
	ExtractedValue string `json:"extractedValue"`
	// Reason is populated only when Passed is false. One of:
	// "missing-key" (path resolved to "" — we can't tell apart a literal
	// "" value from a missing key, so the label is operator-readable
	// rather than programmatically precise), "not-in-list", "not-equals",
	// or "extraction-error" if the JSONPath itself failed to evaluate.
	Reason string `json:"reason,omitempty"`
}

// FieldResult records one field extraction. Mirrors the rendered envelope's
// data block but exposes the raw extracted value before prefix application
// so an operator can tell apart "empty payload value" from "prefix swallowed
// the value."
type FieldResult struct {
	Label          string `json:"label"`
	JsonPath       string `json:"jsonPath"`
	ExtractedValue string `json:"extractedValue"`
	Truncated      bool   `json:"truncated"`
}

// DecisionDebug is DecideDebug's richer return shape. Embeds Decision so
// callers serializing for the debug endpoint inherit Match/DropReason/
// Envelope without duplicating fields.
type DecisionDebug struct {
	Decision
	// ResolvedEvent is the event identifier the dispatcher computed from
	// EventHeader and EventPath. Empty when the binding doesn't configure
	// either.
	ResolvedEvent string         `json:"resolvedEvent"`
	FilterResults []FilterResult `json:"filterResults"`
	FieldResults  []FieldResult  `json:"fieldResults"`
}

// Drop reason constants used by FilterResult.Reason. Operator-readable
// strings, not part of the public API contract today — mirrored verbatim
// in the debug endpoint response so the PWA can render them as-is.
const (
	filterReasonMissingKey      = "missing-key"
	filterReasonNotInList       = "not-in-list"
	filterReasonNotEquals       = "not-equals"
	filterReasonExtractionError = "extraction-error"
)

// Decide is the pure pipeline that decides whether (and how) to dispatch one
// inbound request:
//
//  1. Resolve event identifier from binding.EventHeader and binding.EventPath.
//  2. Reject if MatchEvents is non-empty and the resolved event is not listed.
//  3. Run all Filters; first failure rejects the request.
//  4. Extract Fields and render the envelope.
//
// The function does no I/O.
//
// JSONPath dialect: operator-supplied paths follow the conventional
// "$.foo.bar" / "$.foo[0].bar" form; we adapt them to the k8s
// client-go/util/jsonpath template syntax ({.foo.bar}) before evaluation.
// Empty / "$" paths return the entire document. Missing keys yield "".
func Decide(body []byte, headers http.Header, requestID, agent string, binding apiv1.AgentInboundBinding) Decision {
	// 1. Resolve event identifier.
	event := ""
	if binding.EventHeader != "" {
		event = headers.Get(binding.EventHeader)
	}
	if binding.EventPath != "" {
		extracted, err := evalJSONPath(body, binding.EventPath)
		if err == nil && extracted != "" {
			if event == "" {
				event = extracted
			} else {
				event = event + "." + extracted
			}
		}
	}

	// 2. matchEvents.
	if len(binding.MatchEvents) > 0 {
		if !contains(binding.MatchEvents, event) {
			return Decision{Match: false, DropReason: DropReasonUnmatchedEvent}
		}
	}

	// 3. Filters — all must pass.
	for _, f := range binding.Filters {
		if len(f.In) == 0 && f.Equals == "" {
			// Operator misconfig: filter with no clauses. Treat as pass so we
			// don't black-hole traffic on a typo.
			continue
		}
		val, err := evalJSONPath(body, f.JsonPath)
		if err != nil {
			return Decision{Match: false, DropReason: DropReasonFilterRejected}
		}
		if len(f.In) > 0 {
			if !contains(f.In, val) {
				return Decision{Match: false, DropReason: DropReasonFilterRejected}
			}
		}
		if f.Equals != "" {
			if val != f.Equals {
				return Decision{Match: false, DropReason: DropReasonFilterRejected}
			}
		}
	}

	// 4. Field extraction.
	type renderedField struct {
		label string
		value string
	}
	fields := make([]renderedField, 0, len(binding.Fields))
	for _, fld := range binding.Fields {
		raw, _ := evalJSONPath(body, fld.JsonPath)
		val := raw
		if fld.Truncate > 0 {
			val = truncateRunes(val, fld.Truncate)
		}
		if fld.Prefix != "" {
			val = fld.Prefix + val
		}
		fields = append(fields, renderedField{label: fld.Label, value: val})
	}

	// 5. Render envelope.
	var b strings.Builder
	fmt.Fprintf(&b, "[inbound:%s] binding=%s agent=%s\n", requestID, binding.Name, agent)
	b.WriteString("data:\n")
	for _, f := range fields {
		writeKV(&b, f.label, f.value)
	}
	b.WriteString("action:\n")
	for _, line := range strings.Split(binding.Action, "\n") {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteByte('\n')
	}

	return Decision{Match: true, Envelope: b.String()}
}

// writeKV writes one "  label: value" line. Multi-line values get a folded
// block-scalar feel: the first line follows "label: ", subsequent lines are
// indented 4 spaces so an agent reader can tell continuations apart.
func writeKV(b *strings.Builder, label, value string) {
	if !strings.Contains(value, "\n") {
		fmt.Fprintf(b, "  %s: %s\n", label, value)
		return
	}
	lines := strings.Split(value, "\n")
	fmt.Fprintf(b, "  %s: %s\n", label, lines[0])
	for _, l := range lines[1:] {
		b.WriteString("    ")
		b.WriteString(l)
		b.WriteByte('\n')
	}
}

// truncateRunes trims s to at most n runes (UTF-8 safe). Passing n<=0 returns
// s unchanged; the caller guards on Truncate>0 before invoking.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n])
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// evalJSONPath parses body as generic JSON and evaluates a JSONPath
// expression. The accepted dialect is the conventional "$.foo.bar" /
// "$.foo[0].bar" form; this helper rewrites it into the k8s template form
// "{.foo.bar}" before handing it to k8s.io/client-go/util/jsonpath.
//
// Returns:
//   - "" with no error when the JSON parses but the path resolves to nothing
//     (AllowMissingKeys is enabled so missing fields are not an error).
//   - "" with an error when the body is not valid JSON.
//   - The first matched value coerced to string via fmt.Sprint otherwise.
func evalJSONPath(body []byte, path string) (string, error) {
	var doc interface{}
	if len(body) == 0 {
		// Treat empty body as empty doc; jsonpath on nil returns "".
		doc = nil
	} else if err := json.Unmarshal(body, &doc); err != nil {
		return "", err
	}

	tmpl := normalizeJSONPath(path)
	jp := jsonpath.New("inbound")
	jp.AllowMissingKeys(true)
	if err := jp.Parse(tmpl); err != nil {
		return "", err
	}

	results, err := jp.FindResults(doc)
	if err != nil {
		return "", err
	}
	for _, group := range results {
		for _, v := range group {
			return coerceJSONPathValue(v), nil
		}
	}
	return "", nil
}

// normalizeJSONPath converts a "$.foo.bar" / "foo.bar" / "$" path into the
// k8s template form "{.foo.bar}" / "{$}" expected by client-go's jsonpath
// package. Already-templated input ("{...}") is returned unchanged.
func normalizeJSONPath(path string) string {
	p := strings.TrimSpace(path)
	if p == "" {
		return "{$}"
	}
	if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
		return p
	}
	switch {
	case p == "$":
		return "{$}"
	case strings.HasPrefix(p, "$."):
		return "{." + p[2:] + "}"
	case strings.HasPrefix(p, "$["):
		// "$[0].foo" → "{$[0].foo}". Rare for our payloads but supported.
		return "{" + p + "}"
	case strings.HasPrefix(p, "."):
		return "{" + p + "}"
	default:
		// Bare "foo.bar" — treat as relative, i.e. "{.foo.bar}".
		return "{." + p + "}"
	}
}

// coerceJSONPathValue stringifies one reflect.Value returned by
// jsonpath.FindResults. nil and invalid values become "". interface{}
// pass-throughs are unwrapped before formatting so we don't print "<nil>"
// for null JSON values.
func coerceJSONPathValue(v reflect.Value) string {
	if !v.IsValid() {
		return ""
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return ""
	}
	return fmt.Sprint(v.Interface())
}

// DecideDebug runs the same pipeline as Decide but accumulates per-step
// trace data: which JSONPath each filter walked, what value it extracted,
// why it passed or failed, and the same for each Field. Used by the
// /api/v1/inbound-debug endpoint to power operator tooling.
//
// The result's embedded Decision matches Decide(...) verbatim — same Match
// boolean, same DropReason, same Envelope — so the debugger reflects
// production behaviour exactly. The trace is additive.
//
// Performance is the same as Decide (one extra slice allocation per
// filter / field — negligible relative to the JSONPath evaluation already
// happening). Decide and DecideDebug do NOT share a helper today; the
// pipeline is small enough that two parallel implementations are easier
// to read than a callback-driven shared core. If the dispatcher ever
// grows new pipeline stages we should refactor — right now duplicating is
// the right call.
func DecideDebug(body []byte, headers http.Header, requestID, agent string, binding apiv1.AgentInboundBinding) DecisionDebug {
	dbg := DecisionDebug{
		FilterResults: []FilterResult{},
		FieldResults:  []FieldResult{},
	}

	// 1. Resolve event identifier.
	event := ""
	if binding.EventHeader != "" {
		event = headers.Get(binding.EventHeader)
	}
	if binding.EventPath != "" {
		extracted, err := evalJSONPath(body, binding.EventPath)
		if err == nil && extracted != "" {
			if event == "" {
				event = extracted
			} else {
				event = event + "." + extracted
			}
		}
	}
	dbg.ResolvedEvent = event

	// 2. matchEvents.
	if len(binding.MatchEvents) > 0 {
		if !contains(binding.MatchEvents, event) {
			dbg.Decision = Decision{Match: false, DropReason: DropReasonUnmatchedEvent}
			return dbg
		}
	}

	// 3. Filters — all must pass. Keep walking even on failure so the
	// operator sees all filters in the trace, but record the FIRST
	// failure as the drop reason (matches Decide's first-failure
	// short-circuit).
	firstFailure := ""
	for _, f := range binding.Filters {
		fr := FilterResult{JsonPath: f.JsonPath, Passed: true}

		if len(f.In) == 0 && f.Equals == "" {
			// Operator misconfig: filter with no clauses. Decide treats
			// this as pass; mirror that here. Surface the empty extracted
			// value so the trace makes sense.
			val, _ := evalJSONPath(body, f.JsonPath)
			fr.ExtractedValue = val
			dbg.FilterResults = append(dbg.FilterResults, fr)
			continue
		}

		val, err := evalJSONPath(body, f.JsonPath)
		fr.ExtractedValue = val
		if err != nil {
			fr.Passed = false
			fr.Reason = filterReasonExtractionError
			dbg.FilterResults = append(dbg.FilterResults, fr)
			if firstFailure == "" {
				firstFailure = DropReasonFilterRejected
			}
			continue
		}
		// Decide treats an empty extracted value as a real "" — if the
		// operator's filter says `in: [""]` or `equals: ""`, that's a
		// legitimate match. So we evaluate the In/Equals clauses on the
		// raw `val` first; only if they fail AND val is empty do we
		// surface "missing-key" as the operator-facing reason. This
		// keeps DecideDebug's Match/DropReason byte-identical to Decide.
		matched := true
		mismatchReason := ""
		if len(f.In) > 0 {
			if !contains(f.In, val) {
				matched = false
				mismatchReason = filterReasonNotInList
			}
		}
		if matched && f.Equals != "" {
			if val != f.Equals {
				matched = false
				mismatchReason = filterReasonNotEquals
			}
		}
		if !matched {
			fr.Passed = false
			// Promote "missing-key" only when the empty value caused the
			// mismatch — i.e. the operator probably typo'd a JSONPath.
			// If In/Equals legitimately accept "", val won't trip this.
			if val == "" {
				fr.Reason = filterReasonMissingKey
			} else {
				fr.Reason = mismatchReason
			}
			dbg.FilterResults = append(dbg.FilterResults, fr)
			if firstFailure == "" {
				firstFailure = DropReasonFilterRejected
			}
			continue
		}
		dbg.FilterResults = append(dbg.FilterResults, fr)
	}
	if firstFailure != "" {
		dbg.Decision = Decision{Match: false, DropReason: firstFailure}
		return dbg
	}

	// 4. Field extraction — record raw value (before prefix) plus
	// truncation flag.
	type renderedField struct {
		label string
		value string
	}
	fields := make([]renderedField, 0, len(binding.Fields))
	for _, fld := range binding.Fields {
		raw, _ := evalJSONPath(body, fld.JsonPath)
		val := raw
		truncated := false
		if fld.Truncate > 0 {
			truncatedVal := truncateRunes(val, fld.Truncate)
			if truncatedVal != val {
				truncated = true
			}
			val = truncatedVal
		}
		dbg.FieldResults = append(dbg.FieldResults, FieldResult{
			Label:          fld.Label,
			JsonPath:       fld.JsonPath,
			ExtractedValue: raw,
			Truncated:      truncated,
		})
		// Apply prefix to the rendered (post-truncation) value, matching
		// Decide. Note we record the pre-prefix raw above so the trace
		// exposes the upstream value unmodified.
		if fld.Prefix != "" {
			val = fld.Prefix + val
		}
		fields = append(fields, renderedField{label: fld.Label, value: val})
	}

	// 5. Render envelope — same rules as Decide.
	var b strings.Builder
	fmt.Fprintf(&b, "[inbound:%s] binding=%s agent=%s\n", requestID, binding.Name, agent)
	b.WriteString("data:\n")
	for _, f := range fields {
		writeKV(&b, f.label, f.value)
	}
	b.WriteString("action:\n")
	for _, line := range strings.Split(binding.Action, "\n") {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteByte('\n')
	}

	dbg.Decision = Decision{Match: true, Envelope: b.String()}
	return dbg
}
