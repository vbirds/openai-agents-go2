package review

import (
	"encoding/json"
	"testing"
)

func TestDefaultSchemaCompilesNativeAndStrict(t *testing.T) {
	s, err := compileSchema(DefaultSchema(), "")
	if err != nil {
		t.Fatalf("compileSchema: %v", err)
	}
	if s.native == nil {
		t.Fatalf("default schema must be natively representable; lost: %v", s.lostKeywords)
	}
	if s.name != "code_review" {
		t.Errorf("name = %q, want code_review", s.name)
	}
	if !isStrictCompatible(DefaultSchema()) {
		t.Error("default schema must be strict-compatible")
	}
}

func TestDefaultSchemaValidatesTypedReview(t *testing.T) {
	s, err := compileSchema(DefaultSchema(), "")
	if err != nil {
		t.Fatalf("compileSchema: %v", err)
	}

	review := Review{
		Summary:    "ok",
		Verdict:    VerdictApprove,
		Confidence: 1,
		Findings:   []Finding{},
	}
	data, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	var instance any
	if err := json.Unmarshal(data, &instance); err != nil {
		t.Fatal(err)
	}
	if err := s.validate(instance); err != nil {
		t.Errorf("marshaled Review must validate against the default schema: %v", err)
	}
}

func TestToNativeSchemaDetectsLostKeywords(t *testing.T) {
	cases := []struct {
		name       string
		schema     string
		wantNative bool
	}{
		{"supported subset", `{"type":"object","properties":{"a":{"type":"string","enum":["x"]}},"required":["a"],"additionalProperties":false}`, true},
		{"oneOf", `{"type":"object","properties":{"a":{"oneOf":[{"type":"string"}]}}}`, false},
		{"defs and ref", `{"type":"object","properties":{"a":{"$ref":"#/$defs/x"}},"$defs":{"x":{"type":"string"}}}`, false},
		{"format keyword", `{"type":"object","properties":{"a":{"type":"string","format":"date-time"}}}`, false},
		{"nested supported", `{"type":"array","items":{"type":"object","properties":{"n":{"type":"integer","minimum":0}}}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			native, lost, err := toNativeSchema(json.RawMessage(tc.schema))
			if err != nil {
				t.Fatalf("toNativeSchema: %v", err)
			}
			gotNative := native != nil && len(lost) == 0
			if gotNative != tc.wantNative {
				t.Errorf("native = %v (lost %v), want native = %v", gotNative, lost, tc.wantNative)
			}
		})
	}
}

func TestIsStrictCompatible(t *testing.T) {
	strict := `{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}},"required":["a"]}`
	if !isStrictCompatible(json.RawMessage(strict)) {
		t.Error("expected strict-compatible")
	}
	missingRequired := `{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}}}`
	if isStrictCompatible(json.RawMessage(missingRequired)) {
		t.Error("missing required must not be strict-compatible")
	}
	openObject := `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`
	if isStrictCompatible(json.RawMessage(openObject)) {
		t.Error("object without additionalProperties:false must not be strict-compatible")
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{`{"a":1}`, `{"a":1}`, false},
		{"  {\"a\":1}\n", `{"a":1}`, false},
		{"```json\n{\"a\":1}\n```", `{"a":1}`, false},
		{"Here is the result:\n{\"a\": {\"b\": 2}}\nHope that helps!", `{"a": {"b": 2}}`, false},
		{"no json here", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := extractJSON(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("extractJSON(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("extractJSON(%q): %v", tc.in, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("extractJSON(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
