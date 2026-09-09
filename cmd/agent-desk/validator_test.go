package main

import "testing"

func TestValidateBasic(t *testing.T) {
	sch := Schema{
		"type":                 "object",
		"required":             []any{"message"},
		"additionalProperties": false,
		"properties": map[string]any{
			"message": Schema{"type": "string", "minLength": 1.0},
			"count":   Schema{"type": "integer"},
		},
	}
	if got := Validate(sch, map[string]any{"message": "hi", "count": 3}); len(got) != 0 {
		t.Fatalf("expected valid, got %v", got)
	}
	if got := Validate(sch, map[string]any{"message": 42}); len(got) == 0 {
		t.Fatal("expected type error for non-string message")
	}
	if got := Validate(sch, map[string]any{}); len(got) == 0 {
		t.Fatal("expected missing required error")
	}
	if got := Validate(sch, map[string]any{"message": "hi", "nope": true}); len(got) == 0 {
		t.Fatal("expected additional property error")
	}
}

func TestValidateNested(t *testing.T) {
	sch := Schema{
		"type": "object",
		"properties": map[string]any{
			"items": Schema{
				"type":     "array",
				"minItems": 1.0,
				"items": Schema{
					"type": "string",
					"enum": []any{"a", "b"},
				},
			},
		},
	}
	if got := Validate(sch, map[string]any{"items": []any{"a", "b"}}); len(got) != 0 {
		t.Fatalf("expected valid, got %v", got)
	}
	if got := Validate(sch, map[string]any{"items": []any{"c"}}); len(got) == 0 {
		t.Fatal("expected enum error")
	}
	if got := Validate(sch, map[string]any{"items": []any{}}); len(got) == 0 {
		t.Fatal("expected minItems error")
	}
}

func TestValidateAnyOf(t *testing.T) {
	sch := Schema{
		"anyOf": []any{
			Schema{"type": "string"},
			Schema{"type": "integer"},
		},
	}
	if got := Validate(sch, "hi"); len(got) != 0 {
		t.Fatalf("expected string branch to match, got %v", got)
	}
	if got := Validate(sch, 3.0); len(got) != 0 {
		t.Fatalf("expected integer branch to match, got %v", got)
	}
	if got := Validate(sch, true); len(got) == 0 {
		t.Fatal("expected boolean to match neither branch")
	}
}

func TestValidateOneOf(t *testing.T) {
	// Overlapping branches (any string, or strings at least 3 long) so a
	// long string matches both — oneOf must reject that as ambiguous even
	// though anyOf would accept it.
	sch := Schema{
		"oneOf": []any{
			Schema{"type": "string"},
			Schema{"type": "string", "minLength": 3.0},
		},
	}
	if got := Validate(sch, "hi"); len(got) != 0 {
		t.Fatalf("expected exactly one branch (plain string) to match, got %v", got)
	}
	if got := Validate(sch, "hello"); len(got) == 0 {
		t.Fatal("expected ambiguous match (both branches) to be rejected")
	}
	if got := Validate(sch, 42.0); len(got) == 0 {
		t.Fatal("expected number to match neither branch")
	}
}

func TestEmptySchemaAcceptsAnything(t *testing.T) {
	if got := Validate(Schema{}, "whatever"); len(got) != 0 {
		t.Fatalf("empty schema should accept anything, got %v", got)
	}
}
