package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// realWorldSpecialistOutput is a verbatim failure from a non-OpenAI backend
// that ignored response_format: "tests" is not in the category enum and
// confidence is a verbal level instead of a number.
const realWorldSpecialistOutput = `{"findings":[{"severity":"medium","category":"tests","confidence":"high","file":"src/zone_svr/tests/CPlayerCrossWorldBossTest.cpp","line_start":92,"line_end":94,"title":"Setup failures are treated as passing tests","body":"When PrepareCrossWorldBossPlayer fails, the test case just returns.","suggestion":"Use a failing assertion for required setup."}]}`

func TestCoerceRepairsRealWorldSpecialistOutput(t *testing.T) {
	schema, err := compileSchema(json.RawMessage(specialistSchemaJSON), "specialist_findings")
	if err != nil {
		t.Fatal(err)
	}
	if !schema.coerce {
		t.Fatal("specialist schema must enable coercion")
	}

	repaired := coerceReviewOutput(json.RawMessage(realWorldSpecialistOutput))
	var instance any
	if err := json.Unmarshal(repaired, &instance); err != nil {
		t.Fatal(err)
	}
	if err := schema.validate(instance); err != nil {
		t.Fatalf("repaired output must validate: %v\n%s", err, repaired)
	}

	var out struct {
		Findings []Finding `json:"findings"`
	}
	if err := json.Unmarshal(repaired, &out); err != nil {
		t.Fatal(err)
	}
	f := out.Findings[0]
	if f.Category != "testing" {
		t.Errorf("category = %q, want testing", f.Category)
	}
	if f.Confidence != 0.9 {
		t.Errorf("confidence = %v, want 0.9", f.Confidence)
	}
	if f.Severity != SeverityMedium || f.LineStart != 92 {
		t.Errorf("untouched fields corrupted: %+v", f)
	}
}

func TestCoerceFieldRules(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"category synonym", `{"findings":[{"category":"docs"}]}`, `"category":"documentation"`},
		{"category unknown", `{"findings":[{"category":"weird"}]}`, `"category":"other"`},
		{"category missing", `{"findings":[{"title":"x"}]}`, `"category":"other"`},
		{"severity synonym", `{"findings":[{"severity":"Blocker"}]}`, `"severity":"critical"`},
		{"severity canonical case", `{"findings":[{"severity":"HIGH"}]}`, `"severity":"high"`},
		{"severity unknown kept", `{"findings":[{"severity":"weird"}]}`, `"severity":"weird"`},
		{"confidence percent", `{"findings":[{"confidence":85}]}`, `"confidence":0.85`},
		{"confidence numeric string", `{"findings":[{"confidence":"0.7"}]}`, `"confidence":0.7`},
		{"confidence percent string", `{"findings":[{"confidence":"90%"}]}`, `"confidence":0.9`},
		{"confidence missing", `{"findings":[{"title":"x"}]}`, `"confidence":0.5`},
		{"line string", `{"findings":[{"line_start":"42"}]}`, `"line_start":42`},
		{"line garbage", `{"findings":[{"line_end":"n/a"}]}`, `"line_end":0`},
		{"suggestion missing", `{"findings":[{"title":"x"}]}`, `"suggestion":""`},
		{"verdict synonym", `{"verdict":"LGTM","findings":[]}`, `"verdict":"approve"`},
		{"verdict spaced", `{"verdict":"request changes","findings":[]}`, `"verdict":"request_changes"`},
		{"top confidence verbal", `{"confidence":"very high","findings":[]}`, `"confidence":0.95`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(coerceReviewOutput(json.RawMessage(tc.in)))
			if !strings.Contains(out, tc.want) {
				t.Errorf("coerced output %s missing %s", out, tc.want)
			}
		})
	}

	// Non-JSON input is returned untouched.
	if got := coerceReviewOutput(json.RawMessage(`not json`)); string(got) != "not json" {
		t.Errorf("non-JSON must pass through, got %s", got)
	}
}

func TestReviewCoercesOffEnumOutputWithoutRetry(t *testing.T) {
	// A full default-schema response with off-enum values must succeed on
	// the first attempt thanks to coercion.
	payload := `{
		"summary": "ok", "verdict": "approved", "confidence": "high",
		"findings": [{
			"title": "t", "body": "b", "severity": "major", "category": "tests",
			"confidence": "75%", "file": "b/x.go", "line_start": "10", "line_end": "8",
			"suggestion": ""
		}]
	}`
	model := &fakeModel{responses: []string{payload}}
	r := newTestReviewer(t, model)

	resp, err := r.Review(context.Background(), defaultRequest())
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if resp.Retries != 0 {
		t.Errorf("retries = %d, want 0 (coercion must repair without a retry)", resp.Retries)
	}
	if resp.Review.Verdict != VerdictApprove {
		t.Errorf("verdict = %q, want approve", resp.Review.Verdict)
	}
	f := resp.Review.Findings[0]
	if f.Category != "testing" || f.Severity != SeverityHigh || f.Confidence != 0.75 {
		t.Errorf("finding not coerced: %+v", f)
	}
	// Coercion plus anchor normalization: b/ prefix stripped, range swapped.
	if f.File != "x.go" || f.LineStart != 8 || f.LineEnd != 10 {
		t.Errorf("anchors not normalized: %+v", f)
	}
}

func TestSchemaAlwaysEmbeddedInPrompt(t *testing.T) {
	model := &fakeModel{responses: []string{validReviewJSON}}
	r := newTestReviewer(t, model)
	if _, err := r.Review(context.Background(), defaultRequest()); err != nil {
		t.Fatalf("Review: %v", err)
	}
	// Native structured outputs are requested AND the schema is spelled out
	// in the prompt, so backends that ignore response_format still see it.
	if model.requests[0].ResponseFormat.OfJSONSchema == nil {
		t.Error("native response format must still be set")
	}
	sys := model.requests[0].Messages[0].OfSystem
	if sys == nil || !strings.Contains(sys.Content.OfString.Value, `"verdict"`) {
		t.Error("schema must be embedded in the system prompt")
	}
}

func TestCustomSchemaNotCoerced(t *testing.T) {
	schema, err := compileSchema(json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`), "custom")
	if err != nil {
		t.Fatal(err)
	}
	if schema.coerce {
		t.Error("coercion must not apply to custom schemas")
	}
}
