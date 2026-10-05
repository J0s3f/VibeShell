package opencode

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"j0s.at/vibeshell/internal/domain"
)

// stubClock is a deterministic ports.Clock.
type stubClock struct{ now int64 }

func (s stubClock) NowUnixMilli() int64   { return s.now }
func (s stubClock) MonotonicNanos() int64 { return s.now * 1000 }

// chunkReader delivers at most n bytes per Read, proving framing never
// depends on transport segmentation.
type chunkReader struct {
	s string
	n int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.s) == 0 {
		return 0, io.EOF
	}
	m := c.n
	if m > len(c.s) {
		m = len(c.s)
	}
	if m > len(p) {
		m = len(p)
	}
	copy(p, c.s[:m])
	c.s = c.s[m:]
	return m, nil
}

func mustRouteID(t *testing.T, s string) domain.RouteID {
	t.Helper()
	id, err := domain.ParseRouteID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustAccountID(t *testing.T, s string) domain.AccountID {
	t.Helper()
	id, err := domain.ParseAccountID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustKeyRef(t *testing.T, s string) domain.KeyRef {
	t.Helper()
	id, err := domain.ParseKeyRef(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// recordedRequest captures one inbound inference request for assertions.
type recordedRequest struct {
	path        string
	auth        string
	userAgent   string
	session     string
	contentType string
	accept      string
	body        string
}

const (
	testRouteChat     = "rte_AAAAAAAAAAAAAAAAAAAAAAAAAA"
	testRouteResp     = "rte_BBBBBBBBBBBBBBBBBBBBBBBBBB"
	testRouteMessages = "rte_CCCCCCCCCCCCCCCCCCCCCCCCCC"
	testRouteGemini   = "rte_DDDDDDDDDDDDDDDDDDDDDDDDDD"
	testAccount       = "acc_EEEEEEEEEEEEEEEEEEEEEEEEEE"
	testKey           = "key_FFFFFFFFFFFFFFFFFFFFFFFFFF"
	testSecret        = "sk-live-testsecret-abcdef1234567890"
)

// serveStream starts an httptest server that records its request and
// serves one SSE body. It returns the server and a pointer to the record.
func serveStream(t *testing.T, status int, body string, headers map[string]string) (*httptest.Server, *recordedRequest, *int) {
	t.Helper()
	var rec recordedRequest
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		raw, _ := io.ReadAll(r.Body)
		rec = recordedRequest{
			path:        r.URL.Path,
			auth:        r.Header.Get("Authorization"),
			userAgent:   r.Header.Get("User-Agent"),
			session:     r.Header.Get(SessionHeader),
			contentType: r.Header.Get("Content-Type"),
			accept:      r.Header.Get("Accept"),
			body:        string(raw),
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &rec, &hits
}

func baseGateway(t *testing.T, routeID domain.RouteID, cfg RouteConfig, now int64) *Gateway {
	t.Helper()
	accountID := mustAccountID(t, testAccount)
	key := mustKeyRef(t, testKey)
	return &Gateway{
		Routes:   map[domain.RouteID]RouteConfig{routeID: cfg},
		Accounts: map[domain.AccountID]AccountConfig{accountID: {KeyRefs: []domain.KeyRef{key}}},
		Secrets:  MapSecrets{key: testSecret},
		Clock:    stubClock{now: now},
	}
}

func baseRequest(routeID domain.RouteID, accountID domain.AccountID) domain.ModelRequest {
	return domain.ModelRequest{
		RouteID:    routeID,
		AccountID:  accountID,
		Messages:   []domain.Message{{Role: domain.RoleUser, Content: "hello"}},
		MaxTokens:  64,
		RequestID:  "req-1",
		DeadlineMs: 1_800_000_000_000,
	}
}

func envelopeOf(t *testing.T, err error) domain.ErrorEnvelope {
	t.Helper()
	var env domain.ErrorEnvelope
	if !errors.As(err, &env) {
		t.Fatalf("expected domain.ErrorEnvelope, got %T: %v", err, err)
	}
	return env
}

func TestChatFamilyEndToEnd(t *testing.T) {
	routeID := mustRouteID(t, testRouteChat)
	accountID := mustAccountID(t, testAccount)
	srv, rec, hits := serveStream(t, 200, loadFixture(t, "chat-stream.sse"), nil)
	gw := baseGateway(t, routeID, RouteConfig{Product: ProductConsole, Protocol: ProtocolChat, Model: "glow-free", BaseURL: srv.URL}, 1000)
	gw.SessionID = func() string { return "ses-test-1" }

	resp, err := gw.Request(context.Background(), baseRequest(routeID, accountID))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "Hello world" {
		t.Fatalf("text = %q", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_abc1" || tc.Name != "world_lookup" || string(tc.Arguments) != `{"path":"/home/alice"}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if resp.FinishReason != domain.FinishReasonToolCalls {
		t.Fatalf("finish = %q", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 120 || resp.Usage.CompletionTokens != 34 || resp.Usage.TotalTokens != 154 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if resp.RouteID != routeID || resp.AccountID != accountID || resp.RequestID != "req-1" {
		t.Fatalf("identity echo = %+v", resp)
	}
	if *hits != 1 {
		t.Fatalf("hits = %d", *hits)
	}
	// Wire assertions: exact model, streaming shape, per-request
	// credential, honest identity, session hook, chat suffix.
	if !strings.HasSuffix(rec.path, "/chat/completions") {
		t.Fatalf("path = %q", rec.path)
	}
	for _, want := range []string{`"model":"glow-free"`, `"stream":true`} {
		if !strings.Contains(rec.body, want) {
			t.Fatalf("wire body missing %s: %s", want, rec.body)
		}
	}
	if rec.auth != "Bearer "+testSecret {
		t.Fatalf("auth = %q", rec.auth)
	}
	if rec.userAgent != ClientIdentity {
		t.Fatalf("user-agent = %q", rec.userAgent)
	}
	if rec.session != "ses-test-1" {
		t.Fatalf("session header = %q", rec.session)
	}
	if rec.contentType != "application/json" || rec.accept != "text/event-stream" {
		t.Fatalf("headers = %+v", rec)
	}
}

func TestResponsesFamilyEndToEnd(t *testing.T) {
	routeID := mustRouteID(t, testRouteResp)
	accountID := mustAccountID(t, testAccount)
	srv, rec, _ := serveStream(t, 200, loadFixture(t, "responses-stream.sse"), nil)
	gw := baseGateway(t, routeID, RouteConfig{Product: ProductConsole, Protocol: ProtocolResponses, Model: "rp-model", BaseURL: srv.URL}, 1000)

	resp, err := gw.Request(context.Background(), baseRequest(routeID, accountID))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "Hello world" {
		t.Fatalf("text = %q", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "fc_1" || tc.Name != "world_lookup" || string(tc.Arguments) != `{"path":"/home/alice"}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if resp.FinishReason != domain.FinishReasonStop {
		t.Fatalf("finish = %q", resp.FinishReason)
	}
	if resp.Usage.TotalTokens != 154 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if !strings.HasSuffix(rec.path, "/responses") {
		t.Fatalf("path = %q", rec.path)
	}
	if strings.HasSuffix(rec.path, "/chat/completions") {
		t.Fatalf("responses model hit chat endpoint %q", rec.path)
	}
	// Session hook unset: header omitted, never empty-sent.
	if rec.session != "" {
		t.Fatalf("session header = %q, want omitted", rec.session)
	}
}

func TestMessagesFamilyEndToEnd(t *testing.T) {
	routeID := mustRouteID(t, testRouteMessages)
	accountID := mustAccountID(t, testAccount)
	srv, rec, _ := serveStream(t, 200, loadFixture(t, "anthropic-stream.sse"), nil)
	gw := baseGateway(t, routeID, RouteConfig{Product: ProductGo, Protocol: ProtocolMessages, Model: "claude-thing", BaseURL: srv.URL}, 1000)

	resp, err := gw.Request(context.Background(), baseRequest(routeID, accountID))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "Hello world" {
		t.Fatalf("text = %q", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Name != "world_lookup" || string(tc.Arguments) != `{"path":"/home/alice"}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if resp.FinishReason != domain.FinishReasonToolCalls {
		t.Fatalf("finish = %q", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 120 || resp.Usage.CompletionTokens != 34 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if !strings.HasSuffix(rec.path, "/messages") {
		t.Fatalf("path = %q", rec.path)
	}
}

func TestGeminiFamilyEndToEnd(t *testing.T) {
	routeID := mustRouteID(t, testRouteGemini)
	accountID := mustAccountID(t, testAccount)
	srv, rec, _ := serveStream(t, 200, loadFixture(t, "gemini-stream.sse"), nil)
	gw := baseGateway(t, routeID, RouteConfig{Product: ProductGo, Protocol: ProtocolGemini, Model: "gem-flash", BaseURL: srv.URL}, 1000)

	resp, err := gw.Request(context.Background(), baseRequest(routeID, accountID))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "Hello world" {
		t.Fatalf("text = %q", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 0 {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	if resp.FinishReason != domain.FinishReasonStop {
		t.Fatalf("finish = %q", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 120 || resp.Usage.CompletionTokens != 34 || resp.Usage.TotalTokens != 154 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if !strings.HasSuffix(rec.path, "/models:streamGenerateContent") {
		t.Fatalf("path = %q", rec.path)
	}
}

func TestUnknownProtocolNeverHitsChat(t *testing.T) {
	routeID := mustRouteID(t, testRouteChat)
	accountID := mustAccountID(t, testAccount)
	srv, _, hits := serveStream(t, 500, "boom", nil)
	gw := baseGateway(t, routeID, RouteConfig{Product: ProductConsole, Protocol: "", Model: "mystery", BaseURL: srv.URL}, 1000)

	_, err := gw.Request(context.Background(), baseRequest(routeID, accountID))
	env := envelopeOf(t, err)
	if env.Class != domain.FailureInvalidArguments {
		t.Fatalf("class = %s", env.Class)
	}
	if *hits != 0 {
		t.Fatalf("unclassified protocol reached the wire (%d hits)", *hits)
	}
}

func TestUnknownRouteAndAccount(t *testing.T) {
	routeID := mustRouteID(t, testRouteChat)
	accountID := mustAccountID(t, testAccount)
	gw := baseGateway(t, routeID, RouteConfig{Product: ProductConsole, Protocol: ProtocolChat, Model: "m", BaseURL: "http://127.0.0.1:1"}, 1000)

	otherRoute, _ := domain.ParseRouteID("rte_ZZZZZZZZZZZZZZZZZZZZZZZZZZ")
	_, err := gw.Request(context.Background(), baseRequest(otherRoute, accountID))
	if env := envelopeOf(t, err); env.Class != domain.FailureModelNotFound {
		t.Fatalf("unknown route class = %s", env.Class)
	}

	otherAccount, _ := domain.ParseAccountID("acc_ZZZZZZZZZZZZZZZZZZZZZZZZZZ")
	_, err = gw.Request(context.Background(), baseRequest(routeID, otherAccount))
	if env := envelopeOf(t, err); env.Class != domain.FailureInvalidCredential {
		t.Fatalf("unknown account class = %s", env.Class)
	}

	empty := baseRequest(routeID, accountID)
	empty.Messages = nil
	_, err = gw.Request(context.Background(), empty)
	if env := envelopeOf(t, err); env.Class != domain.FailureInvalidArguments {
		t.Fatalf("empty messages class = %s", env.Class)
	}
}

func TestMissingSecretResolverIsCredentialFailure(t *testing.T) {
	routeID := mustRouteID(t, testRouteChat)
	accountID := mustAccountID(t, testAccount)
	srv, _, hits := serveStream(t, 200, loadFixture(t, "chat-stream.sse"), nil)
	gw := baseGateway(t, routeID, RouteConfig{Product: ProductConsole, Protocol: ProtocolChat, Model: "m", BaseURL: srv.URL}, 1000)
	gw.Secrets = MapSecrets{} // configured resolver knows no keys

	_, err := gw.Request(context.Background(), baseRequest(routeID, accountID))
	if env := envelopeOf(t, err); env.Class != domain.FailureInvalidCredential {
		t.Fatalf("class = %s", env.Class)
	}
	if *hits != 0 {
		t.Fatalf("request without credentials reached the wire")
	}
}

func TestHTTP200WithErrorBodyIsQuotaFailure(t *testing.T) {
	routeID := mustRouteID(t, testRouteChat)
	accountID := mustAccountID(t, testAccount)
	srv, _, _ := serveStream(t, 200, loadFixture(t, "error-200-envelope.json"), nil)
	for _, proto := range []Protocol{ProtocolChat, ProtocolResponses, ProtocolMessages, ProtocolGemini} {
		gw := baseGateway(t, routeID, RouteConfig{Product: ProductConsole, Protocol: proto, Model: "m", BaseURL: srv.URL}, 1000)
		_, err := gw.Request(context.Background(), baseRequest(routeID, accountID))
		env := envelopeOf(t, err)
		if env.Class != domain.FailureQuotaExhausted {
			t.Fatalf("%s: class = %s", proto, env.Class)
		}
		// The configured live secret must never surface in diagnostics.
		if strings.Contains(env.Message+env.RawError, testSecret) {
			t.Fatalf("%s: secret leaked into envelope", proto)
		}
	}
}

func TestStatusErrorMapping(t *testing.T) {
	routeID := mustRouteID(t, testRouteChat)
	accountID := mustAccountID(t, testAccount)
	cases := []struct {
		name    string
		status  int
		fixture string
		class   domain.FailureClass
	}{
		{"rate limit", 429, "error-openai-429.json", domain.FailureRateLimited},
		{"auth failure", 401, "error-gemini-400.json", domain.FailureInvalidCredential},
		{"quota envelope", 402, "error-200-envelope.json", domain.FailureQuotaExhausted},
		{"bad request", 400, "error-anthropic-400.json", domain.FailureInvalidArguments},
		{"outage bare", 500, "", domain.FailureProviderOutage},
		{"removed bare", 404, "", domain.FailureModelNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := ""
			if c.fixture != "" {
				body = loadFixture(t, c.fixture)
			}
			srv, _, _ := serveStream(t, c.status, body, nil)
			gw := baseGateway(t, routeID, RouteConfig{Product: ProductConsole, Protocol: ProtocolChat, Model: "m", BaseURL: srv.URL}, 1000)
			_, err := gw.Request(context.Background(), baseRequest(routeID, accountID))
			env := envelopeOf(t, err)
			if env.Class != c.class {
				t.Fatalf("class = %s, want %s (%s)", env.Class, c.class, env.Message)
			}
			if env.RouteID == nil || env.AccountID == nil {
				t.Fatalf("envelope missing route/account scope: %+v", env)
			}
		})
	}
}

func TestRetryAfterPropagatesMilliseconds(t *testing.T) {
	routeID := mustRouteID(t, testRouteChat)
	accountID := mustAccountID(t, testAccount)
	srv, _, _ := serveStream(t, 429, loadFixture(t, "error-openai-429.json"), map[string]string{"Retry-After": "2"})
	gw := baseGateway(t, routeID, RouteConfig{Product: ProductConsole, Protocol: ProtocolChat, Model: "m", BaseURL: srv.URL}, 1000)

	_, err := gw.Request(context.Background(), baseRequest(routeID, accountID))
	env := envelopeOf(t, err)
	if env.Class != domain.FailureRateLimited {
		t.Fatalf("class = %s", env.Class)
	}
	if env.RetryAfter == nil || *env.RetryAfter != 2000 {
		t.Fatalf("retry_after = %+v", env.RetryAfter)
	}
}

func TestNoCredentialMaterialInErrors(t *testing.T) {
	routeID := mustRouteID(t, testRouteChat)
	accountID := mustAccountID(t, testAccount)
	secrets := []string{
		"sk-ant-testsecretkey1234567890",
		"sk-test-openai-secret-abcdef123456",
		"Bearer opencode-test-token-xyz987654321",
		"api_key=testsecretvalue123",
		"x-api-key: test-header-secret-999",
		"https://example.test/v1/chat?key=testquerysecret123",
	}
	for _, secret := range secrets {
		body := `{"error":{"message":"request failed with credential ` + secret + ` rejected","type":"authentication_error"}}`
		srv, _, _ := serveStream(t, 401, body, nil)
		gw := baseGateway(t, routeID, RouteConfig{Product: ProductConsole, Protocol: ProtocolChat, Model: "m", BaseURL: srv.URL}, 1000)
		_, err := gw.Request(context.Background(), baseRequest(routeID, accountID))
		env := envelopeOf(t, err)
		rendered := env.Message + "|" + env.RawError
		for _, frag := range []string{"testsecretkey1234567890", "testsecretvalue123", "test-header-secret-999", "testquerysecret123", "test-token-xyz987654321", "abcdef123456"} {
			if strings.Contains(rendered, frag) {
				t.Fatalf("credential fragment leaked for input %q: %q", secret, rendered)
			}
		}
		if !strings.Contains(rendered, "[REDACTED]") {
			t.Fatalf("expected redaction marker for input %q: %q", secret, rendered)
		}
	}
	if got := Redact("model_not_found: no such model gpt-9"); got != "model_not_found: no such model gpt-9" {
		t.Fatalf("over-redaction: %q", got)
	}
}

func TestMalformedJSONPerFamilyIsInvalidResponse(t *testing.T) {
	routeID := mustRouteID(t, testRouteChat)
	accountID := mustAccountID(t, testAccount)
	bodies := map[Protocol]string{
		ProtocolChat:      "data: {not json}\n\n",
		ProtocolResponses: "event: response.output_text.delta\ndata: {oops\n\n",
		ProtocolMessages:  "event: content_block_delta\ndata: [1,2\n\n",
		ProtocolGemini:    "data: {\"candidates\": \n\n",
	}
	for proto, body := range bodies {
		srv, _, _ := serveStream(t, 200, body, nil)
		gw := baseGateway(t, routeID, RouteConfig{Product: ProductConsole, Protocol: proto, Model: "m", BaseURL: srv.URL}, 1000)
		_, err := gw.Request(context.Background(), baseRequest(routeID, accountID))
		env := envelopeOf(t, err)
		if env.Class != domain.FailureInvalidResponse {
			t.Fatalf("%s: class = %s (%s)", proto, env.Class, env.Message)
		}
	}
}

func TestTruncatedStreamIsNetworkTimeout(t *testing.T) {
	routeID := mustRouteID(t, testRouteChat)
	accountID := mustAccountID(t, testAccount)
	full := loadFixture(t, "chat-stream.sse")
	mid := strings.Index(full, `" world"`) + 2
	if mid <= 2 {
		t.Fatal("fixture changed: anchor missing")
	}
	srv, _, _ := serveStream(t, 200, full[:mid], nil)
	gw := baseGateway(t, routeID, RouteConfig{Product: ProductConsole, Protocol: ProtocolChat, Model: "m", BaseURL: srv.URL}, 1000)

	_, err := gw.Request(context.Background(), baseRequest(routeID, accountID))
	env := envelopeOf(t, err)
	if env.Class != domain.FailureNetworkTimeout {
		t.Fatalf("class = %s (%s)", env.Class, env.Message)
	}
}

func TestCancelledRequestIsUserCancelled(t *testing.T) {
	routeID := mustRouteID(t, testRouteChat)
	accountID := mustAccountID(t, testAccount)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	t.Cleanup(srv.Close)
	gw := baseGateway(t, routeID, RouteConfig{Product: ProductConsole, Protocol: ProtocolChat, Model: "m", BaseURL: srv.URL}, 1000)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := gw.Request(ctx, baseRequest(routeID, accountID))
	env := envelopeOf(t, err)
	if env.Class != domain.FailureUserCancelled {
		t.Fatalf("class = %s (%s)", env.Class, env.Message)
	}
	if !errors.Is(err, domain.FailureUserCancelled) {
		t.Fatalf("envelope does not match FailureUserCancelled via errors.Is")
	}
}

func TestExpiredDeadlineNeverHitsWire(t *testing.T) {
	routeID := mustRouteID(t, testRouteChat)
	accountID := mustAccountID(t, testAccount)
	srv, _, hits := serveStream(t, 200, loadFixture(t, "chat-stream.sse"), nil)
	gw := baseGateway(t, routeID, RouteConfig{Product: ProductConsole, Protocol: ProtocolChat, Model: "m", BaseURL: srv.URL}, 5000)

	req := baseRequest(routeID, accountID)
	req.DeadlineMs = 4000 // already past against the stub clock
	_, err := gw.Request(context.Background(), req)
	if env := envelopeOf(t, err); env.Class != domain.FailureNetworkTimeout {
		t.Fatalf("class = %s", env.Class)
	}
	if *hits != 0 {
		t.Fatalf("expired deadline reached the wire")
	}
}

func TestBoundedBodiesAndEvents(t *testing.T) {
	routeID := mustRouteID(t, testRouteChat)
	accountID := mustAccountID(t, testAccount)
	full := loadFixture(t, "chat-stream.sse")
	for _, tc := range []struct {
		name   string
		bounds Bounds
	}{
		{"body", Bounds{MaxBodyBytes: 32}},
		{"frame", Bounds{MaxFrameBytes: 16}},
		{"events", Bounds{MaxEvents: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := serveStream(t, 200, full, nil)
			gw := baseGateway(t, routeID, RouteConfig{Product: ProductConsole, Protocol: ProtocolChat, Model: "m", BaseURL: srv.URL}, 1000)
			gw.Bounds = tc.bounds
			_, err := gw.Request(context.Background(), baseRequest(routeID, accountID))
			if env := envelopeOf(t, err); env.Class != domain.FailureInvalidResponse {
				t.Fatalf("class = %s (%s)", env.Class, env.Message)
			}
		})
	}
}

// TestDefaultEventBoundCoversAReasoningSizedStream proves the default event
// bound admits a long streamed response. A reasoning model emits its chain of
// thought and the artifact in many small frames, so a generation-sized answer
// can exceed the former 4096-event default and must not be rejected as a
// runaway stream.
func TestDefaultEventBoundCoversAReasoningSizedStream(t *testing.T) {
	const deltas = 6000 // above the former 4096 default
	var b strings.Builder
	for i := 0; i < deltas; i++ {
		b.WriteString(`data: {"choices":[{"delta":{"content":"x"},"index":0}]}` + "\n\n")
	}
	b.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}]}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")

	res, err := DecodeStream(context.Background(), ProtocolChat, strings.NewReader(b.String()), Bounds{})
	if err != nil {
		t.Fatalf("DecodeStream with the default bounds: %v", err)
	}
	if len(res.Text) != deltas {
		t.Fatalf("decoded text length = %d, want %d", len(res.Text), deltas)
	}
	if res.FinishReason != FinishStop {
		t.Fatalf("finish reason = %q, want stop", res.FinishReason)
	}
}

func TestSplitFramesDecodeAtfinestGranularity(t *testing.T) { // One byte per Read across every family: framing correctness never
	// depends on transport segmentation.
	cases := map[Protocol]string{
		ProtocolChat:      "chat-stream.sse",
		ProtocolResponses: "responses-stream.sse",
		ProtocolMessages:  "anthropic-stream.sse",
		ProtocolGemini:    "gemini-stream.sse",
	}
	for proto, fixture := range cases {
		r := &chunkReader{s: loadFixture(t, fixture), n: 1}
		res, err := DecodeStream(context.Background(), proto, r, Bounds{})
		if err != nil {
			t.Fatalf("%s: %v", proto, err)
		}
		if res.Text != "Hello world" {
			t.Fatalf("%s: text = %q", proto, res.Text)
		}
	}
}

func TestUnknownEventTypesIgnored(t *testing.T) {
	body := "event: frobnicate-future-v9\ndata: {\"whatever\":true}\n\n" + loadFixture(t, "responses-stream.sse")
	res, err := DecodeStream(context.Background(), ProtocolResponses, strings.NewReader(body), Bounds{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Hello world" {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestEndpointSeparationPerProtocol(t *testing.T) {
	chat, err := EndpointFor(ProductConsole, ProtocolChat)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Protocol{ProtocolResponses, ProtocolMessages, ProtocolGemini} {
		u, err := EndpointFor(ProductConsole, p)
		if err != nil {
			t.Fatal(err)
		}
		if u == chat {
			t.Fatalf("protocol %s resolves to chat endpoint %s", p, u)
		}
	}
	if _, err := EndpointFor(ProductConsole, ""); err == nil {
		t.Fatal("empty protocol resolved instead of failing closed")
	}
	if _, err := EndpointFor("mall", ProtocolChat); err == nil {
		t.Fatal("unknown product resolved instead of failing")
	}
}
