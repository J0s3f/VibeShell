package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// ToolCall is one canonical assembled tool invocation.
type ToolCall struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// Usage is canonical token accounting across all families.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

// Result is the canonical assembled outcome of one decoded stream.
type Result struct {
	Text         string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
}

// Finish reasons shared across families.
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolCalls     = "tool_calls"
	FinishContentFilter = "content_filter"
)

// DecodeStream decodes one complete SSE response body of the given
// protocol family into its canonical Result. HTTP error statuses must be
// handled by checkStatus before calling DecodeStream; a 200 body carrying
// a JSON error envelope is still reported as a *providerError, never as
// success.
func DecodeStream(ctx context.Context, protocol Protocol, body io.Reader, bounds Bounds) (Result, error) {
	opts := bounds.withDefaults()
	switch protocol {
	case ProtocolChat:
		return decodeChat(ctx, body, opts)
	case ProtocolResponses:
		return decodeResponses(ctx, body, opts)
	case ProtocolMessages:
		return decodeAnthropic(ctx, body, opts)
	case ProtocolGemini:
		return decodeGemini(ctx, body, opts)
	default:
		return Result{}, fmt.Errorf("opencode: unknown protocol %q; refusing chat default", string(protocol))
	}
}

// toResponse converts a decoded Result into the canonical domain response.
// Tool arguments that are not valid JSON are reported as an invalid_response
// failure (PLAN 9.1: bounded repair, then another candidate), never passed
// through silently.
func (r Result) toResponse(req domain.ModelRequest, latencyMs, now int64) (domain.ModelResponse, error) {
	calls := make([]domain.ToolCall, 0, len(r.ToolCalls))
	for _, tc := range r.ToolCalls {
		var args json.RawMessage
		if tc.Arguments != "" {
			if !json.Valid([]byte(tc.Arguments)) {
				env := domain.NewErrorEnvelope(domain.FailureInvalidResponse,
					fmt.Sprintf("model %s returned malformed tool arguments for %q", req.RouteID.String(), tc.Name),
					req.RouteID, req.AccountID, now)
				return domain.ModelResponse{}, env
			}
			args = json.RawMessage(tc.Arguments)
		}
		calls = append(calls, domain.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: args})
	}
	msg := domain.Message{Role: domain.RoleAssistant, Content: stripReasoningBlocks(r.Text)}
	if len(calls) > 0 {
		msg.ToolCalls = calls
	}
	return domain.ModelResponse{
		RequestID:    req.RequestID,
		RouteID:      req.RouteID,
		AccountID:    req.AccountID,
		Message:      msg,
		FinishReason: mapFinishReason(r.FinishReason),
		Usage: domain.Usage{
			PromptTokens:     int(r.Usage.InputTokens),
			CompletionTokens: int(r.Usage.OutputTokens),
			TotalTokens:      int(r.Usage.TotalTokens),
		},
		LatencyMs: latencyMs,
		Timestamp: now,
	}, nil
}

// mapFinishReason maps canonical finish tokens onto domain values,
// passing unknown values through unchanged rather than inventing data.
func mapFinishReason(reason string) domain.FinishReason {
	switch reason {
	case FinishStop:
		return domain.FinishReasonStop
	case FinishLength:
		return domain.FinishReasonLength
	case FinishToolCalls:
		return domain.FinishReasonToolCalls
	case FinishContentFilter:
		return domain.FinishReasonContentFilter
	case "":
		return domain.FinishReasonStop
	default:
		return domain.FinishReason(reason)
	}
}

// ---------------------------------------------------------------------------
// Chat Completions
// ---------------------------------------------------------------------------

// decodeChat decodes OpenAI-compatible Chat Completions SSE: streaming
// text deltas, tool_call assembly across chunks, finish_reason, and usage.
func decodeChat(ctx context.Context, body io.Reader, opts Bounds) (Result, error) {
	events, bare, err := scanSSE(ctx, boundedReader(body, opts.MaxBodyBytes), opts.MaxFrameBytes, opts.MaxEvents)
	if err != nil {
		return Result{}, err
	}
	if len(events) == 0 {
		return Result{}, emptyStreamError(bare, "chat")
	}
	var (
		out      Result
		byIndex  = map[int]*ToolCall{}
		order    []int
		sawChunk bool
	)
	for _, ev := range events {
		if strings.TrimSpace(ev.Data) == "[DONE]" {
			break
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
			// A 200 carrying a JSON error envelope is an error, never success.
			if perr := sniffErrorEnvelope(200, []byte(ev.Data)); perr != nil {
				return Result{}, perr
			}
			return Result{}, fmt.Errorf("opencode: malformed chat chunk: %w", err)
		}
		if chunk.Error != nil {
			return Result{}, normalizeEnvelope(200, chatErrorMessage(chunk.Error))
		}
		if chunk.Object != "" && chunk.Object != "chat.completion.chunk" {
			return Result{}, fmt.Errorf("opencode: unexpected chat object %q", chunk.Object)
		}
		for _, choice := range chunk.Choices {
			sawChunk = true
			out.Text += choice.Delta.Content
			for _, tc := range choice.Delta.ToolCalls {
				call, dup := byIndex[tc.Index]
				if !dup {
					call = &ToolCall{Index: tc.Index}
					byIndex[tc.Index] = call
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					call.ID = tc.ID
				}
				if tc.Function.Name != "" {
					call.Name = tc.Function.Name
				}
				call.Arguments += tc.Function.Arguments
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				out.FinishReason = normalizeFinish(*choice.FinishReason)
			}
		}
		if chunk.Usage != nil {
			out.Usage = Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens:  chunk.Usage.TotalTokens,
			}
		}
	}
	if !sawChunk {
		// The stream may have carried only an error envelope handled above;
		// anything else with zero choices is malformed, not empty success.
		if perr := sniffStreamError(events); perr != nil {
			return Result{}, perr
		}
		return Result{}, fmt.Errorf("opencode: chat stream carried no choices")
	}
	for _, idx := range order {
		out.ToolCalls = append(out.ToolCalls, *byIndex[idx])
	}
	if out.FinishReason == "" && len(out.ToolCalls) > 0 {
		out.FinishReason = FinishToolCalls
	}
	return out, nil
}

type chatChunk struct {
	Object  string `json:"object"`
	Error   any    `json:"error"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning_content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

// normalizeFinish maps provider finish tokens onto the canonical set,
// passing unknown values through unchanged rather than inventing data.
func normalizeFinish(reason string) string {
	switch reason {
	case "stop", "length", "tool_calls", "content_filter":
		return reason
	default:
		return reason
	}
}

// ---------------------------------------------------------------------------
// Responses API
// ---------------------------------------------------------------------------

// decodeResponses decodes OpenAI Responses API streaming events into the
// canonical Result: output-text deltas, function-call argument assembly,
// finish state, and usage.
func decodeResponses(ctx context.Context, body io.Reader, opts Bounds) (Result, error) {
	events, bare, err := scanSSE(ctx, boundedReader(body, opts.MaxBodyBytes), opts.MaxFrameBytes, opts.MaxEvents)
	if err != nil {
		return Result{}, err
	}
	if len(events) == 0 {
		return Result{}, emptyStreamError(bare, "responses")
	}
	var (
		out      Result
		calls    = map[string]*ToolCall{}
		order    []string
		sawEvent bool
	)
	for _, ev := range events {
		name := ev.Type
		if strings.TrimSpace(ev.Data) == "[DONE]" {
			break
		}
		// Responses payloads are always JSON objects; error events are
		// explicit failures, never unknown-type noise.
		switch name {
		case "", "response.output_text.delta", "response.function_call_arguments.delta",
			"response.output_item.added", "response.output_item.done",
			"response.completed", "response.failed", "response.incomplete", "error":
		default:
			continue // Unknown event types are ignored, not fatal.
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(ev.Data), &raw); err != nil {
			if perr := sniffErrorEnvelope(200, []byte(ev.Data)); perr != nil {
				return Result{}, perr
			}
			return Result{}, fmt.Errorf("opencode: malformed responses event %q: %w", name, err)
		}
		sawEvent = true
		switch name {
		case "response.output_text.delta":
			var p struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
				return Result{}, fmt.Errorf("opencode: malformed text delta: %w", err)
			}
			out.Text += p.Delta
		case "response.output_item.added", "response.output_item.done":
			var p struct {
				Item struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"item"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
				return Result{}, fmt.Errorf("opencode: malformed output item: %w", err)
			}
			if p.Item.Type != "function_call" || p.Item.ID == "" {
				continue
			}
			if _, dup := calls[p.Item.ID]; !dup {
				calls[p.Item.ID] = &ToolCall{ID: p.Item.ID, Name: p.Item.Name, Index: len(order)}
				order = append(order, p.Item.ID)
			} else if p.Item.Name != "" {
				calls[p.Item.ID].Name = p.Item.Name
			}
		case "response.function_call_arguments.delta":
			var p struct {
				ItemID string `json:"item_id"`
				Delta  string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
				return Result{}, fmt.Errorf("opencode: malformed arguments delta: %w", err)
			}
			call, ok := calls[p.ItemID]
			if !ok {
				call = &ToolCall{ID: p.ItemID, Index: len(order)}
				calls[p.ItemID] = call
				order = append(order, p.ItemID)
			}
			call.Arguments += p.Delta
		case "response.completed":
			out.FinishReason = FinishStop
			out.Usage = extractResponsesUsage(raw)
		case "response.incomplete":
			out.FinishReason = FinishLength
			out.Usage = extractResponsesUsage(raw)
		case "response.failed", "error":
			return Result{}, normalizeEnvelope(200, strings.TrimSpace(ev.Data))
		}
	}
	if !sawEvent {
		if perr := sniffStreamError(events); perr != nil {
			return Result{}, perr
		}
		return Result{}, fmt.Errorf("opencode: responses stream carried no decodable events")
	}
	for _, id := range order {
		out.ToolCalls = append(out.ToolCalls, *calls[id])
	}
	if out.FinishReason == "" && len(out.ToolCalls) > 0 {
		out.FinishReason = FinishToolCalls
	}
	return out, nil
}

func extractResponsesUsage(raw map[string]json.RawMessage) Usage {
	// Usage may sit at .response.usage or .usage depending on the event.
	candidates := []json.RawMessage{raw["response"], raw["usage"]}
	for _, c := range candidates {
		if len(c) == 0 {
			continue
		}
		var holder struct {
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
				TotalTokens  int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(c, &holder); err == nil && holder.Usage != nil {
			return Usage{
				InputTokens:  holder.Usage.InputTokens,
				OutputTokens: holder.Usage.OutputTokens,
				TotalTokens:  holder.Usage.TotalTokens,
			}
		}
		var direct struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		}
		if err := json.Unmarshal(c, &direct); err == nil &&
			(direct.InputTokens != 0 || direct.OutputTokens != 0 || direct.TotalTokens != 0) {
			return Usage(direct)
		}
	}
	return Usage{}
}

// ---------------------------------------------------------------------------
// Anthropic Messages
// ---------------------------------------------------------------------------

// decodeAnthropic decodes Anthropic-compatible Messages SSE:
// content-block text deltas, tool_use input_json_delta assembly,
// message_delta usage, and explicit error events.
func decodeAnthropic(ctx context.Context, body io.Reader, opts Bounds) (Result, error) {
	events, bare, err := scanSSE(ctx, boundedReader(body, opts.MaxBodyBytes), opts.MaxFrameBytes, opts.MaxEvents)
	if err != nil {
		return Result{}, err
	}
	if len(events) == 0 {
		return Result{}, emptyStreamError(bare, "messages")
	}
	var (
		out    Result
		blocks = map[int]*ToolCall{}
		order  []int
	)
	for _, ev := range events {
		switch ev.Type {
		case "ping":
			continue // Keepalive: ignored.
		case "message_start", "content_block_start", "content_block_delta",
			"content_block_stop", "message_delta", "message_stop", "error":
		default:
			// Unknown event types are ignored, not fatal.
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(ev.Data), &raw); err != nil {
			if perr := sniffErrorEnvelope(200, []byte(ev.Data)); perr != nil {
				return Result{}, perr
			}
			return Result{}, fmt.Errorf("opencode: malformed messages event %q: %w", ev.Type, err)
		}
		switch ev.Type {
		case "error":
			return Result{}, normalizeEnvelope(200, strings.TrimSpace(ev.Data))
		case "content_block_start":
			var p struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type  string `json:"type"`
					ID    string `json:"id"`
					Name  string `json:"name"`
					Input any    `json:"input"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
				return Result{}, fmt.Errorf("opencode: malformed block start: %w", err)
			}
			if p.ContentBlock.Type != "tool_use" {
				continue
			}
			blocks[p.Index] = &ToolCall{Index: len(order), ID: p.ContentBlock.ID, Name: p.ContentBlock.Name}
			order = append(order, p.Index)
		case "content_block_delta":
			var p struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
				return Result{}, fmt.Errorf("opencode: malformed block delta: %w", err)
			}
			switch p.Delta.Type {
			case "text_delta":
				out.Text += p.Delta.Text
			case "input_json_delta":
				call, ok := blocks[p.Index]
				if !ok {
					call = &ToolCall{Index: len(order)}
					blocks[p.Index] = call
					order = append(order, p.Index)
				}
				call.Arguments += p.Delta.PartialJSON
			default:
				// thinking/signature deltas are not shell output: ignored.
			}
		case "message_delta":
			var p struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage *struct {
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
				return Result{}, fmt.Errorf("opencode: malformed message delta: %w", err)
			}
			if p.Delta.StopReason != "" {
				out.FinishReason = normalizeAnthropicStop(p.Delta.StopReason)
			}
			if p.Usage != nil {
				out.Usage.OutputTokens = p.Usage.OutputTokens
				out.Usage.TotalTokens = out.Usage.InputTokens + out.Usage.OutputTokens
			}
		case "message_start":
			var p struct {
				Message struct {
					Usage *struct {
						InputTokens  int64 `json:"input_tokens"`
						OutputTokens int64 `json:"output_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
				return Result{}, fmt.Errorf("opencode: malformed message start: %w", err)
			}
			if p.Message.Usage != nil {
				out.Usage.InputTokens = p.Message.Usage.InputTokens
				out.Usage.TotalTokens = out.Usage.InputTokens + out.Usage.OutputTokens
			}
		case "message_stop":
			// Terminal marker; finish state already came from message_delta.
		}
	}
	if out.Text == "" && len(out.ToolCalls) == 0 && len(order) == 0 {
		if perr := sniffStreamError(events); perr != nil {
			return Result{}, perr
		}
		return Result{}, fmt.Errorf("opencode: messages stream carried no content")
	}
	for _, idx := range order {
		out.ToolCalls = append(out.ToolCalls, *blocks[idx])
	}
	if out.FinishReason == "" {
		out.FinishReason = FinishStop
	}
	return out, nil
}

// normalizeAnthropicStop maps Messages stop reasons onto canonical values.
func normalizeAnthropicStop(reason string) string {
	switch reason {
	case "end_turn":
		return FinishStop
	case "max_tokens":
		return FinishLength
	case "tool_use":
		return FinishToolCalls
	case "refusal":
		return FinishContentFilter
	default:
		return reason
	}
}

// ---------------------------------------------------------------------------
// Gemini generateContent
// ---------------------------------------------------------------------------

// decodeGemini decodes Gemini-compatible generateContent streaming
// (alt=sse): candidate text parts across chunks, finishReason, and
// usageMetadata. Only the first candidate is consumed; the rest is not
// shell output.
func decodeGemini(ctx context.Context, body io.Reader, opts Bounds) (Result, error) {
	events, bare, err := scanSSE(ctx, boundedReader(body, opts.MaxBodyBytes), opts.MaxFrameBytes, opts.MaxEvents)
	if err != nil {
		return Result{}, err
	}
	if len(events) == 0 {
		return Result{}, emptyStreamError(bare, "gemini")
	}
	var (
		out      Result
		sawChunk bool
	)
	for _, ev := range events {
		if strings.TrimSpace(ev.Data) == "[DONE]" {
			break
		}
		var chunk geminiChunk
		if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
			if perr := sniffErrorEnvelope(200, []byte(ev.Data)); perr != nil {
				return Result{}, perr
			}
			return Result{}, fmt.Errorf("opencode: malformed gemini chunk: %w", err)
		}
		if chunk.Error != nil {
			return Result{}, normalizeEnvelope(200, string(mustJSON(chunk.Error)))
		}
		if len(chunk.Candidates) == 0 && chunk.Usage == nil {
			if perr := sniffErrorEnvelope(200, []byte(ev.Data)); perr != nil {
				return Result{}, perr
			}
			continue
		}
		if len(chunk.Candidates) > 0 {
			sawChunk = true
			for _, part := range chunk.Candidates[0].Content.Parts {
				out.Text += part.Text
			}
			if r := chunk.Candidates[0].FinishReason; r != "" && out.FinishReason == "" {
				out.FinishReason = normalizeGeminiFinish(r)
			}
		}
		if chunk.Usage != nil {
			out.Usage = Usage{
				InputTokens:  chunk.Usage.PromptTokenCount,
				OutputTokens: chunk.Usage.CandidatesTokenCount,
				TotalTokens:  chunk.Usage.TotalTokenCount,
			}
		}
	}
	if !sawChunk {
		if perr := sniffStreamError(events); perr != nil {
			return Result{}, perr
		}
		return Result{}, fmt.Errorf("opencode: gemini stream carried no candidates")
	}
	if out.FinishReason == "" {
		out.FinishReason = FinishStop
	}
	return out, nil
}

type geminiChunk struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	Usage *struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		TotalTokenCount      int64 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	Error any `json:"error"`
}

// normalizeGeminiFinish maps generateContent finish reasons canonically.
func normalizeGeminiFinish(reason string) string {
	switch reason {
	case "STOP":
		return FinishStop
	case "MAX_TOKENS":
		return FinishLength
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return FinishContentFilter
	default:
		return reason
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
