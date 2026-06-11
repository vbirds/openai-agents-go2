package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// userReviewSchemaV3 is a real production schema (verbatim): Chinese review
// output with a const schema_version, 0-100 score, and a rich issue shape.
// It uses $schema and const, which the SDK schema type cannot represent, so
// it exercises the prompt-enforced path end to end.
const userReviewSchemaV3 = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["schema_version", "score", "summary", "issues"],
  "properties": {
    "schema_version": {"type": "string", "const": "aicodereview.review.v3"},
    "score": {"type": "integer", "description": "代码整体质量评分，0-100。"},
    "summary": {"type": "string", "minLength": 1},
    "issues": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": [
          "title", "severity", "category", "detail", "suggestion", "location",
          "llm_confidence", "finding_type", "blocking", "why_likely_true", "evidence_kind"
        ],
        "properties": {
          "title":      {"type": "string"},
          "severity":   {"type": "string", "enum": ["critical", "major", "minor", "trivial", "suggestion"]},
          "category":   {"type": "string", "enum": ["correctness", "security", "best_practice", "performance", "tests", "docs", "style", "maintainability", "other"]},
          "detail":     {"type": "string"},
          "suggestion": {"type": "string"},
          "location": {
            "type": "object",
            "additionalProperties": false,
            "required": ["file_path", "start_line", "end_line", "side", "snippet"],
            "properties": {
              "file_path":  {"type": "string"},
              "start_line": {"type": "integer"},
              "end_line":   {"type": "integer"},
              "side":       {"type": "string", "enum": ["new", "old", ""]},
              "snippet":    {"type": "string"}
            }
          },
          "llm_confidence": {"type": "string", "enum": ["high", "medium", "low"]},
          "finding_type":   {"type": "string", "enum": ["bug", "risk", "advice"]},
          "blocking":       {"type": "boolean"},
          "why_likely_true": {"type": "string", "minLength": 10},
          "evidence_kind":  {"type": "string", "enum": ["syntax", "control_flow", "api_contract", "cross_file", "inference"]}
        }
      }
    }
  }
}`

// nearMissUserPayload is the kind of output weaker backends produce against
// the schema above: missing const schema_version, stringified score and
// line numbers, wrong-case enums, stringified boolean, and an invented
// extra key on a closed object.
const nearMissUserPayload = `{
  "score": "85",
  "summary": "整体实现较为完整，但存在一个测试可靠性问题。",
  "issues": [{
    "title": "Setup 失败被当作测试通过",
    "severity": "Major",
    "category": "TESTS",
    "detail": "PrepareCrossWorldBossPlayer 失败时直接 return，doctest 视为通过。",
    "suggestion": "使用 REQUIRE 断言确保前置条件失败时测试失败。",
    "location": {
      "file_path": "src/zone_svr/tests/CPlayerCrossWorldBossTest.cpp",
      "start_line": "92",
      "end_line": "94",
      "side": "New",
      "snippet": "if (!PrepareCrossWorldBossPlayer(...)) return;"
    },
    "llm_confidence": "High",
    "finding_type": "Bug",
    "blocking": "false",
    "why_likely_true": "第92行的早退分支没有任何断言，doctest 不会将其计为失败。",
    "evidence_kind": "control-flow",
    "extra_field_invented_by_model": true
  }]
}`

func TestCoerceToSchemaRepairsUserSchemaOutput(t *testing.T) {
	schema, err := compileSchema(json.RawMessage(userReviewSchemaV3), "aicodereview")
	if err != nil {
		t.Fatalf("compileSchema: %v", err)
	}
	if schema.native != nil {
		t.Error("schema with $schema/const must use the prompt-enforced path")
	}
	if schema.coerce {
		t.Error("custom schemas must not use the built-in coercer")
	}

	repaired := coerceToSchema(json.RawMessage(nearMissUserPayload), schema.raw)
	var instance any
	if err := json.Unmarshal(repaired, &instance); err != nil {
		t.Fatal(err)
	}
	if err := schema.validate(instance); err != nil {
		t.Fatalf("repaired output must validate: %v\n%s", err, repaired)
	}

	var out struct {
		SchemaVersion string `json:"schema_version"`
		Score         int    `json:"score"`
		Issues        []struct {
			Severity     string `json:"severity"`
			Category     string `json:"category"`
			Confidence   string `json:"llm_confidence"`
			FindingType  string `json:"finding_type"`
			Blocking     bool   `json:"blocking"`
			EvidenceKind string `json:"evidence_kind"`
			Location     struct {
				StartLine int    `json:"start_line"`
				Side      string `json:"side"`
			} `json:"location"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(repaired, &out); err != nil {
		t.Fatal(err)
	}
	if out.SchemaVersion != "aicodereview.review.v3" {
		t.Errorf("missing const not filled: %q", out.SchemaVersion)
	}
	if out.Score != 85 {
		t.Errorf("score = %d, want 85", out.Score)
	}
	issue := out.Issues[0]
	if issue.Severity != "major" || issue.Category != "tests" ||
		issue.Confidence != "high" || issue.FindingType != "bug" {
		t.Errorf("enums not snapped: %+v", issue)
	}
	if issue.Blocking != false {
		t.Errorf("blocking = %v, want false", issue.Blocking)
	}
	if issue.EvidenceKind != "control_flow" {
		t.Errorf("evidence_kind = %q, want control_flow (dash normalized)", issue.EvidenceKind)
	}
	if issue.Location.StartLine != 92 || issue.Location.Side != "new" {
		t.Errorf("location not coerced: %+v", issue.Location)
	}
	if strings.Contains(string(repaired), "extra_field_invented_by_model") {
		t.Error("unknown key on closed object must be dropped")
	}
}

func TestReviewEndToEndWithUserSchema(t *testing.T) {
	model := &fakeModel{responses: []string{nearMissUserPayload}}
	r := newTestReviewer(t, model)

	req := defaultRequest()
	req.Schema = json.RawMessage(userReviewSchemaV3)
	req.SchemaName = "aicodereview"

	resp, err := r.Review(context.Background(), req)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if resp.Retries != 0 {
		t.Errorf("retries = %d, want 0 (generic coercion must repair without a retry)", resp.Retries)
	}
	if resp.Review != nil {
		t.Error("custom schema must not populate the typed Review")
	}
	if !json.Valid(resp.Output) || !strings.Contains(string(resp.Output), "aicodereview.review.v3") {
		t.Errorf("output not repaired: %s", resp.Output)
	}
	// Prompt mode: the schema must be in the system prompt so any backend
	// can comply.
	sys := model.requests[0].Messages[0].OfSystem
	if sys == nil || !strings.Contains(sys.Content.OfString.Value, "llm_confidence") {
		t.Error("custom schema must be embedded in the system prompt")
	}
	// And no native response_format is sent for non-representable schemas.
	if model.requests[0].ResponseFormat.OfJSONSchema != nil {
		t.Error("schema with const/$schema must not claim native enforcement")
	}
}

func TestCoerceToSchemaConservative(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"level": {"type": "string", "enum": ["a", "b"]},
			"count": {"type": "integer"},
			"open":  {"type": "object", "properties": {"x": {"type": "string"}}},
			"ref":   {"$ref": "#/$defs/x"}
		},
		"$defs": {"x": {"type": "string"}}
	}`)

	// Values that cannot be confidently repaired stay untouched.
	out := string(coerceToSchema(json.RawMessage(
		`{"level":"zzz","count":"not a number","open":{"x":"v","extra":1},"ref":123}`), schema))
	for _, want := range []string{`"level":"zzz"`, `"count":"not a number"`, `"extra":1`, `"ref":123`} {
		if !strings.Contains(out, want) {
			t.Errorf("conservative pass-through violated, missing %s in %s", want, out)
		}
	}

	// Invalid JSON passes through.
	if got := coerceToSchema(json.RawMessage(`{broken`), schema); string(got) != `{broken` {
		t.Error("invalid JSON must pass through")
	}
}
