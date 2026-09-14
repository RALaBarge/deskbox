package tcs

import (
	"fmt"
	"sort"
)

// unsupported maps a JSON Schema keyword the TCS subset deliberately does not
// implement to the reason, so a contract that uses one is told why the gate
// won't honor it rather than having it silently ignored at validation time.
// A keyword the validator ignores is worse than one it rejects: the author
// believes a constraint is being enforced when nothing enforces it.
var unsupported = map[string]string{
	"pattern": "regex matching is left to the tool's own runtime (escaping + ReDoS belong downstream)",
	"format":  "format assertions are not enforced by the gate; validate in the tool",
	"$ref":    "references are not resolved by the gate; inline the schema",
	"allOf":   "not supported; express the combined shape directly, or use oneOf/anyOf",
	"const":   "not supported; use enum with a single value",
}

// knownKeywords is every keyword validateValue actually reads. A keyword
// outside this set (and outside unsupported) is a typo or a
// misunderstanding — flagged so "maxLenght" doesn't silently do nothing.
var knownKeywords = map[string]bool{
	"type": true, "required": true, "properties": true, "additionalProperties": true,
	"items": true, "minItems": true, "maxItems": true, "enum": true,
	"minLength": true, "maxLength": true, "minimum": true, "maximum": true,
	"oneOf": true, "anyOf": true, "summary": true, "description": true, "title": true,
}

var knownTypes = map[string]bool{
	"object": true, "array": true, "string": true, "integer": true,
	"number": true, "boolean": true, "null": true,
}

// Lint walks a schema and reports everything the gate cannot honor: keywords
// the subset rejects on purpose, unrecognized keywords (typos), and malformed
// structure (a non-list oneOf, an unknown type name). It is exactly the set
// of problems that would otherwise surface as "the contract validated fine
// but didn't constrain what I thought" — caught offline, before the desk
// ever loads the tool. An empty schema is valid (accepts anything).
func Lint(s Schema) []string {
	var errs []string
	lintSchema(s, "$", &errs)
	sort.Strings(errs)
	return errs
}

func lintSchema(s Schema, path string, errs *[]string) {
	for k := range s {
		if reason, no := unsupported[k]; no {
			*errs = append(*errs, fmt.Sprintf("%s: %q is not supported by the TCS schema subset — %s", path, k, reason))
			continue
		}
		if !knownKeywords[k] {
			*errs = append(*errs, fmt.Sprintf("%s: unknown keyword %q (typo? it will be silently ignored by the validator)", path, k))
		}
	}

	if raw, ok := s["type"]; ok {
		if t, ok := raw.(string); !ok {
			*errs = append(*errs, fmt.Sprintf("%s: type must be a string", path))
		} else if !knownTypes[t] {
			*errs = append(*errs, fmt.Sprintf("%s: unknown type %q", path, t))
		}
	}

	if props, ok := asSchema(s["properties"]); ok {
		for k, raw := range props {
			if ps, ok := asSchema(raw); ok {
				lintSchema(ps, path+"."+k, errs)
			} else {
				*errs = append(*errs, fmt.Sprintf("%s.%s: property schema must be an object", path, k))
			}
		}
	}
	if items, ok := asSchema(s["items"]); ok {
		lintSchema(items, path+"[]", errs)
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		if raw, ok := s[key]; ok {
			branches, ok := raw.([]any)
			if !ok || len(branches) == 0 {
				*errs = append(*errs, fmt.Sprintf("%s: %s must be a non-empty list of schemas", path, key))
				continue
			}
			for i, b := range branches {
				if bs, ok := asSchema(b); ok {
					lintSchema(bs, fmt.Sprintf("%s/%s[%d]", path, key, i), errs)
				} else {
					*errs = append(*errs, fmt.Sprintf("%s/%s[%d]: branch must be a schema object", path, key, i))
				}
			}
		}
	}
}
