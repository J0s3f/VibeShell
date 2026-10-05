package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// decodeChat decodes OpenAI-compatible Chat Completions SSE: streaming text
// deltas, tool_call assembly across chunks, finish_reason, and usage.
func decodeChat(ctx context.Context, body io.Reader, o *Options) (Result, error) {
	opts := o.withDefaults()
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
			return Result{}, fmt.Errorf("gateway: malformed chat chunk: %w", err)
		}
		if chunk.Error != nil {
			return Result{}, normalizeEnvelope(200, chatErrorMessage(chunk.Error))
		}
		if chunk.Object != "" && chunk.Object != "chat.completion.chunk" {
			return Result{}, fmt.Errorf("gateway: unexpected chat object %q", chunk.Object)
		}
		for _, choice := range chunk.Choices {
			sawChunk = true
			out.Text += choice.Delta.Content
			if choice.Delta.Reasoning != "" {
				// Reasoning deltas are preserved separately, never merged
				// into the emitted text.
			}
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
		return Result{}, fmt.Errorf("gateway: chat stream carried no choices")
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
