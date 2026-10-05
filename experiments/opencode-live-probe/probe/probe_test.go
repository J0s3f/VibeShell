package probe

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gateway "j0s.at/vibeshell/experiments/opencode-protocol"
)

const fakeKey = "sk-test-fake-key-12345"

// catalogueHandler serves both product catalogues and counts every hit.
func catalogueHandler(hits *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m-free","object":"model","created":1,"owned_by":"test"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

const chatSSE = `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}

data: [DONE]

`

func TestDryRunMakesNoNetwork(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(catalogueHandler(&hits))
	defer srv.Close()
	var out bytes.Buffer
	code := Run(context.Background(), &Config{
		Live: false, ConsoleBase: srv.URL, GoBase: srv.URL, Stdout: &out, Now: time.Now().UTC,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if hits.Load() != 0 {
		t.Fatalf("dry-run made %d HTTP requests, want 0", hits.Load())
	}
	if !strings.Contains(out.String(), "SKIPPED: dry-run") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestLiveWithoutKeyMakesNoNetwork(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(catalogueHandler(&hits))
	defer srv.Close()
	var out bytes.Buffer
	code := Run(context.Background(), &Config{
		Live: true, KeyEnv: "PROBE_TEST_MISSING_KEY", ConsoleBase: srv.URL, GoBase: srv.URL, Stdout: &out,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if hits.Load() != 0 {
		t.Fatalf("no-key run made %d HTTP requests, want 0", hits.Load())
	}
	if !strings.Contains(out.String(), "SKIPPED: no key") {
		t.Fatalf("output = %q", out.String())
	}
}

func writeMetaFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "meta.json")
	meta := `{"opencode":{"models":{"m-free":{"id":"m-free","tool_call":true,` +
		`"modalities":{"input":["text"],"output":["text"]},"limit":{"context":128000},` +
		`"cost":{"input":0,"output":0},"provider":{"npm":"@ai-sdk/openai"}}}}}`
	if err := os.WriteFile(p, []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLiveProbeRecordsAndRedacts(t *testing.T) {
	var hits atomic.Int64
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		sawAuth = r.Header.Get("Authorization")
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m-free","object":"model","created":1,"owned_by":"test"}]}`))
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(chatSSE))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(chatSSE))
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	var out bytes.Buffer
	t.Setenv("PROBE_TEST_KEY", fakeKey)
	code := Run(context.Background(), &Config{
		Live: true, KeyEnv: "PROBE_TEST_KEY", ConsoleBase: srv.URL, GoBase: srv.URL,
		MetadataPath: writeMetaFixture(t), OutDir: dir, Families: []gateway.Protocol{gateway.ProtocolChat},
		MinContext: 1000, Stdout: &out, Now: time.Now().UTC,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output %q", code, out.String())
	}
	if sawAuth != "Bearer "+fakeKey {
		t.Fatalf("Authorization header = %q", sawAuth)
	}
	if out.String() == "" || strings.Contains(out.String(), fakeKey) {
		t.Fatalf("stdout leaked key or empty: %q", out.String())
	}
	files, err := filepath.Glob(filepath.Join(dir, "*-live-probe.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("receipt files = %v, err %v", files, err)
	}
	b, _ := os.ReadFile(files[0])
	if strings.Contains(string(b), fakeKey) {
		t.Fatalf("receipt leaked key: %s", b)
	}
	if !strings.Contains(string(b), `"stream_decode_ok": true`) {
		t.Fatalf("receipt missing stream acceptance: %s", b)
	}
}

func TestRedactionOfKeyEchoingError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m-free","object":"model","created":1,"owned_by":"test"}]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key Bearer ` + fakeKey + `"}}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	var out bytes.Buffer
	t.Setenv("PROBE_TEST_KEY", fakeKey)
	code := Run(context.Background(), &Config{
		Live: true, KeyEnv: "PROBE_TEST_KEY", ConsoleBase: srv.URL, GoBase: srv.URL,
		MetadataPath: writeMetaFixture(t), OutDir: dir, Families: []gateway.Protocol{gateway.ProtocolChat},
		MinContext: 1000, Stdout: &out,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (failures recorded, not fatal)", code)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*-live-probe.json"))
	b, _ := os.ReadFile(files[0])
	if strings.Contains(string(b), fakeKey) {
		t.Fatalf("receipt leaked key via error: %s", b)
	}
	if strings.Contains(out.String(), fakeKey) {
		t.Fatalf("stdout leaked key: %q", out.String())
	}
}

func TestCatalogueFailureExitCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	var out bytes.Buffer
	t.Setenv("PROBE_TEST_KEY", fakeKey)
	code := Run(context.Background(), &Config{
		Live: true, KeyEnv: "PROBE_TEST_KEY", ConsoleBase: srv.URL, GoBase: srv.URL,
		MetadataPath: writeMetaFixture(t), Stdout: &out,
	})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}
