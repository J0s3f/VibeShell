package opencode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Gateway implements ports.ModelGateway over stdlib net/http. It owns
// product-route/protocol separation, per-request credentials, honest
// client identity, bounded streaming decode, and redacted PLAN 9.1 error
// normalization. It owns no routing policy: route/account selection,
// failover, and health live in the domain and application layers.
type Gateway struct {
	// HTTP is the transport. Nil selects a default client with a
	// conservative per-request ceiling; request deadlines still come
	// from the caller context and req.DeadlineMs.
	HTTP *http.Client
	// Routes binds route IDs to product/protocol/model. Unknown routes
	// fail as model_not_found; unknown protocols fail closed as
	// invalid_arguments, never as a silent chat default.
	Routes map[domain.RouteID]RouteConfig
	// Accounts binds account IDs to their credential references.
	Accounts map[domain.AccountID]AccountConfig
	// Secrets resolves opaque KeyRefs to key material per request.
	// Resolved material travels only in the Authorization header value:
	// it is never logged, persisted, or formatted into errors.
	Secrets SecretResolver
	// Clock supplies wall-clock milliseconds for latency, timestamps,
	// and deadline arithmetic. Nil selects the system clock.
	Clock ports.Clock
	// SessionID supplies the Go session identity for SessionHeader.
	// Nil or empty means the header is omitted. Live Go header behavior
	// is UNVERIFIED; this hook keeps the wire shape adjustable from one
	// place after a live probe.
	SessionID func() string
	// Bounds caps streamed responses; zero selects documented defaults.
	Bounds Bounds
	// MaxErrorBody caps error-body reads. Zero selects 64 KiB.
	MaxErrorBody int64
}

// SecretResolver resolves one opaque credential reference to key material
// for a single request. Implementations read mounted secret files or
// container environment; they never log the material they return.
type SecretResolver interface {
	ResolveKey(ref domain.KeyRef) (string, error)
}

// MapSecrets is an in-memory SecretResolver for tests and thin composition
// roots that already hold resolved references. Production wiring should
// prefer a file/environment-backed resolver.
type MapSecrets map[domain.KeyRef]string

// ResolveKey implements SecretResolver.
func (m MapSecrets) ResolveKey(ref domain.KeyRef) (string, error) {
	if s, ok := m[ref]; ok && s != "" {
		return s, nil
	}
	return "", fmt.Errorf("opencode: no secret for key reference")
}

// systemClock is the default Clock.
type systemClock struct{}

func (systemClock) NowUnixMilli() int64   { return time.Now().UnixMilli() }
func (systemClock) MonotonicNanos() int64 { return time.Now().UnixNano() }

// Compile-time contract check: Gateway serves the ModelGateway port.
var _ ports.ModelGateway = (*Gateway)(nil)

func (g *Gateway) clock() ports.Clock {
	if g.Clock != nil {
		return g.Clock
	}
	return systemClock{}
}

func (g *Gateway) transport() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 120 * time.Second}
}

func (g *Gateway) maxErrorBody() int64 {
	if g.MaxErrorBody > 0 {
		return g.MaxErrorBody
	}
	return 64 << 10
}

// Request performs one provider-neutral inference against the route's
// product/protocol endpoint and normalizes the stream into a canonical
// domain response. Failures are domain.ErrorEnvelope values in PLAN 9.1
// classes with redacted details; unknown errors are FailureUnknown, never
// success.
func (g *Gateway) Request(ctx context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
	now := g.clock().NowUnixMilli()
	fail := func(class domain.FailureClass, msg string) (domain.ModelResponse, error) {
		return domain.ModelResponse{}, domain.NewErrorEnvelope(class, Redact(msg), req.RouteID, req.AccountID, now)
	}

	if req.RouteID.IsZero() {
		return fail(domain.FailureInvalidArguments, "missing route ID")
	}
	if req.AccountID.IsZero() {
		return fail(domain.FailureInvalidArguments, "missing account ID")
	}
	if len(req.Messages) == 0 {
		return fail(domain.FailureInvalidArguments, "no messages in request")
	}
	route, ok := g.Routes[req.RouteID]
	if !ok {
		return fail(domain.FailureModelNotFound, "unknown model route")
	}
	account, ok := g.Accounts[req.AccountID]
	if !ok || len(account.KeyRefs) == 0 {
		return fail(domain.FailureInvalidCredential, "unknown account or no credentials")
	}
	var ref domain.KeyRef
	for _, k := range account.KeyRefs {
		if !k.IsZero() {
			ref = k
			break
		}
	}
	if ref.IsZero() {
		return fail(domain.FailureInvalidCredential, "account has no valid credential reference")
	}
	if g.Secrets == nil {
		return fail(domain.FailureInvalidCredential, "no secret resolver configured")
	}
	secret, err := g.Secrets.ResolveKey(ref)
	if err != nil || secret == "" {
		return fail(domain.FailureInvalidCredential, "credential unavailable for account")
	}

	// Product/protocol separation: an unclassified protocol never
	// resolves to a chat URL; it fails here as invalid_arguments.
	endpoint, err := route.endpoint()
	if err != nil {
		return fail(domain.FailureInvalidArguments, err.Error())
	}
	wire, err := encodeRequest(route, req)
	if err != nil {
		return fail(domain.FailureInvalidArguments, err.Error())
	}

	if req.DeadlineMs > 0 {
		remaining := req.DeadlineMs - now
		if remaining <= 0 {
			return fail(domain.FailureNetworkTimeout, "request deadline already exceeded")
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.UnixMilli(req.DeadlineMs))
		defer cancel()
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(wire))
	if err != nil {
		return fail(domain.FailureUnknown, fmt.Sprintf("building request: %v", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+secret)
	httpReq.Header.Set("User-Agent", ClientIdentity)
	if g.SessionID != nil {
		if session := g.SessionID(); session != "" {
			httpReq.Header.Set(SessionHeader, session)
		}
	}

	start := g.clock().NowUnixMilli()
	resp, err := g.transport().Do(httpReq)
	if err != nil {
		return domain.ModelResponse{}, g.transportError(ctx, req, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, g.maxErrorBody()))
		perr := normalizeError(resp.StatusCode, body)
		return domain.ModelResponse{}, perr.envelope(req.RouteID, req.AccountID, parseRetryAfter(resp.Header.Get("Retry-After")), g.clock().NowUnixMilli())
	}

	result, err := DecodeStream(ctx, route.Protocol, resp.Body, g.Bounds)
	if err != nil {
		return domain.ModelResponse{}, g.decodeError(ctx, req, err, resp.Header.Get("Retry-After"))
	}
	end := g.clock().NowUnixMilli()
	latency := end - start
	if latency < 0 {
		latency = 0
	}
	out, err := result.toResponse(req, latency, end)
	if err != nil {
		return domain.ModelResponse{}, err
	}
	return out, nil
}

// transportError classifies a failed HTTP round trip. Context cancellation
// is the user's own abort (no health penalty); everything else on the
// wire is a network timeout/outage signal for routing.
func (g *Gateway) transportError(ctx context.Context, req domain.ModelRequest, err error) error {
	now := g.clock().NowUnixMilli()
	switch {
	case errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled:
		return domain.NewErrorEnvelope(domain.FailureUserCancelled, "request cancelled", req.RouteID, req.AccountID, now)
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded:
		return domain.NewErrorEnvelope(domain.FailureNetworkTimeout, "request deadline exceeded", req.RouteID, req.AccountID, now)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return domain.NewErrorEnvelope(domain.FailureNetworkTimeout, "provider timed out", req.RouteID, req.AccountID, now)
	}
	return domain.NewErrorEnvelope(domain.FailureNetworkTimeout, Redact(fmt.Sprintf("provider transport error: %v", err)), req.RouteID, req.AccountID, now)
}

// decodeError classifies a streaming failure into PLAN 9.1 classes with
// redacted details. Cancellation stays user_cancelled; truncation is a
// network signal; malformed or over-bound payloads are invalid_response;
// provider envelopes keep their normalized class.
func (g *Gateway) decodeError(ctx context.Context, req domain.ModelRequest, err error, retryAfterHeader string) error {
	now := g.clock().NowUnixMilli()
	retryAfter := parseRetryAfter(retryAfterHeader)
	var perr *providerError
	if errors.As(err, &perr) {
		return perr.envelope(req.RouteID, req.AccountID, retryAfter, now)
	}
	if errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled {
		return domain.NewErrorEnvelope(domain.FailureUserCancelled, "request cancelled", req.RouteID, req.AccountID, now)
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		env := domain.NewErrorEnvelope(domain.FailureNetworkTimeout, "request deadline exceeded", req.RouteID, req.AccountID, now)
		env.RetryAfter = retryAfter
		return env
	}
	msg := Redact(err.Error())
	switch {
	case IsTruncated(err):
		return domain.NewErrorEnvelope(domain.FailureNetworkTimeout, msg, req.RouteID, req.AccountID, now)
	case isBoundError(err):
		env := domain.NewErrorEnvelope(domain.FailureInvalidResponse, msg, req.RouteID, req.AccountID, now)
		env.RetryAfter = retryAfter
		return env
	case isMalformedError(err):
		env := domain.NewErrorEnvelope(domain.FailureInvalidResponse, msg, req.RouteID, req.AccountID, now)
		env.RetryAfter = retryAfter
		return env
	default:
		env := domain.NewErrorEnvelope(domain.FailureUnknown, msg, req.RouteID, req.AccountID, now)
		env.RawError = msg
		env.RetryAfter = retryAfter
		return env
	}
}

func isBoundError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "exceeds") ||
		strings.Contains(s, "frame exceeds") ||
		strings.Contains(s, "too many SSE events")
}

func isMalformedError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "malformed") ||
		strings.Contains(s, "unexpected") ||
		strings.Contains(s, "carried no") ||
		strings.Contains(s, "empty")
}

// parseRetryAfter converts a Retry-After header (delay seconds, the only
// form honored) to milliseconds. HTTP dates and unparsable values yield
// nil: no invented cooldown.
func parseRetryAfter(header string) *int64 {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil
	}
	if secs, err := strconv.ParseInt(header, 10, 64); err == nil && secs >= 0 && secs <= 3600 {
		ms := secs * 1000
		return &ms
	}
	return nil
}
