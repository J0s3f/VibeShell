package opencode

import (
	"encoding/json"
	"fmt"

	"j0s.at/vibeshell/internal/domain"
)

// encodeRequest renders one canonical domain.ModelRequest into the wire
// body for the route's protocol. The model ID comes from the route config
// (exact upstream ID, never rewritten). A negative MaxTokens is rejected;
// zero omits the limit and leaves server-side defaulting in place.
func encodeRequest(route RouteConfig, req domain.ModelRequest) ([]byte, error) {
	if route.Model == "" {
		return nil, fmt.Errorf("opencode: route has no model ID")
	}
	if req.MaxTokens < 0 {
		return nil, fmt.Errorf("opencode: max_tokens must not be negative")
	}
	switch route.Protocol {
	case ProtocolChat:
		return encodeChat(route.Model, req)
	case ProtocolResponses:
		return encodeResponses(route.Model, req)
	case ProtocolMessages:
		return encodeAnthropic(route.Model, req)
	case ProtocolGemini:
		return encodeGemini(route.Model, req)
	default:
		return nil, fmt.Errorf("opencode: unknown protocol %q; refusing chat default", string(route.Protocol))
	}
}

// roleString passes the canonical role through; the domain allows only
// system/user/assistant/tool, which every family names the same.
func roleString(r domain.MessageRole) string {
	return string(r)
}

func rawArgs(a json.RawMessage) string {
	if len(a) == 0 {
		return "{}"
	}
	return string(a)
}

func rawSchema(p json.RawMessage) any {
	if len(p) == 0 {
		return map[string]any{"type": "object"}
	}
	var v any
	if err := json.Unmarshal(p, &v); err != nil {
		return map[string]any{"type": "object"}
	}
	return v
}

// ---------------------------------------------------------------------------
// Chat Completions
// ---------------------------------------------------------------------------

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func encodeChat(model string, req domain.ModelRequest) ([]byte, error) {
	messages := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		item := map[string]any{"role": roleString(m.Role)}
		if m.Content != "" {
			item["content"] = m.Content
		} else {
			item["content"] = nil
		}
		if m.Name != "" {
			item["name"] = m.Name
		}
		if m.ToolCallID != "" {
			item["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]chatToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				c := chatToolCall{ID: tc.ID, Type: "function"}
				c.Function.Name = tc.Name
				c.Function.Arguments = rawArgs(tc.Arguments)
				calls = append(calls, c)
			}
			item["tool_calls"] = calls
		}
		messages = append(messages, item)
	}
	body := map[string]any{
		"model":          model,
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  rawSchema(t.Parameters),
				},
			})
		}
		body["tools"] = tools
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}
	return json.Marshal(body)
}

// ---------------------------------------------------------------------------
// Responses API
// ---------------------------------------------------------------------------

// encodeResponses maps canonical history onto Responses input items.
// Assistant tool calls become function_call items so a multi-turn tool
// loop survives a model switch; tool results become function_call_output
// items keyed by call ID. Live acceptance of this shape is UNVERIFIED.
func encodeResponses(model string, req domain.ModelRequest) ([]byte, error) {
	input := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch {
		case m.Role == domain.RoleTool:
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  m.Content,
			})
		case m.Role == domain.RoleAssistant && len(m.ToolCalls) > 0:
			if m.Content != "" {
				input = append(input, map[string]any{"role": "assistant", "content": m.Content})
			}
			for _, tc := range m.ToolCalls {
				input = append(input, map[string]any{
					"type":      "function_call",
					"id":        tc.ID,
					"name":      tc.Name,
					"arguments": rawArgs(tc.Arguments),
				})
			}
		default:
			input = append(input, map[string]any{"role": roleString(m.Role), "content": m.Content})
		}
	}
	body := map[string]any{
		"model":  model,
		"input":  input,
		"stream": true,
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tool := map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
				"parameters":  rawSchema(t.Parameters),
			}
			if t.Strict {
				tool["strict"] = true
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
	}
	if req.MaxTokens > 0 {
		body["max_output_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	return json.Marshal(body)
}

// ---------------------------------------------------------------------------
// Anthropic Messages
// ---------------------------------------------------------------------------

// encodeAnthropic maps canonical history onto Messages: system messages
// concatenate into top-level system, tool results become tool_result
// blocks inside a user message. Live acceptance is UNVERIFIED.
func encodeAnthropic(model string, req domain.ModelRequest) ([]byte, error) {
	var system []string
	messages := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case domain.RoleSystem:
			system = append(system, m.Content)
		case domain.RoleTool:
			messages = append(messages, map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type":        "tool_result",
					"tool_use_id": m.ToolCallID,
					"content":     m.Content,
				}},
			})
		case domain.RoleAssistant:
			if len(m.ToolCalls) > 0 {
				blocks := make([]any, 0, len(m.ToolCalls)+1)
				if m.Content != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
				}
				for _, tc := range m.ToolCalls {
					var input any = map[string]any{}
					if len(tc.Arguments) > 0 {
						var v any
						if err := json.Unmarshal(tc.Arguments, &v); err == nil {
							input = v
						}
					}
					blocks = append(blocks, map[string]any{
						"type":  "tool_use",
						"id":    tc.ID,
						"name":  tc.Name,
						"input": input,
					})
				}
				messages = append(messages, map[string]any{"role": "assistant", "content": blocks})
			} else {
				messages = append(messages, map[string]any{"role": "assistant", "content": m.Content})
			}
		default:
			messages = append(messages, map[string]any{"role": roleString(m.Role), "content": m.Content})
		}
	}
	body := map[string]any{
		"model":      model,
		"messages":   messages,
		"stream":     true,
		"max_tokens": 1024,
	}
	if len(system) > 0 {
		joined := ""
		for i, s := range system {
			if i > 0 {
				joined += "\n\n"
			}
			joined += s
		}
		body["system"] = joined
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": rawSchema(t.Parameters),
			})
		}
		body["tools"] = tools
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	return json.Marshal(body)
}

// ---------------------------------------------------------------------------
// Gemini generateContent
// ---------------------------------------------------------------------------

// encodeGemini maps canonical history onto generateContent: system
// messages become systemInstruction, roles map user<->user and
// assistant/system-output<->model, tool definitions become
// functionDeclarations. Live acceptance is UNVERIFIED.
func encodeGemini(model string, req domain.ModelRequest) ([]byte, error) {
	var system []string
	contents := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case domain.RoleSystem:
			system = append(system, m.Content)
		case domain.RoleTool:
			contents = append(contents, map[string]any{
				"role": "user",
				"parts": []any{map[string]any{
					"functionResponse": map[string]any{
						"name":     m.Name,
						"response": map[string]any{"output": m.Content},
					},
				}},
			})
		default:
			role := "user"
			if m.Role == domain.RoleAssistant {
				role = "model"
			}
			parts := make([]any, 0, 2)
			if m.Content != "" {
				parts = append(parts, map[string]any{"text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				var args any = map[string]any{}
				if len(tc.Arguments) > 0 {
					var v any
					if err := json.Unmarshal(tc.Arguments, &v); err == nil {
						args = v
					}
				}
				parts = append(parts, map[string]any{
					"functionCall": map[string]any{"name": tc.Name, "args": args},
				})
			}
			contents = append(contents, map[string]any{"role": role, "parts": parts})
		}
	}
	body := map[string]any{
		"model":    model,
		"contents": contents,
	}
	if len(system) > 0 {
		joined := ""
		for i, s := range system {
			if i > 0 {
				joined += "\n\n"
			}
			joined += s
		}
		body["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": joined}}}
	}
	gen := map[string]any{}
	if req.MaxTokens > 0 {
		gen["maxOutputTokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		gen["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		gen["topP"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		gen["stopSequences"] = req.Stop
	}
	if len(gen) > 0 {
		body["generationConfig"] = gen
	}
	if len(req.Tools) > 0 {
		decls := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			decls = append(decls, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  rawSchema(t.Parameters),
			})
		}
		body["tools"] = []any{map[string]any{"functionDeclarations": decls}}
	}
	return json.Marshal(body)
}
