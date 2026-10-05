package opencode

import (
	"fmt"

	"j0s.at/vibeshell/internal/domain"
)

// Protocol identifies one wire-protocol family. It is distinct from the
// product route: the same model ID can exist on Console and Go with
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
// documentation (A06). Inference paths beneath these bases are
// conventional and UNVERIFIED without an authenticated key.
const (
	// ConsoleBase serves the Console catalogue and inference families.
	ConsoleBase = "https://opencode.ai/inference/v1"
	// ZenBase is the documented Console/Zen compatibility base.
	ZenBase = "https://opencode.ai/zen/v1"
	// GoBase is the OpenCode Go subscription base.
	GoBase = "https://opencode.ai/zen/go/v1"
)

// SessionHeader carries the Go subscription session identity. The value
// comes from the Gateway's SessionID hook and is omitted when empty. Live
// Go header behavior is UNVERIFIED (no key); this hook exists so a live
// probe can confirm or correct the wire shape without changing callers.
const SessionHeader = "x-opencode-session"

// ClientIdentity is the honest User-Agent sent on every inference and
// catalogue request. It names VibeShell, never a browser.
const ClientIdentity = "vibeshell (j0s.at/vibeshell; Go net/http)"

// EndpointFor resolves the inference URL for a product/protocol pair. A
// non-chat protocol never resolves to a chat URL, and an unknown protocol
// fails closed: callers cannot silently send a Messages/Responses/Gemini
// model to a Chat Completions endpoint.
func EndpointFor(product Product, protocol Protocol) (string, error) {
	var base string
	switch product {
	case ProductConsole:
		base = ConsoleBase
	case ProductGo:
		base = GoBase
	default:
		return "", fmt.Errorf("opencode: unknown product %q", string(product))
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
		return "", fmt.Errorf("opencode: unknown protocol %q; refusing chat default", string(protocol))
	}
	return base + suffix, nil
}

// RouteConfig binds one domain route ID to its product route, wire
// protocol, and exact upstream model ID. The model ID is sent verbatim;
// the adapter never rewrites it.
type RouteConfig struct {
	Product  Product
	Protocol Protocol
	// Model is the exact upstream model ID for the request payload.
	Model string
	// BaseURL overrides the product base for tests (httptest servers).
	// Empty selects the product base via EndpointFor.
	BaseURL string
}

// endpoint returns the inference URL for this route, honoring BaseURL.
func (r RouteConfig) endpoint() (string, error) {
	if r.BaseURL != "" {
		suffix, err := protocolSuffix(r.Protocol)
		if err != nil {
			return "", err
		}
		return r.BaseURL + suffix, nil
	}
	return EndpointFor(r.Product, r.Protocol)
}

func protocolSuffix(p Protocol) (string, error) {
	switch p {
	case ProtocolChat:
		return "/chat/completions", nil
	case ProtocolResponses:
		return "/responses", nil
	case ProtocolMessages:
		return "/messages", nil
	case ProtocolGemini:
		return "/models:streamGenerateContent", nil
	default:
		return "", fmt.Errorf("opencode: unknown protocol %q; refusing chat default", string(p))
	}
}

// AccountConfig groups the credential references eligible for one account.
// Keys share the account's quota: choosing among them never changes model
// selection odds, and a failure against one key does not disable the rest.
type AccountConfig struct {
	KeyRefs []domain.KeyRef
}
