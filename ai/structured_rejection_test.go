package ai

import (
	"errors"
	"strings"
	"testing"
)

func TestStructuredToolRejection(t *testing.T) {
	tool := Tool{Name: "read", Parameters: Object(Prop("path", String()), Opt("limit", Integer()))}
	for _, tc := range []struct {
		args                  map[string]any
		kind, field, expected string
	}{
		{map[string]any{"limit": 1}, "missing_required", "path", ""},
		{map[string]any{"path": "SECRET_CONTENT", "limit": map[string]any{"SECRET_KEY": "SECRET_VALUE"}}, "invalid_type", "limit", "integer"},
	} {
		_, err := ValidateToolArguments(tool, ToolCall{Arguments: tc.args})
		var rejection *ToolRejection
		if !errors.As(err, &rejection) {
			t.Fatalf("expected structured rejection, got %v", err)
		}
		if rejection.Kind != tc.kind || rejection.Field != tc.field || rejection.Expected != tc.expected {
			t.Fatalf("unexpected rejection: %+v", rejection)
		}
		for _, s := range []string{"SECRET_CONTENT", "SECRET_KEY", "SECRET_VALUE"} {
			if strings.Contains(err.Error(), s) {
				t.Fatalf("leak: %s", err)
			}
		}
	}
}

func TestStructuredRejectionDoesNotReflectUntrustedSchema(t *testing.T) {
	tool := Tool{Name: "SECRET_TOOL", Parameters: Object(Prop("SECRET_FIELD", String()))}
	_, err := ValidateToolArguments(tool, ToolCall{})
	var rejection *ToolRejection
	if !errors.As(err, &rejection) || strings.Contains(err.Error(), "SECRET") || rejection.Field != "" {
		t.Fatalf("unsafe rejection: %+v %v", rejection, err)
	}
	_, err = ValidateToolCall(nil, ToolCall{Name: "SECRET_TOOL"})
	if !errors.As(err, &rejection) || rejection.Kind != "unavailable" || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unsafe unavailable: %v", err)
	}
}

func TestStructuredRejectionFallbackDoesNotLeak(t *testing.T) {
	no := false
	tool := Tool{Parameters: &Schema{Type: "object", AdditionalAllowed: &no}}
	_, err := ValidateToolArguments(tool, ToolCall{Arguments: map[string]any{"SECRET_KEY": "SECRET_VALUE"}})
	var r *ToolRejection
	if !errors.As(err, &r) || r.Kind != "invalid_arguments" || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unsafe fallback: %v", err)
	}
}

func TestStructuredRejectionNullableFields(t *testing.T) {
	nullableString := &Schema{Type: "string", Nullable: true}
	nullableInteger := &Schema{Type: "integer", Nullable: true}
	tool := Tool{Parameters: Object(Prop("path", nullableString), Prop("limit", nullableInteger))}
	for _, tc := range []struct {
		name            string
		args            map[string]any
		field, expected string
	}{
		{"null path and invalid nullable limit", map[string]any{"path": nil, "limit": map[string]any{}}, "limit", "integer or null"},
		{"null limit and invalid nullable path", map[string]any{"path": []any{}, "limit": nil}, "path", "string or null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateToolArguments(tool, ToolCall{Arguments: tc.args})
			var r *ToolRejection
			if !errors.As(err, &r) || r.Kind != "invalid_type" || r.Field != tc.field || r.Expected != tc.expected {
				t.Fatalf("unexpected rejection: %+v (%v)", r, err)
			}
		})
	}
}

func TestStructuredRejectionSkipsValidNullableBeforeInvalidField(t *testing.T) {
	tool := Tool{Parameters: Object(Prop("limit", &Schema{Type: "integer", Nullable: true}), Prop("path", String()))}
	_, err := ValidateToolArguments(tool, ToolCall{Arguments: map[string]any{"limit": nil, "path": []any{}}})
	var r *ToolRejection
	if !errors.As(err, &r) || r.Kind != "invalid_type" || r.Field != "path" || r.Expected != "string" {
		t.Fatalf("unexpected rejection: %+v (%v)", r, err)
	}
	if _, err := ValidateToolArguments(tool, ToolCall{Arguments: map[string]any{"limit": nil, "path": "ok"}}); err != nil {
		t.Fatalf("valid nullable argument rejected: %v", err)
	}
}
