package main

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
)

// Schema is the subset of JSON Schema that TCS files may use.
// Supported keywords: type, required, properties, additionalProperties,
// items, minItems/maxItems, enum, minLength/maxLength, minimum/maximum,
// oneOf/anyOf. Deliberately not supported: pattern, format, $ref, allOf,
// const — pattern/format especially, on purpose: a regex validator in the
// gate is complexity (escaping, ReDoS exposure) a tool's own runtime
// already handles better downstream. oneOf/anyOf are structural (which
// shape is this?), not content-level, so they stay in the gate.
type Schema map[string]any

// Validate returns a list of violations for value against schema.
// An empty schema accepts anything.
func Validate(s Schema, v any) []string {
	if len(s) == 0 {
		return nil
	}
	return validateValue(s, v, "$")
}

// validateValue checks v against s. oneOf/anyOf, when present, are the
// entire check for this node — they don't compose with a sibling "type"
// keyword. That's a deliberate simplification of full JSON Schema: the
// real use case is "this field is either a string or a number", which is
// naturally a oneOf of two type-only schemas, not a schema that mixes its
// own type constraint with a oneOf branch list.
func validateValue(s Schema, v any, path string) []string {
	if raw, ok := s["oneOf"]; ok {
		return validateOneOf(raw, v, path)
	}
	if raw, ok := s["anyOf"]; ok {
		return validateAnyOf(raw, v, path)
	}

	var errs []string
	typ, _ := s["type"].(string)

	switch typ {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: expected object, got %s", path, typeName(v))}
		}
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if rs, ok := r.(string); ok {
					if _, has := m[rs]; !has {
						errs = append(errs, fmt.Sprintf("%s: missing required property %q", path, rs))
					}
				}
			}
		}
		if props, ok := asSchema(s["properties"]); ok {
			for k, raw := range props {
				if pv, has := m[k]; has {
					if ps, ok := asSchema(raw); ok {
						errs = append(errs, validateValue(ps, pv, path+"."+k)...)
					}
				}
			}
		}
		if extra, ok := s["additionalProperties"].(bool); ok && !extra {
			for k := range m {
				if !propsContains(s, k) {
					errs = append(errs, fmt.Sprintf("%s: unexpected property %q is not allowed by the contract", path, k))
				}
			}
		}
	case "array":
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
			return []string{fmt.Sprintf("%s: expected array, got %s", path, typeName(v))}
		}
		if items, ok := asSchema(s["items"]); ok {
			for i := 0; i < rv.Len(); i++ {
				errs = append(errs, validateValue(items, rv.Index(i).Interface(), fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
		if mn, ok := numOf(s["minItems"]); ok && rv.Len() < int(mn) {
			errs = append(errs, fmt.Sprintf("%s: expected at least %d items, got %d", path, int(mn), rv.Len()))
		}
		if mx, ok := numOf(s["maxItems"]); ok && rv.Len() > int(mx) {
			errs = append(errs, fmt.Sprintf("%s: expected at most %d items, got %d", path, int(mx), rv.Len()))
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: expected string, got %s", path, typeName(v)))
			break
		}
		if enum, ok := s["enum"].([]any); ok {
			found := false
			for _, e := range enum {
				if e == str {
					found = true
					break
				}
			}
			if !found {
				errs = append(errs, fmt.Sprintf("%s: %q is not one of the allowed values %v", path, str, enum))
			}
		}
		if mn, ok := numOf(s["minLength"]); ok && len(str) < int(mn) {
			errs = append(errs, fmt.Sprintf("%s: string shorter than minLength %d", path, int(mn)))
		}
		if mx, ok := numOf(s["maxLength"]); ok && len(str) > int(mx) {
			errs = append(errs, fmt.Sprintf("%s: string longer than maxLength %d", path, int(mx)))
		}
	case "integer":
		f, ok := toFloat(v)
		if !ok || math.Trunc(f) != f {
			errs = append(errs, fmt.Sprintf("%s: expected integer, got %s", path, typeName(v)))
			break
		}
		errs = append(errs, checkBounds(s, path, f)...)
	case "number":
		f, ok := toFloat(v)
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: expected number, got %s", path, typeName(v)))
			break
		}
		errs = append(errs, checkBounds(s, path, f)...)
	case "boolean":
		if _, ok := v.(bool); !ok {
			errs = append(errs, fmt.Sprintf("%s: expected boolean, got %s", path, typeName(v)))
		}
	case "null":
		if v != nil {
			errs = append(errs, fmt.Sprintf("%s: expected null, got %s", path, typeName(v)))
		}
	}
	return errs
}

// validateAnyOf requires v to satisfy at least one of the listed schemas.
func validateAnyOf(raw any, v any, path string) []string {
	branches, ok := raw.([]any)
	if !ok || len(branches) == 0 {
		return []string{fmt.Sprintf("%s: anyOf must be a non-empty list of schemas", path)}
	}
	for _, b := range branches {
		bs, ok := asSchema(b)
		if !ok {
			continue
		}
		if len(validateValue(bs, v, path)) == 0 {
			return nil
		}
	}
	return []string{fmt.Sprintf("%s: value does not match any of the %d allowed shapes", path, len(branches))}
}

// validateOneOf requires v to satisfy exactly one of the listed schemas —
// real JSON Schema oneOf semantics, not "at least one" (that's anyOf).
func validateOneOf(raw any, v any, path string) []string {
	branches, ok := raw.([]any)
	if !ok || len(branches) == 0 {
		return []string{fmt.Sprintf("%s: oneOf must be a non-empty list of schemas", path)}
	}
	matches := 0
	for _, b := range branches {
		bs, ok := asSchema(b)
		if !ok {
			continue
		}
		if len(validateValue(bs, v, path)) == 0 {
			matches++
		}
	}
	switch {
	case matches == 0:
		return []string{fmt.Sprintf("%s: value does not match any of the %d allowed shapes", path, len(branches))}
	case matches > 1:
		return []string{fmt.Sprintf("%s: value matches %d of the %d allowed shapes, oneOf requires exactly one", path, matches, len(branches))}
	default:
		return nil
	}
}

func checkBounds(s Schema, path string, f float64) []string {
	var errs []string
	if mn, ok := numOf(s["minimum"]); ok && f < mn {
		errs = append(errs, fmt.Sprintf("%s: %v is below minimum %v", path, f, mn))
	}
	if mx, ok := numOf(s["maximum"]); ok && f > mx {
		errs = append(errs, fmt.Sprintf("%s: %v is above maximum %v", path, f, mx))
	}
	return errs
}

func propsContains(s Schema, key string) bool {
	props, ok := asSchema(s["properties"])
	if !ok {
		return false
	}
	_, has := props[key]
	return has
}

// asSchema normalizes either a Schema or a plain map[string]any (yaml.v3
// decodes nested mappings either way depending on context) into a Schema.
func asSchema(v any) (Schema, bool) {
	switch vv := v.(type) {
	case Schema:
		return vv, true
	case map[string]any:
		return Schema(vv), true
	}
	return nil, false
}

func numOf(v any) (float64, bool) { return toFloat(v) }

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func typeName(v any) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, int, int64, json.Number:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}
