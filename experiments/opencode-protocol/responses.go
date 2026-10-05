package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// decodeResponses decodes OpenAI Responses API streaming events into the
// canonical Result: output-text deltas, function-call argument assembly,
// finish state, and usage.
func decodeResponses(ctx context.Context, body io.Reader, o *Options) (Result, error) {
	opts := o.withDefaults()
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
			return Result{}, fmt.Errorf("gateway: malformed responses event %q: %w", name, err)
		}
		sawEvent = true
		switch name {
		case "response.output_text.delta":
			var p struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
				return Result{}, fmt.Errorf("gateway: malformed text delta: %w", err)
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
				return Result{}, fmt.Errorf("gateway: malformed output item: %w", err)
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
				return Result{}, fmt.Errorf("gateway: malformed arguments delta: %w", err)
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
		return Result{}, fmt.Errorf("gateway: responses stream carried no decodable events")
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
