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

func TestEmptySchemaAcceptsAnything(t *testing.T) {
	if got := Validate(Schema{}, "whatever"); len(got) != 0 {
		t.Fatalf("empty schema should accept anything, got %v", got)
	}
}