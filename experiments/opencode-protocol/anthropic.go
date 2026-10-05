package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// decodeAnthropic decodes Anthropic-compatible Messages SSE: content-block
// text deltas, tool_use input_json_delta assembly, message_delta usage, and
// explicit error events.
func decodeAnthropic(ctx context.Context, body io.Reader, o *Options) (Result, error) {
	opts := o.withDefaults()
	events, bare, err := scanSSE(ctx, boundedReader(body, opts.MaxBodyBytes), opts.MaxFrameBytes, opts.MaxEvents)
	if err != nil {
		return Result{}, err
	}
	if len(events) == 0 {
		return Result{}, emptyStreamError(bare, "messages")
	}
	var (
		out     Result
		blocks  = map[int]*ToolCall{}
		order   []int
		sawStop bool
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
			return Result{}, fmt.Errorf("gateway: malformed messages event %q: %w", ev.Type, err)
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
				return Result{}, fmt.Errorf("gateway: malformed block start: %w", err)
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
				return Result{}, fmt.Errorf("gateway: malformed block delta: %w", err)
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
				return Result{}, fmt.Errorf("gateway: malformed message delta: %w", err)
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
				return Result{}, fmt.Errorf("gateway: malformed message start: %w", err)
			}
			if p.Message.Usage != nil {
				out.Usage.InputTokens = p.Message.Usage.InputTokens
				out.Usage.TotalTokens = out.Usage.InputTokens + out.Usage.OutputTokens
			}
		case "message_stop":
			sawStop = true
		}
	}
	_ = sawStop
	if out.Text == "" && len(out.ToolCalls) == 0 && len(order) == 0 {
		if perr := sniffStreamError(events); perr != nil {
			return Result{}, perr
		}
		return Result{}, fmt.Errorf("gateway: messages stream carried no content")
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
