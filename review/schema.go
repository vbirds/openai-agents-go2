package review

import (
	"encoding/json"
	"fmt"
	"sort"

	gjs "github.com/google/jsonschema-go/jsonschema"

	libjs "github.com/MitulShah1/openai-agents-go/jsonschema"
)

// defaultSchemaJSON is the built-in review result schema. It deliberately
// restricts itself to keywords supported by OpenAI structured outputs in
// strict mode (every object closes additionalProperties and requires all of
// its properties), so it is always enforced natively by the model.
const defaultSchemaJSON = `{
  "type": "object",
  "description": "Result of an automated code review.",
  "additionalProperties": false,
  "properties": {
    "summary": {
      "type": "string",
      "description": "Short overall assessment of the change (2-4 sentences)."
    },
    "verdict": {
      "type": "string",
      "enum": ["approve", "request_changes", "comment"],
      "description": "Overall recommendation: approve if the change is safe to merge, request_changes if it has issues that must be fixed, comment if there are only optional remarks."
    },
    "confidence": {
      "type": "number",
      "description": "Confidence in the verdict, between 0 and 1."
    },
    "findings": {
      "type": "array",
      "description": "Individual review comments, ordered from most to least severe. Empty if the change is clean.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "title": {
            "type": "string",
            "description": "One-line summary of the issue."
          },
          "body": {
            "type": "string",
            "description": "Explanation of the issue, why it matters, and its impact."
          },
          "severity": {
            "type": "string",
            "enum": ["critical", "high", "medium", "low", "info"],
            "description": "critical: data loss, security hole, or guaranteed crash. high: likely bug or vulnerability. medium: probable issue or significant maintainability concern. low: minor issue. info: informational remark."
          },
          "category": {
            "type": "string",
            "enum": ["bug", "security", "performance", "style", "maintainability", "testing", "documentation", "other"],
            "description": "Kind of issue."
          },
          "confidence": {
            "type": "number",
            "description": "Confidence that the finding is real and correctly located, between 0 and 1."
          },
          "file": {
            "type": "string",
            "description": "Path of the affected file exactly as it appears in the diff or provided files."
          },
          "line_start": {
            "type": "integer",
            "description": "First affected line in the post-change file (1-based). 0 if not tied to specific lines."
          },
          "line_end": {
            "type": "integer",
            "description": "Last affected line (inclusive). 0 if not tied to specific lines."
          },
          "suggestion": {
            "type": "string",
            "description": "Concrete proposed fix (code or prose). Empty string if none."
          }
        },
        "required": ["title", "body", "severity", "category", "confidence", "file", "line_start", "line_end", "suggestion"]
      }
    }
  },
  "required": ["summary", "verdict", "confidence", "findings"]
}`

// DefaultSchema returns the built-in review result schema as raw JSON.
func DefaultSchema() json.RawMessage {
	return json.RawMessage(defaultSchemaJSON)
}

// outputSchema is the compiled form of an output schema: a validator that is
// always applied client-side, plus (when possible) a native representation
// accepted by the structured-outputs API.
type outputSchema struct {
	name string
	raw  json.RawMessage

	// resolved validates instances client-side.
	resolved *gjs.Resolved

	// native is the SDK schema used for the response_format parameter.
	// nil when the raw schema uses keywords the SDK cannot represent, in
	// which case the schema is enforced via the prompt instead.
	native *libjs.Schema

	// lostKeywords lists schema keywords that could not be represented
	// natively (the reason native is nil).
	lostKeywords []string
}

// compileSchema parses and resolves a raw JSON Schema, and attempts to build
// a native structured-outputs representation of it.
func compileSchema(raw json.RawMessage, name string) (*outputSchema, error) {
	if name == "" {
		name = "code_review"
	}

	var schema gjs.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSchema, err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSchema, err)
	}

	out := &outputSchema{name: name, raw: raw, resolved: resolved}
	native, lost, err := toNativeSchema(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSchema, err)
	}
	if len(lost) == 0 {
		out.native = native
	} else {
		out.lostKeywords = lost
	}
	return out, nil
}

// validate checks an already-unmarshaled JSON instance against the schema.
func (s *outputSchema) validate(instance any) error {
	return s.resolved.Validate(instance)
}

// toNativeSchema converts a raw JSON Schema into the SDK's schema type and
// reports which keywords were dropped in the conversion. A non-empty lost
// list means native structured outputs cannot faithfully enforce the schema.
func toNativeSchema(raw json.RawMessage) (*libjs.Schema, []string, error) {
	var native libjs.Schema
	if err := json.Unmarshal(raw, &native); err != nil {
		// Shape mismatch (e.g. "required": true, boolean schemas): the SDK
		// type cannot represent it, but the schema itself may still be valid.
		return nil, []string{"(schema shape not representable: " + err.Error() + ")"}, nil
	}

	roundTrip, err := json.Marshal(&native)
	if err != nil {
		return nil, nil, err
	}

	var origMap, rtMap map[string]any
	if err := json.Unmarshal(raw, &origMap); err != nil {
		return nil, nil, fmt.Errorf("schema root must be a JSON object: %w", err)
	}
	if err := json.Unmarshal(roundTrip, &rtMap); err != nil {
		return nil, nil, err
	}

	lost := map[string]bool{}
	diffKeys("", origMap, rtMap, lost)
	if len(lost) == 0 {
		return &native, nil, nil
	}
	keys := make([]string, 0, len(lost))
	for k := range lost {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return nil, keys, nil
}

// diffKeys records key paths present in orig but missing after the SDK
// round-trip. Values are not compared: the SDK preserves values for every
// key it knows about, so a present key implies a preserved value.
func diffKeys(prefix string, orig, roundTrip map[string]any, lost map[string]bool) {
	for k, ov := range orig {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		rv, ok := roundTrip[k]
		if !ok {
			lost[path] = true
			continue
		}
		switch ovt := ov.(type) {
		case map[string]any:
			if rvt, ok := rv.(map[string]any); ok {
				diffKeys(path, ovt, rvt, lost)
			} else {
				lost[path] = true
			}
		case []any:
			rvt, ok := rv.([]any)
			if !ok || len(rvt) != len(ovt) {
				lost[path] = true
				continue
			}
			for i, item := range ovt {
				if m, ok := item.(map[string]any); ok {
					if rm, ok := rvt[i].(map[string]any); ok {
						diffKeys(fmt.Sprintf("%s[%d]", path, i), m, rm, lost)
					} else {
						lost[path] = true
					}
				}
			}
		}
	}
}
