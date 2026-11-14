package ai

import (
	"strings"
	"testing"
)

func TestIssue45SafeValidationDiagnostics(t *testing.T) {
	tool := Tool{Name: "read", Parameters: Object(Prop("path", String()), Prop("limit", Integer()))}
	_, err := ValidateToolArguments(tool, ToolCall{Name: "read", Arguments: map[string]any{"path": "SECRET_CONTENT", "limit": map[string]any{"secret": "SECRET_VALUE"}}})
	if err == nil {
		t.Fatal("expected rejection")
	}
	msg := err.Error()

	for _, forbidden := range []string{"SECRET_CONTENT", "SECRET_VALUE", "Received arguments"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("leak %q: %s", forbidden, msg)
		}
	}
}

func TestIssue45UnknownToolDoesNotEchoUntrustedName(t *testing.T) {
	_, err := ValidateToolCall(nil, ToolCall{Name: "SECRET_TOKEN\nINJECT"})
	if err == nil || strings.Contains(err.Error(), "SECRET_TOKEN") || strings.Contains(err.Error(), "\n") {
		t.Fatalf("unsafe error: %v", err)
	}
}
