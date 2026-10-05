package gateway

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
)

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

// blockingReader blocks until ctx is done, for cancellation tests.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) {
	time.Sleep(2 * time.Second)
	return 0, io.EOF
}

// serveFixture serves body as an SSE httptest server; the client side reads
// through a chunked reader to prove split-frame tolerance end to end.
func serveFixture(t *testing.T, status int, body string, chunk int) io.Reader {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL) //nolint:gosec,noctx
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != status {
		t.Fatalf("fixture status = %d, want %d", resp.StatusCode, status)
	}
	if chunk > 0 {
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return &chunkReader{s: string(raw), n: chunk}
	}
	return resp.Body
}

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestChatDecodesTextToolCallsFinishUsage(t *testing.T) {
	r := serveFixture(t, 200, loadFixture(t, "chat-stream.sse"), 3)
	res, err := DecodeStream(context.Background(), ProtocolChat, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Hello world" {
		t.Fatalf("text = %q", res.Text)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", res.ToolCalls)
	}
	tc := res.ToolCalls[0]
	if tc.ID != "call_abc1" || tc.Name != "world_lookup" || tc.Arguments != `{"path":"/home/alice"}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if res.FinishReason != FinishToolCalls {
		t.Fatalf("finish = %q", res.FinishReason)
	}
	if res.Usage.InputTokens != 120 || res.Usage.OutputTokens != 34 || res.Usage.TotalTokens != 154 {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

func TestResponsesDecodesTextFunctionCallsUsage(t *testing.T) {
	r := serveFixture(t, 200, loadFixture(t, "responses-stream.sse"), 5)
	res, err := DecodeStream(context.Background(), ProtocolResponses, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Hello world" {
		t.Fatalf("text = %q", res.Text)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", res.ToolCalls)
	}
	tc := res.ToolCalls[0]
	if tc.ID != "fc_1" || tc.Name != "world_lookup" || tc.Arguments != `{"path":"/home/alice"}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if res.FinishReason != FinishStop {
		t.Fatalf("finish = %q", res.FinishReason)
	}
	if res.Usage.TotalTokens != 154 {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

func TestAnthropicDecodesDeltasToolUseUsage(t *testing.T) {
	r := serveFixture(t, 200, loadFixture(t, "anthropic-stream.sse"), 2)
	res, err := DecodeStream(context.Background(), ProtocolMessages, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Hello world" {
		t.Fatalf("text = %q", res.Text)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", res.ToolCalls)
	}
	tc := res.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Name != "world_lookup" || tc.Arguments != `{"path":"/home/alice"}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if res.FinishReason != FinishToolCalls {
		t.Fatalf("finish = %q", res.FinishReason)
	}
	if res.Usage.InputTokens != 120 || res.Usage.OutputTokens != 34 {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

func TestGeminiDecodesPartsFinishUsage(t *testing.T) {
	r := serveFixture(t, 200, loadFixture(t, "gemini-stream.sse"), 7)
	res, err := DecodeStream(context.Background(), ProtocolGemini, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Hello world" {
		t.Fatalf("text = %q", res.Text)
	}
	if res.FinishReason != FinishStop {
		t.Fatalf("finish = %q", res.FinishReason)
	}
	if res.Usage.InputTokens != 120 || res.Usage.OutputTokens != 34 || res.Usage.TotalTokens != 154 {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

func TestSSESplitFramesAndMultipleEventsPerRead(t *testing.T) {
	// One Read carries three events; another delivers one byte at a time.
	for _, n := range []int{1, 2, 4096} {
		r := &chunkReader{s: loadFixture(t, "chat-stream.sse"), n: n}
		res, err := DecodeStream(context.Background(), ProtocolChat, r, nil)
		if err != nil {
			t.Fatalf("chunk=%d: %v", n, err)
		}
		if res.Text != "Hello world" || len(res.ToolCalls) != 1 {
			t.Fatalf("chunk=%d: %+v", n, res)
		}
	}
}

func TestMalformedJSONIsError(t *testing.T) {
	for proto, body := range map[Protocol]string{
		ProtocolChat:      "data: {not json}\n\n",
		ProtocolResponses: "event: response.output_text.delta\ndata: {oops\n\n",
		ProtocolMessages:  "event: content_block_delta\ndata: [1,2\n\n",
		ProtocolGemini:    "data: {\"candidates\": \n\n",
	} {
		_, err := DecodeStream(context.Background(), proto, strings.NewReader(body), nil)
		if err == nil {
			t.Fatalf("%s: expected malformed JSON error", proto)
		}
	}
}

func TestHTTP200WithErrorBodyIsError(t *testing.T) {
	body := loadFixture(t, "error-200-envelope.json")
	for _, proto := range []Protocol{ProtocolChat, ProtocolResponses, ProtocolMessages, ProtocolGemini} {
		_, err := DecodeStream(context.Background(), proto, strings.NewReader(body), nil)
		var perr *ProviderError
		if !errors.As(err, &perr) {
			t.Fatalf("%s: expected *ProviderError, got %v", proto, err)
		}
		if perr.Category != CategoryQuota {
			t.Fatalf("%s: category = %s", proto, perr.Category)
		}
	}
}

func TestStreamInterruptionMidResponse(t *testing.T) {
	full := loadFixture(t, "chat-stream.sse")
	// A cut inside a JSON payload must fail, never decode partial success.
	mid := strings.Index(full, `" world"`) + 2
	if mid <= 2 {
		t.Fatal("fixture changed: anchor missing")
	}
	if _, err := DecodeStream(context.Background(), ProtocolChat, strings.NewReader(full[:mid]), nil); err == nil {
		t.Fatal("mid-payload cut: expected interruption error")
	}
	// A cleanly dropped terminal sentinel still decodes: SSE prefixes are
	// valid streams, so only mid-frame tails are detectable interruptions.
	sentinel := strings.Index(full, "data: [DONE]")
	if sentinel < 0 {
		t.Fatal("fixture changed: sentinel missing")
	}
	res, err := DecodeStream(context.Background(), ProtocolChat, strings.NewReader(full[:sentinel]), nil)
	if err != nil {
		t.Fatalf("dropped sentinel: %v", err)
	}
	if res.Text != "Hello world" {
		t.Fatalf("dropped sentinel text = %q", res.Text)
	}
}

func TestContextCancellationAbortsDecode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := DecodeStream(ctx, ProtocolChat, blockingReader{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestBoundedResponseBody(t *testing.T) {
	opts := &Options{MaxBodyBytes: 32}
	_, err := DecodeStream(context.Background(), ProtocolChat,
		strings.NewReader(loadFixture(t, "chat-stream.sse")), opts)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected body bound error, got %v", err)
	}
}

func TestOversizedFrameRejected(t *testing.T) {
	opts := &Options{MaxFrameBytes: 16}
	_, err := DecodeStream(context.Background(), ProtocolChat,
		strings.NewReader(loadFixture(t, "chat-stream.sse")), opts)
	if err == nil || !strings.Contains(err.Error(), "frame exceeds") {
		t.Fatalf("expected frame bound error, got %v", err)
	}
}

func TestUnknownEventTypesIgnored(t *testing.T) {
	body := "event: frobnicate-future-v9\ndata: {\"whatever\":true}\n\n" + loadFixture(t, "responses-stream.sse")
	res, err := DecodeStream(context.Background(), ProtocolResponses, strings.NewReader(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Hello world" {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestEndpointSeparationPerProtocol(t *testing.T) {
	chat, err := EndpointFor(gatewayProduct(), ProtocolChat)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Protocol{ProtocolResponses, ProtocolMessages, ProtocolGemini} {
		u, err := EndpointFor(gatewayProduct(), p)
		if err != nil {
			t.Fatal(err)
		}
		if u == chat {
			t.Fatalf("protocol %s resolves to chat endpoint %s", p, u)
		}
	}
}

func gatewayProduct() Product { return ProductConsole }
