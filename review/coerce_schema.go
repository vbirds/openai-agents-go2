package review

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// coerceToSchema repairs an instance toward an arbitrary JSON Schema before
// validation, using only information the schema itself provides — no
// hardcoded field knowledge. It fixes the near-miss output weaker backends
// produce against custom schemas:
//
//   - const: the value is replaced with the schema's const (it is the only
//     legal value); missing required const properties are filled in
//   - enum: case-/whitespace-insensitive matches snap to the canonical value
//   - type: stringified numbers and booleans are parsed ("85" -> 85,
//     "true" -> true), integral floats become integers, and numbers become
//     strings where a string is required
//   - closed objects (additionalProperties: false): unknown keys are
//     dropped instead of failing validation
//
// Values it cannot confidently repair are left untouched for validation and
// the corrective retry. Schema combinators it does not understand ($ref,
// oneOf, ...) pass values through unchanged.
func coerceToSchema(raw json.RawMessage, schemaRaw json.RawMessage) json.RawMessage {
	var schema map[string]any
	if err := json.Unmarshal(schemaRaw, &schema); err != nil {
		return raw
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return raw
	}
	coerced := coerceValue(instance, schema)
	out, err := json.Marshal(coerced)
	if err != nil {
		return raw
	}
	return out
}

func coerceValue(value any, schema map[string]any) any {
	// $ref and combinators are beyond safe repair; pass through.
	if _, ok := schema["$ref"]; ok {
		return value
	}

	// const admits exactly one value: anything else can only be wrong.
	if c, ok := schema["const"]; ok {
		return c
	}

	if enum, ok := schema["enum"].([]any); ok {
		return coerceEnum(value, enum)
	}

	switch schemaType(schema) {
	case "object":
		return coerceObject(value, schema)
	case "array":
		return coerceArray(value, schema)
	case "integer":
		return coerceInteger(value)
	case "number":
		return coerceNumber(value)
	case "boolean":
		return coerceBoolean(value)
	case "string":
		return coerceString(value)
	default:
		return value
	}
}

func schemaType(schema map[string]any) string {
	t, _ := schema["type"].(string)
	return t
}

func coerceObject(value any, schema map[string]any) any {
	obj, ok := value.(map[string]any)
	if !ok {
		return value
	}
	properties, _ := schema["properties"].(map[string]any)

	// Closed object: unknown keys can only fail validation; drop them.
	if ap, ok := schema["additionalProperties"].(bool); ok && !ap && properties != nil {
		for key := range obj {
			if _, known := properties[key]; !known {
				delete(obj, key)
			}
		}
	}

	for key, sub := range properties {
		subSchema, ok := sub.(map[string]any)
		if !ok {
			continue
		}
		if v, present := obj[key]; present {
			obj[key] = coerceValue(v, subSchema)
		}
	}

	// Fill missing required properties that admit exactly one value.
	if required, ok := schema["required"].([]any); ok {
		for _, r := range required {
			key, ok := r.(string)
			if !ok {
				continue
			}
			if _, present := obj[key]; present {
				continue
			}
			if subSchema, ok := properties[key].(map[string]any); ok {
				if c, ok := subSchema["const"]; ok {
					obj[key] = c
				}
			}
		}
	}
	return obj
}

func coerceArray(value any, schema map[string]any) any {
	arr, ok := value.([]any)
	if !ok {
		return value
	}
	items, ok := schema["items"].(map[string]any)
	if !ok {
		return arr
	}
	for i, item := range arr {
		arr[i] = coerceValue(item, items)
	}
	return arr
}

// coerceEnum snaps near-miss strings ("High", " major ", "BEST_PRACTICE")
// onto the canonical enum value when the match is unambiguous.
func coerceEnum(value any, enum []any) any {
	for _, e := range enum {
		if value == e {
			return value
		}
	}
	s, ok := value.(string)
	if !ok {
		return value
	}
	normalized := normalizeEnumToken(s)
	for _, e := range enum {
		if es, ok := e.(string); ok && normalizeEnumToken(es) == normalized {
			return es
		}
	}
	return value
}

func normalizeEnumToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

func coerceInteger(value any) any {
	switch n := value.(type) {
	case float64:
		if n == float64(int64(n)) {
			return int64(n)
		}
		return value
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil && f == float64(int64(f)) {
			return int64(f)
		}
		return value
	default:
		return value
	}
}

func coerceNumber(value any) any {
	if s, ok := value.(string); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f
		}
	}
	return value
}

func coerceBoolean(value any) any {
	if s, ok := value.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true":
			return true
		case "false":
			return false
		}
	}
	return value
}

func coerceString(value any) any {
	switch v := value.(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return fmt.Sprintf("%t", v)
	default:
		return value
	}
}
