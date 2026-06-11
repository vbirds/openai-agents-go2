package review

import (
	"encoding/json"
	"strconv"
	"strings"
)

// coerceReviewOutput tolerantly repairs model output for the built-in
// review/specialist schemas before validation. Backends that ignore
// response_format produce semantically right but literally wrong values -
// "tests" instead of "testing", "confidence": "high" instead of a number,
// line numbers as strings - which would otherwise burn retries or fail the
// review. Only fields owned by the built-in schemas are touched; anything
// unrecognized is left for validation (and the corrective retry) to handle.
func coerceReviewOutput(raw json.RawMessage) json.RawMessage {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}

	if v, ok := doc["verdict"]; ok {
		doc["verdict"] = coerceVerdict(v)
	}
	if v, ok := doc["confidence"]; ok {
		doc["confidence"] = coerceConfidence(v)
	}
	if findings, ok := doc["findings"].([]any); ok {
		for _, item := range findings {
			if f, ok := item.(map[string]any); ok {
				coerceFinding(f)
			}
		}
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

func coerceFinding(f map[string]any) {
	f["category"] = coerceCategory(f["category"])
	if v, ok := f["severity"]; ok {
		f["severity"] = coerceSeverity(v)
	}
	f["confidence"] = coerceConfidence(f["confidence"])
	f["line_start"] = coerceLine(f["line_start"])
	f["line_end"] = coerceLine(f["line_end"])
	if _, ok := f["suggestion"].(string); !ok {
		f["suggestion"] = ""
	}
}

// categorySynonyms maps frequent off-enum category spellings to canonical
// values.
var categorySynonyms = map[string]string{
	"test": "testing", "tests": "testing", "unit-test": "testing", "unit_test": "testing",
	"doc": "documentation", "docs": "documentation", "document": "documentation",
	"perf": "performance", "efficiency": "performance",
	"vulnerability": "security", "vuln": "security",
	"maintenance": "maintainability", "refactor": "maintainability", "refactoring": "maintainability",
	"correctness": "bug", "logic": "bug", "defect": "bug", "error": "bug",
	"formatting": "style", "naming": "style",
}

var canonicalCategories = map[string]bool{
	"bug": true, "security": true, "performance": true, "style": true,
	"maintainability": true, "testing": true, "documentation": true, "other": true,
}

func coerceCategory(v any) any {
	s, ok := v.(string)
	if !ok {
		return "other"
	}
	c := strings.ToLower(strings.TrimSpace(s))
	if canonicalCategories[c] {
		return c
	}
	if mapped, ok := categorySynonyms[c]; ok {
		return mapped
	}
	return "other"
}

// severitySynonyms maps frequent off-enum severity spellings. Unknown
// values are left untouched: guessing a severity would corrupt gating, so
// validation and the corrective retry handle them instead.
var severitySynonyms = map[string]string{
	"blocker": "critical", "blocking": "critical", "fatal": "critical",
	"major": "high", "severe": "high", "important": "high", "error": "high",
	"moderate": "medium", "warning": "medium", "warn": "medium",
	"minor": "low", "trivial": "low", "nit": "low",
	"informational": "info", "note": "info", "notice": "info", "hint": "info",
}

func coerceSeverity(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	sev := strings.ToLower(strings.TrimSpace(s))
	if _, err := ParseSeverity(sev); err == nil {
		return sev
	}
	if mapped, ok := severitySynonyms[sev]; ok {
		return mapped
	}
	return v
}

var verdictSynonyms = map[string]string{
	"approved": "approve", "accept": "approve", "accepted": "approve", "lgtm": "approve",
	"changes_requested": "request_changes", "request changes": "request_changes",
	"needs_changes": "request_changes", "needs_work": "request_changes", "reject": "request_changes",
	"comments": "comment", "commented": "comment", "neutral": "comment",
}

func coerceVerdict(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	verdict := strings.ToLower(strings.TrimSpace(s))
	switch Verdict(verdict) {
	case VerdictApprove, VerdictRequestChanges, VerdictComment:
		return verdict
	}
	if mapped, ok := verdictSynonyms[verdict]; ok {
		return mapped
	}
	return v
}

// coerceConfidence normalizes confidence to a number in [0, 1]: numeric
// strings are parsed, percentages are scaled down, and verbal levels map to
// representative values. Missing or unintelligible confidence becomes 0.5 -
// explicitly uncertain.
func coerceConfidence(v any) any {
	switch c := v.(type) {
	case float64:
		return scaleConfidence(c)
	case string:
		s := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(c), "%")))
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return scaleConfidence(n)
		}
		switch s {
		case "very high", "certain", "highest":
			return 0.95
		case "high":
			return 0.9
		case "medium", "moderate":
			return 0.6
		case "low":
			return 0.3
		case "very low":
			return 0.1
		}
		return 0.5
	case nil:
		return 0.5
	default:
		return 0.5
	}
}

func scaleConfidence(n float64) float64 {
	if n > 1 && n <= 100 {
		n /= 100
	}
	if n < 0 {
		return 0
	}
	if n > 1 {
		return 1
	}
	return n
}

// coerceLine normalizes line anchors to non-negative integers; strings are
// parsed, anything unintelligible becomes 0 ("not tied to specific lines").
func coerceLine(v any) any {
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0
		}
		return int(n)
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil && i >= 0 {
			return i
		}
		return 0
	default:
		return 0
	}
}
