package opencode

import (
	"encoding/json"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

func encodeTestRequest() domain.ModelRequest {
	temp := 0.5
	return domain.ModelRequest{
		Messages: []domain.Message{
			{Role: domain.RoleSystem, Content: "be simulated"},
			{Role: domain.RoleUser, Content: "open notes"},
			{Role: domain.RoleAssistant, Content: "done", ToolCalls: []domain.ToolCall{
				{ID: "call_1", Name: "world_lookup", Arguments: json.RawMessage(`{"path":"/x"}`)},
			}},
			{Role: domain.RoleTool, ToolCallID: "call_1", Name: "world_lookup", Content: `{"ok":true}`},
		},
		Tools: []domain.ToolDefinition{
			{Name: "world_lookup", Description: "look up", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
		MaxTokens:   64,
		Temperature: &temp,
		Stop:        []string{"halt"},
	}
}

func TestEncodeCarriesExactModelAndStream(t *testing.T) {
	route := RouteConfig{Product: ProductConsole, Model: "exact-model-id"}
	cases := map[Protocol]RouteConfig{
		ProtocolChat:      {Product: ProductConsole, Protocol: ProtocolChat, Model: "exact-model-id"},
		ProtocolResponses: {Product: ProductConsole, Protocol: ProtocolResponses, Model: "exact-model-id"},
		ProtocolMessages:  {Product: ProductConsole, Protocol: ProtocolMessages, Model: "exact-model-id"},
		ProtocolGemini:    {Product: ProductConsole, Protocol: ProtocolGemini, Model: "exact-model-id"},
	}
	_ = route
	for proto, cfg := range cases {
		raw, err := encodeRequest(cfg, encodeTestRequest())
		if err != nil {
			t.Fatalf("%s: %v", proto, err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("%s: %v", proto, err)
		}
		if body["model"] != "exact-model-id" {
			t.Fatalf("%s: model rewritten: %v", proto, body["model"])
		}
		if proto != ProtocolGemini {
			if body["stream"] != true {
				t.Fatalf("%s: stream not requested: %v", proto, body)
			}
		}
	}
}

func TestEncodeChatShape(t *testing.T) {
	raw, err := encodeRequest(RouteConfig{Protocol: ProtocolChat, Model: "m"}, encodeTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
		Tools    []map[string]any `json:"tools"`
		Stop     []string         `json:"stop"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 4 || len(body.Tools) != 1 || len(body.Stop) != 1 {
		t.Fatalf("shape = %s", raw)
	}
	if body.Messages[0]["role"] != "system" || body.Messages[3]["tool_call_id"] != "call_1" {
		t.Fatalf("roles = %s", raw)
	}
}

func TestEncodeAnthropicLiftsSystem(t *testing.T) {
	raw, err := encodeRequest(RouteConfig{Protocol: ProtocolMessages, Model: "m"}, encodeTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		System   string           `json:"system"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.System != "be simulated" {
		t.Fatalf("system = %q", body.System)
	}
	for _, m := range body.Messages {
		if m["role"] == "system" {
			t.Fatalf("system message leaked into messages: %s", raw)
		}
	}
}

func TestEncodeGeminiInstructionAndRoles(t *testing.T) {
	raw, err := encodeRequest(RouteConfig{Protocol: ProtocolGemini, Model: "m"}, encodeTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"systemInstruction"`, `"role":"model"`, `"functionCall"`, `"functionResponse"`, `"generationConfig"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("gemini body missing %s: %s", want, s)
		}
	}
}

func TestEncodeResponsesToolOutputItems(t *testing.T) {
	raw, err := encodeRequest(RouteConfig{Protocol: ProtocolResponses, Model: "m"}, encodeTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"function_call_output"`, `"call_id":"call_1"`, `"max_output_tokens":64`} {
		if !strings.Contains(s, want) {
			t.Fatalf("responses body missing %s: %s", want, s)
		}
	}
}

func TestEncodeRejectsNegativeTokens(t *testing.T) {
	req := encodeTestRequest()
	req.MaxTokens = -1
	if _, err := encodeRequest(RouteConfig{Protocol: ProtocolChat, Model: "m"}, req); err == nil {
		t.Fatal("negative max_tokens accepted")
	}
}

func TestEncodeRefusesUnknownProtocol(t *testing.T) {
	if _, err := encodeRequest(RouteConfig{Protocol: "", Model: "m"}, encodeTestRequest()); err == nil {
		t.Fatal("unknown protocol encoded instead of refusing")
	}
}
