package providers

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// RED: the catalog declares this API, but importing providers must also
// register an adapter before either public SDK streaming entry point can run.
func TestCodexCatalogDispatchRegistersResponsesAdapterRED(t *testing.T) {
	model := ai.GetModel("openai-codex", "gpt-6-sol")
	if model == nil {
		t.Fatal("openai-codex/gpt-6-sol missing from embedded catalog")
	}
	if model.Api != ai.APIOpenAICodexResponses {
		t.Fatalf("catalog api = %q, want %q", model.Api, ai.APIOpenAICodexResponses)
	}
	adapter, ok := ai.GetApiProvider(model.Api)
	if !ok || adapter.Stream == nil || adapter.StreamSimple == nil {
		t.Fatalf("catalog api %q has no registered Codex Responses stream adapter (registered=%v, stream=%v, simple=%v)", model.Api, ok, adapter.Stream != nil, adapter.StreamSimple != nil)
	}
	// Public dispatch, not a direct invocation of an OpenAI Responses helper.
	// Deliberately no credential or live network: a canceled context must abort.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	final := ai.StreamSimple(ctx, model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, nil).Result()
	if final.StopReason != ai.StopAborted || strings.Contains(final.ErrorMessage, "No API provider registered") {
		t.Fatalf("public Codex dispatch returned %s: %s", final.StopReason, final.ErrorMessage)
	}
}
