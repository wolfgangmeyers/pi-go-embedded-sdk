package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sky-valley/pi/ai"
)

var errCodexStreamUnauthorized = errors.New("Codex response failed: 401 unauthorized after stream start; turn outcome unknown")

// codexWireToolNames changes only names on the outbound Codex wire. Run this
// after OnPayload: hooks must never get an alias or invalidate a prebuilt map.
// The inverse map is request-local and only admits names actually declared.
func codexWireToolNames(payload map[string]any, original, resolved []ai.Message) (map[string]string, error) {
	declared := map[string]bool{}
	for _, messages := range [][]ai.Message{original, resolved} {
		for _, tool := range ai.GetDeclaredTools(messages) {
			declared[tool.Name] = true
		}
	}
	alias := ""
	if declared["mecha_worker.launch"] {
		for i := 0; i < 32; i++ {
			candidate := "mecha_worker_launch"
			if i != 0 {
				candidate = fmt.Sprintf("mecha_worker_launch_%d", i)
			}
			if !declared[candidate] {
				alias = candidate
				break
			}
		}
		if alias == "" {
			return nil, errors.New("Codex tool alias space exhausted")
		}
	}
	wire := map[string]string{}
	seen := map[string]bool{}
	rewrite := func(name string, definition bool) (string, error) {
		if !declared[name] || name == "" {
			return "", errors.New("Codex unexpected tool name")
		}
		if definition {
			if seen[name] {
				return "", errors.New("Codex duplicate tool declaration")
			}
			seen[name] = true
		}
		if name == "mecha_worker.launch" && alias != "" {
			return alias, nil
		}
		return name, nil
	}
	// Top-level and message-anchored definitions are separate wire locations.
	visitTools := func(value any) error {
		var entries []any
		switch v := value.(type) {
		case []any:
			entries = v
		case []map[string]any:
			for _, item := range v {
				entries = append(entries, item)
			}
		default:
			return errors.New("Codex malformed tool declarations")
		}
		for _, entry := range entries {
			tool, ok := entry.(map[string]any)
			if !ok {
				return errors.New("Codex malformed tool declaration")
			}
			name, ok := tool["name"].(string)
			if !ok {
				return errors.New("Codex malformed tool name")
			}
			wireName, err := rewrite(name, true)
			if err != nil {
				return err
			}
			tool["name"] = wireName
			wire[wireName] = name
		}
		return nil
	}
	if tools, ok := payload["tools"]; ok {
		if err := visitTools(tools); err != nil {
			return nil, err
		}
	}
	if input, ok := payload["input"]; ok {
		items, ok := input.([]any)
		if !ok {
			return nil, errors.New("Codex malformed input")
		}
		for _, entry := range items {
			item, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			switch item["type"] {
			case "additional_tools", "tool_search_output":
				if err := visitTools(item["tools"]); err != nil {
					return nil, err
				}
			case "function_call", "custom_tool_call":
				name, ok := item["name"].(string)
				if !ok {
					return nil, errors.New("Codex malformed historical tool call")
				}
				wireName, err := rewrite(name, false)
				if err != nil {
					return nil, err
				}
				item["name"] = wireName
			}
		}
	}
	if choice, ok := payload["tool_choice"].(string); ok {
		if choice != "auto" && choice != "none" && choice != "required" {
			wireName, err := rewrite(choice, false)
			if err != nil {
				return nil, err
			}
			payload["tool_choice"] = wireName
		}
	} else if choice, ok := payload["tool_choice"].(map[string]any); ok {
		if choice["type"] == "function" || choice["type"] == "custom" {
			name, ok := choice["name"].(string)
			if !ok {
				return nil, errors.New("Codex malformed tool choice")
			}
			wireName, err := rewrite(name, false)
			if err != nil {
				return nil, err
			}
			choice["name"] = wireName
		}
	}
	return wire, nil
}

// RegisterOpenAICodexResponses installs Codex's own transport. It deliberately
// does not route through the generic OpenAI Responses client: Codex has a
// different endpoint and must only accept router-supplied request OAuth auth.
func RegisterOpenAICodexResponses() {
	ai.RegisterApiProvider(ai.ApiProvider{Api: ai.APIOpenAICodexResponses,
		Stream: streamCodexResponses,
		StreamSimple: func(ctx context.Context, m *ai.Model, req ai.TranscriptContext, o *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			opts := &ai.StreamOptions{}
			if o != nil {
				*opts = o.StreamOptions
			}
			choice := ai.ToolChoiceAuto
			reasoning := ai.ThinkingLevel("")
			if o != nil {
				reasoning = o.Reasoning
				if o.ToolChoice != "" {
					choice = o.ToolChoice
				}
			}
			return streamCodexResponsesWithToolChoice(ctx, m, req, opts, choice, reasoning)
		},
	})
}

func streamCodexResponses(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.StreamOptions) *ai.AssistantMessageEventStream {
	return streamCodexResponsesWithToolChoice(ctx, model, req, opts, ai.ToolChoiceAuto, "")
}

func streamCodexResponsesWithToolChoice(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.StreamOptions, choice ai.ToolChoice, reasoning ai.ThinkingLevel) *ai.AssistantMessageEventStream {
	stream := ai.NewAssistantMessageEventStream()
	if opts == nil {
		opts = &ai.StreamOptions{}
	}
	go func() {
		output := &ai.AssistantMessage{Api: model.Api, Provider: model.Provider, Model: model.ID, Content: ai.ContentList{}, StopReason: ai.StopPending, Timestamp: time.Now().UnixMilli()}
		// Only fixed adapter messages cross this boundary. Provider bodies,
		// transport errors, hooks and URL errors can contain credential text.
		failureKind := ai.FailureUnknown
		fail := func(message string) {
			if ctx.Err() != nil {
				output.StopReason = ai.StopAborted
			} else {
				output.StopReason = ai.StopError
			}
			output.ErrorMessage = message
			stream.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: output.StopReason, Error: output, FailureKind: failureKind})
			stream.End()
		}
		if ctx.Err() != nil {
			fail("Codex request aborted")
			return
		}
		if model.Api != ai.APIOpenAICodexResponses || model.Provider != "openai-codex" {
			fail("Codex Responses requires an openai-codex model")
			return
		}
		if opts.CodexAuth == nil {
			fail("Codex OAuth token source is required")
			return
		}
		auth, err := opts.CodexAuth(ctx)
		if err != nil {
			fail("Codex auth unavailable")
			return
		}
		if auth.APIKey == "" {
			fail("Codex OAuth token source returned no access token")
			return
		}
		// Only the router's exact account header is accepted. Do not let an
		// auth callback alter content negotiation, identity, or credentials.
		if len(auth.Headers) != 1 {
			fail("Codex auth headers invalid")
			return
		}
		account, ok := auth.Headers["ChatGPT-Account-ID"]
		if !ok || account == nil || strings.TrimSpace(*account) == "" || strings.ContainsAny(*account, "\r\n") {
			fail("Codex account ID missing or invalid")
			return
		}
		resolved := ai.ResolveTranscript(req, getResponsesCompat(model).SupportsMidConvoSystemMessages)
		input, err := responsesInput(model, resolved)
		if err != nil {
			fail("Codex request input invalid")
			return
		}
		// Codex carries only the leading system prompt in instructions. Keep
		// later system updates in input, including on tool continuations.
		instructions := ""
		if len(resolved.Messages) > 0 {
			if system, ok := asSystemMsg(resolved.Messages[0]); ok {
				instructions = ai.GetSystemMessageText(system)
				if instructions != "" {
					// responsesInput emits this nonempty leading prompt first.
					input = input[1:]
				}
			}
		}
		// Pi Codex SSE buildRequestBody (0.87.1): the instruction is always
		// nonempty, even when there is no leading system message.
		if instructions == "" {
			instructions = "You are a helpful assistant."
		}
		params := map[string]any{
			"model": model.ID, "instructions": instructions, "input": input,
			"stream": true, "store": false, "text": map[string]any{"verbosity": "low"},
			"include":     []string{"reasoning.encrypted_content"},
			"tool_choice": choice, "parallel_tool_calls": true,
		}
		// Pi Codex 0.87.1 sends an off mapping without a requested level,
		// but only requests a summary for an explicit non-off effort.
		level := ai.ModelThinkingLevel("off")
		if reasoning != "" && reasoning != "off" {
			level = ai.ClampThinkingLevel(model, ai.ModelThinkingLevel(reasoning))
		}
		if mapped, present := model.ThinkingLevelMap[level]; present && mapped != nil {
			if level == "off" {
				params["reasoning"] = map[string]any{"effort": *mapped}
			} else {
				params["reasoning"] = map[string]any{"effort": *mapped, "summary": "auto"}
			}
		} else if level != "off" {
			params["reasoning"] = map[string]any{"effort": string(level), "summary": "auto"}
		}
		if opts.Temperature != nil {
			params["temperature"] = *opts.Temperature
		}
		compat := getResponsesCompat(model)
		// Codex defaults strict support to true, unlike the generic Responses
		// adapter. Honor an explicit model override without changing that adapter.
		compat.SupportsStrictMode = true
		applyCompat(newCompatOverrides(model.Compat), "supportsStrictMode", &compat.SupportsStrictMode)
		tools := ai.ResolveTranscriptTools(resolved.Messages, compat.SupportsAdditionalTools || compat.SupportsToolSearch).RequestTools
		if len(tools) > 0 {
			converted, e := convertResponsesTools(tools, compat, false)
			if e != nil {
				fail("Codex request tools invalid")
				return
			}
			// TS convertResponsesTools receives strict:null for Codex. Only
			// unconstrained functions need the null default; constrained JSON
			// schema tools retain their resolved true value, and custom grammar
			// tools have no strict field.
			if compat.SupportsStrictMode {
				for _, tool := range converted {
					if tool["type"] == "function" && tool["strict"] == false {
						tool["strict"] = nil
					}
				}
			}
			params["tools"] = converted
		}
		if opts.MaxTokens != nil && *opts.MaxTokens > 0 {
			params["max_output_tokens"] = *opts.MaxTokens
		}
		if opts.OnPayload != nil {
			next, e := opts.OnPayload(params, model)
			if e != nil {
				fail("Codex payload hook failed")
				return
			}
			if next != nil {
				replacement, ok := next.(map[string]any)
				if !ok {
					fail("Codex payload replacement must be an object")
					return
				}
				params = replacement
			}
		}
		wireNames, err := codexWireToolNames(params, req.Messages, resolved.Messages)
		if err != nil {
			fail("Codex payload tools invalid")
			return
		}
		payload, err := json.Marshal(params)
		if err != nil {
			fail("Codex payload encoding failed")
			return
		}
		// The model's base is the only endpoint authority. Router production
		// validation must pin it independently; standalone SDK models may use
		// other HTTPS endpoints, but auth cannot redirect the issued bearer.
		if auth.BaseURL != "" {
			fail("Codex auth base URL override unsupported")
			return
		}
		base := model.BaseURL
		parsed, parseErr := url.Parse(base)
		if parseErr != nil || parsed == nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || strings.ContainsAny(base, "?#") {
			fail("Codex base URL invalid or insecure")
			return
		}
		url, err := requestURL(sdkJoinURL(base, "/codex/responses"))
		if err != nil {
			fail("Codex request URL invalid")
			return
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			fail("Codex request creation failed")
			return
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("ChatGPT-Account-ID", *account)
		// These fixed Codex identity headers are set last, as in Pi's
		// buildBaseCodexHeaders/buildSSEHeaders; auth cannot override them.
		request.Header.Set("Originator", "pi")
		request.Header.Set("User-Agent", piUserAgent())
		request.Header.Set("OpenAI-Beta", "responses=experimental")
		request.Header.Set("Authorization", "Bearer "+auth.APIKey)
		request.Header.Set("Accept", "text/event-stream")
		client := opts.HTTPClient
		if client == nil {
			client = http.DefaultClient
		}
		// Never replay a Codex turn. Even an apparently pre-output HTTP error can
		// represent an accepted operation upstream; the router owns any recovery.
		resp, err := client.Do(request)
		if err != nil {
			fail("Codex request transport failed; turn outcome unknown")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if resp.StatusCode == http.StatusUnauthorized {
				if ctx.Err() == nil {
					failureKind = ai.FailureCodexPreOutputUnauthorized
				}
				fail("Codex Responses HTTP 401 unauthorized before output")
			} else {
				fail("Codex Responses HTTP request failed; turn outcome unknown")
			}
			return
		}
		if opts.OnResponse != nil {
			if err := opts.OnResponse(ai.ProviderResponse{Status: resp.StatusCode, Headers: flattenHeaders(resp.Header)}, model); err != nil {
				fail("Codex response hook failed; turn outcome unknown")
				return
			}
		}
		stream.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: output.Clone()})
		type slot struct {
			index   int
			kind    string
			text    strings.Builder
			args    strings.Builder
			pending []string
			started bool
			call    ai.ToolCall
		}
		slots := map[int]*slot{}
		// A parallel call can emit events while an earlier call has no name.
		// Its partial would expose that nameless call, so hold snapshots until
		// every live tool slot has a declared, canonical name.
		var held []ai.AssistantMessageEvent
		unresolved := func() bool {
			for _, s := range slots {
				if s.kind == "tool" && s.call.Name == "" {
					return true
				}
			}
			return false
		}
		flush := func() {
			if unresolved() {
				return
			}
			for _, queued := range held {
				for i, content := range queued.Partial.Content {
					if call, ok := content.(ai.ToolCall); ok && call.Name == "" {
						call.Name = output.Content[i].(ai.ToolCall).Name
						queued.Partial.Content[i] = call
					}
				}
				stream.Push(queued)
			}
			held = nil
		}
		push := func(event ai.AssistantMessageEvent) {
			if unresolved() {
				held = append(held, event)
				return
			}
			flush()
			stream.Push(event)
		}
		emit := func(kind ai.EventType, s *slot, delta string) {
			push(ai.AssistantMessageEvent{Type: kind, ContentIndex: s.index, Delta: delta, Partial: output.Clone()})
		}
		terminal := false
		err = iterateOpenAISSE2(resp.Body, ctx, func(data any) error {
			// The SSE iterator may continue past response.completed until DONE/EOF.
			// Reject later events before hooks or handlers can expose new output.
			if terminal {
				return errors.New("Codex event after terminal response")
			}
			if opts.OnProviderStreamEvent != nil {
				return opts.OnProviderStreamEvent(data, model)
			}
			return nil
		}, func(ev responsesEvent) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s := slots[ev.OutputIndex]
			switch ev.Type {
			case "response.created":
				output.ResponseID = jsStringField(jsGet(jsGet(ev.JS, "response"), "id"))
			case "response.output_item.added":
				if ev.Item == nil {
					return nil
				}
				if s != nil {
					return errors.New("Codex duplicate output index")
				}
				s = &slot{index: len(output.Content)}
				switch ev.Item.Type {
				case "message":
					s.kind = "text"
					output.Content = append(output.Content, ai.TextContent{})
					slots[ev.OutputIndex] = s
					emit(ai.EventTextStart, s, "")
				case "function_call":
					s.kind = "tool"
					s.call = ai.ToolCall{ID: ev.Item.CallID + "|" + ev.Item.ID}
					output.Content = append(output.Content, s.call)
					slots[ev.OutputIndex] = s
					if ev.Item.Name != "" {
						name, ok := wireNames[ev.Item.Name]
						if !ok {
							return errors.New("Codex undeclared tool call")
						}
						s.call.Name = name
						s.started = true
						output.Content[s.index] = s.call
						emit(ai.EventToolCallStart, s, "")
					}
				}
			case "response.output_text.delta":
				if s != nil && s.kind == "text" {
					s.text.WriteString(ev.Delta)
					output.Content[s.index] = ai.TextContent{Text: s.text.String()}
					emit(ai.EventTextDelta, s, ev.Delta)
				}
			case "response.function_call_arguments.delta":
				if s != nil && s.kind == "tool" {
					s.args.WriteString(ev.Delta)
					s.call.Arguments, _ = parseStreamingJSON(s.args.String())
					output.Content[s.index] = s.call
					if s.started {
						emit(ai.EventToolCallDelta, s, ev.Delta)
					} else {
						s.pending = append(s.pending, ev.Delta)
					}
				}
			case "response.function_call_arguments.done":
				if s != nil && s.kind == "tool" && ev.Arguments != "" {
					s.args.Reset()
					s.args.WriteString(ev.Arguments)
					s.call.Arguments, _ = parseStreamingJSON(ev.Arguments)
					output.Content[s.index] = s.call
				}
			case "response.output_item.done":
				if s == nil || ev.Item == nil {
					return nil
				}
				if s.kind == "text" {
					if len(ev.Item.Content) > 0 {
						s.text.Reset()
						for _, p := range ev.Item.Content {
							s.text.WriteString(p.Text)
						}
					}
					output.Content[s.index] = ai.TextContent{Text: s.text.String(), TextSignature: encodeTextSignatureV1(ev.Item.ID, ev.Item.Phase)}
					push(ai.AssistantMessageEvent{Type: ai.EventTextEnd, ContentIndex: s.index, Content: s.text.String(), Partial: output.Clone()})
				}
				if s.kind == "tool" {
					if ev.Item.Type != "function_call" {
						return errors.New("Codex tool item changed type")
					}
					if ev.Item.Name != "" {
						name, ok := wireNames[ev.Item.Name]
						if !ok || (s.started && s.call.Name != name) {
							return errors.New("Codex tool name changed or undeclared")
						}
						s.call.Name = name
					}
					if s.call.Name == "" {
						return errors.New("Codex tool call has no name")
					}
					if !s.started {
						s.started = true
						output.Content[s.index] = s.call
						emit(ai.EventToolCallStart, s, "")
						for _, delta := range s.pending {
							emit(ai.EventToolCallDelta, s, delta)
						}
					}
					raw := ev.Item.Arguments
					if raw == "" {
						raw = s.args.String()
					}
					s.call.Arguments, _ = parseStreamingJSON(orEmptyJSON(raw))
					output.Content[s.index] = s.call
					tc := s.call
					push(ai.AssistantMessageEvent{Type: ai.EventToolCallEnd, ContentIndex: s.index, ToolCall: &tc, Partial: output.Clone()})
				}
				delete(slots, ev.OutputIndex)
				// A done item can be the last unresolved slot; release other
				// calls' buffered events without exposing an empty-name partial.
				flush()
			case "response.completed":
				for _, pending := range slots {
					if pending.kind == "tool" {
						return errors.New("Codex unfinished tool call")
					}
				}
				terminal = true
				output.ResponseID = jsStringField(jsGet(jsGet(ev.JS, "response"), "id"))
				status := jsStringField(jsGet(jsGet(ev.JS, "response"), "status"))
				if status != "completed" {
					return fmt.Errorf("Codex response status %q", status)
				}
				output.StopReason = ai.StopStop
				for _, c := range output.Content {
					if _, ok := c.(ai.ToolCall); ok {
						output.StopReason = ai.StopToolUse
						break
					}
				}
			case "response.incomplete", "response.failed", "error":
				terminal = true
				failure := jsGet(jsGet(ev.JS, "response"), "error")
				otherFailure := jsGet(ev.JS, "error")
				if jsStringField(jsGet(failure, "code")) == "invalid_token" || jsTokenCount(jsGet(failure, "status")) == http.StatusUnauthorized || jsStringField(jsGet(otherFailure, "code")) == "invalid_token" || jsTokenCount(jsGet(otherFailure, "status")) == http.StatusUnauthorized {
					return errCodexStreamUnauthorized
				}
				return fmt.Errorf("Codex response failed: %s", ev.Type)
			}
			return nil
		})
		if err != nil {
			// An unresolved call is never a dispatchable error result.
			// Buffered events are dropped and the partial is not published.
			if unresolved() || len(held) > 0 {
				output.Content = nil
			}
			if errors.Is(err, errCodexStreamUnauthorized) {
				fail("Codex response failed: 401 unauthorized after stream start; turn outcome unknown")
			} else {
				fail("Codex stream failed; turn outcome unknown")
			}
			return
		}
		if ctx.Err() != nil {
			fail("Codex request aborted")
			return
		}
		if !terminal {
			if unresolved() || len(held) > 0 {
				output.Content = nil
			}
			fail("Codex stream ended before terminal response event; turn outcome unknown")
			return
		}
		stream.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: output.StopReason, Message: output})
		stream.End()
	}()
	return stream
}
