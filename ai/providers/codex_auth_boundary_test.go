package providers

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Every invalid auth/base variant must fail before the fake doer sees a request.
func TestCodexRejectsAuthHeaderInjectionBeforeDo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers ai.ProviderHeaders
	}{
		{"missing account", nil},
		{"nil account", ai.ProviderHeaders{"ChatGPT-Account-ID": nil}},
		{"empty account", ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("  ")}},
		{"alternate case account", ai.ProviderHeaders{"chatgpt-account-id": ai.HeaderValue("fixture-account")}},
		{"accept override", ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("fixture-account"), "Accept": ai.HeaderValue("application/json")}},
		{"content type override", ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("fixture-account"), "Content-Type": ai.HeaderValue("text/plain")}},
		{"authorization override", ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("fixture-account"), "Authorization": ai.HeaderValue("Bearer injected")}},
		{"beta override", ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("fixture-account"), "OpenAI-Beta": ai.HeaderValue("injected")}},
		{"originator override", ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("fixture-account"), "Originator": ai.HeaderValue("injected")}},
		{"user agent override", ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("fixture-account"), "User-Agent": ai.HeaderValue("injected")}},
		{"arbitrary header", ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("fixture-account"), "X-Forwarded-Host": ai.HeaderValue("injected")}},
		{"account newline", ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("fixture\r\nInjected: yes")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := codexFixtureOptions()
			opts.CodexAuth = func(context.Context) (ai.ModelAuth, error) {
				return ai.ModelAuth{APIKey: "fake-token", Headers: tc.headers}, nil
			}
			opts.HTTPClient = ai.HTTPDoer(codexFakeDoer(func(*http.Request) (*http.Response, error) {
				t.Error("invalid Codex auth reached HTTP doer")
				return nil, nil
			}))
			result := ai.StreamSimple(context.Background(), codexFixtureModel("https://fixture.invalid"), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, opts).Result()
			if result == nil || result.StopReason != ai.StopError || strings.Contains(result.ErrorMessage, "fake-token") {
				t.Fatal("invalid auth did not fail closed and redact")
			}
		})
	}
}

func TestCodexRejectsUnsafeBaseAndAuthOverrideBeforeDo(t *testing.T) {
	for _, tc := range []struct {
		name, base, authBase string
	}{
		{"auth override", "https://fixture.invalid/backend-api", "https://other.invalid/backend-api"},
		{"equal auth override unsupported", "https://fixture.invalid/backend-api", "https://fixture.invalid/backend-api"},
		{"http loopback", "http://127.0.0.1:1", ""},
		{"http remote", "http://fixture.invalid", ""},
		{"userinfo", "https://user:pass@fixture.invalid", ""},
		{"query", "https://fixture.invalid/backend-api?route=other", ""},
		{"empty query", "https://fixture.invalid/backend-api?", ""},
		{"fragment", "https://fixture.invalid/backend-api#fragment", ""},
		{"empty fragment", "https://fixture.invalid/backend-api#", ""},
		{"missing host", "https:///backend-api", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := codexFixtureOptions()
			opts.CodexAuth = func(context.Context) (ai.ModelAuth, error) {
				return ai.ModelAuth{APIKey: "fake-token", BaseURL: tc.authBase, Headers: ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("fixture-account")}}, nil
			}
			opts.HTTPClient = ai.HTTPDoer(codexFakeDoer(func(*http.Request) (*http.Response, error) {
				t.Error("unsafe Codex endpoint reached HTTP doer")
				return nil, nil
			}))
			result := ai.StreamSimple(context.Background(), codexFixtureModel(tc.base), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, opts).Result()
			if result == nil || result.StopReason != ai.StopError || strings.Contains(result.ErrorMessage, "fake-token") {
				t.Fatal("unsafe endpoint did not fail closed and redact")
			}
		})
	}
}
