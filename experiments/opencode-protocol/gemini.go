package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// decodeGemini decodes Gemini-compatible generateContent streaming
// (alt=sse): candidate text parts across chunks, finishReason, and
// usageMetadata. Only the first candidate is consumed; the rest is not
// shell output.
func decodeGemini(ctx context.Context, body io.Reader, o *Options) (Result, error) {
	opts := o.withDefaults()
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
			return Result{}, fmt.Errorf("gateway: malformed gemini chunk: %w", err)
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
		return Result{}, fmt.Errorf("gateway: gemini stream carried no candidates")
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
