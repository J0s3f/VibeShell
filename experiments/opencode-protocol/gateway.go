// Package gateway decodes the OpenCode wire-protocol families behind one
// canonical shape: OpenAI-compatible Chat Completions SSE, the OpenAI
// Responses API stream, Anthropic-compatible Messages SSE, and
// Gemini-compatible generateContent SSE (alt=sse).
//
// Every decoder is exercised against local httptest servers and recorded
// fixtures. No live authenticated inference was performed while qualifying
// these codecs; tool-call acceptance, live streaming behavior, and Go
// x-opencode-session behavior remain UNVERIFIED (see
// docs/research/2026-10-03-opencode-protocol-spike.md).
package gateway

import (
	"context"
	"fmt"
	"io"
)

// Protocol identifies one wire-protocol family. Product route (Console vs Go)
// is tracked separately: the same model ID can exist on both products with
// different allowances, so a Protocol alone never selects an endpoint.
type Protocol string

const (
	// ProtocolChat is OpenAI-compatible Chat Completions streaming.
	ProtocolChat Protocol = "chat"
	// ProtocolResponses is the OpenAI Responses API streaming event set.
	ProtocolResponses Protocol = "responses"
	// ProtocolMessages is Anthropic-compatible Messages SSE.
	ProtocolMessages Protocol = "messages"
	// ProtocolGemini is Gemini-compatible generateContent streaming.
	ProtocolGemini Protocol = "gemini"
)

// Product identifies the OpenCode product route behind an endpoint.
type Product string

const (
	// ProductConsole is the Console / pay-as-you-go inference route.
	ProductConsole Product = "console"
	// ProductGo is the OpenCode Go subscription route.
	ProductGo Product = "go"
)

// Base endpoints observed from public catalogue GETs and product
// documentation. Inference paths beneath these bases (suffix routing per
// protocol, Go session headers) are UNVERIFIED without a key.
const (
	// ConsoleBase serves the Console catalogue and inference families.
	ConsoleBase = "https://opencode.ai/inference/v1"
	// ZenBase is the documented Console/Zen compatibility base.
	ZenBase = "https://opencode.ai/zen/v1"
	// GoBase is the OpenCode Go subscription base.
	GoBase = "https://opencode.ai/zen/go/v1"
)

// EndpointFor resolves the inference URL for a product/protocol pair. A
// non-chat protocol never resolves to a chat URL: callers cannot silently
// send a Messages/Responses/Gemini model to a Chat Completions endpoint.
// Suffix routing beyond the observed catalogue paths is UNVERIFIED live.
func EndpointFor(product Product, protocol Protocol) (string, error) {
	var base string
	switch product {
	case ProductConsole:
		base = ConsoleBase
	case ProductGo:
		base = GoBase
	default:
		return "", fmt.Errorf("gateway: unknown product %q", string(product))
	}
	var suffix string
	switch protocol {
	case ProtocolChat:
		suffix = "/chat/completions"
	case ProtocolResponses:
		suffix = "/responses"
	case ProtocolMessages:
		suffix = "/messages"
	case ProtocolGemini:
		suffix = "/models:streamGenerateContent"
	default:
		return "", fmt.Errorf("gateway: unknown protocol %q", string(protocol))
	}
	return base + suffix, nil
}

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

// Options bounds every decode. Zero values select documented defaults.
type Options struct {
	// MaxFrameBytes caps one SSE frame (event block). Default 1 MiB.
	MaxFrameBytes int
	// MaxBodyBytes caps the whole stream body. Default 8 MiB.
	MaxBodyBytes int64
	// MaxEvents caps decoded SSE events. Default 4096.
	MaxEvents int
}

func (o *Options) withDefaults() Options {
	out := Options{
		MaxFrameBytes: 1 << 20,
		MaxBodyBytes:  8 << 20,
		MaxEvents:     4096,
	}
	if o == nil {
		return out
	}
	if o.MaxFrameBytes > 0 {
		out.MaxFrameBytes = o.MaxFrameBytes
	}
	if o.MaxBodyBytes > 0 {
		out.MaxBodyBytes = o.MaxBodyBytes
	}
	if o.MaxEvents > 0 {
		out.MaxEvents = o.MaxEvents
	}
	return out
}

// DecodeStream decodes one complete SSE response body of the given protocol
// family into its canonical Result. HTTP error statuses must be handled by
// CheckStatus before calling DecodeStream; a 200 body carrying a JSON error
// envelope is still reported as a *ProviderError, never as success.
func DecodeStream(ctx context.Context, protocol Protocol, body io.Reader, opts *Options) (Result, error) {
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
		return Result{}, fmt.Errorf("gateway: unknown protocol %q", string(protocol))
	}
}
