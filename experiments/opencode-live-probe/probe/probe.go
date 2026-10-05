// Package probe implements the E02 keyed live-probe harness for the
// OpenCode integration gate.
//
// Safety contract:
//   - Never --live: print a dry-run message, exit 0, make no network
//     request.
//   - --live without a resolvable key: print "SKIPPED: no key", exit 0,
//     make no network request.
//   - A key only ever comes from --key-env or --key-file on the command
//     line, is used only as an Authorization header value, and never
//     appears in output, errors, or receipts (everything user-facing goes
//     through gateway.Redact or never carries the value at all).
//
// When live it fetches both product catalogues (public GET), then runs one
// minimal bounded streaming probe per configured product/protocol family
// against the first eligible free model, recording status, latency, and
// tool/streaming acceptance in a dated receipt.
package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gateway "j0s.at/vibeshell/experiments/opencode-protocol"
	"j0s.at/vibeshell/experiments/opencode-protocol/catalog"
)

// ProtocolFamilies is the default probed wire-protocol set.
var ProtocolFamilies = []gateway.Protocol{
	gateway.ProtocolChat,
	gateway.ProtocolResponses,
	gateway.ProtocolMessages,
	gateway.ProtocolGemini,
}

// Config carries all probe parameters. Bases and client are injectable so
// offline tests never touch the network.
type Config struct {
	Live    bool
	KeyEnv  string // name of the env var holding the key
	KeyFile string // path to a file holding the key
	Key     func() (string, error)

	ConsoleBase  string
	GoBase       string
	MetadataPath string
	Families     []gateway.Protocol
	// Models optionally pins one model per product/protocol, skipping
	// catalogue derivation. Key: product + "/" + protocol.
	Models     map[string]string
	MinContext int

	OutDir  string
	Timeout time.Duration
	Now     func() time.Time
	Client  *http.Client
	Stdout  io.Writer
}

func (c *Config) stdout() io.Writer {
	if c.Stdout != nil {
		return c.Stdout
	}
	return os.Stdout
}

func (c *Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

func (c *Config) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: c.Timeout}
}

func (c *Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 60 * time.Second
}

// ResolveKey returns the configured key material. The result is never
// logged, printed, or stored; callers use it only as a header value.
func (c *Config) ResolveKey() (string, error) {
	if c.Key != nil {
		return c.Key()
	}
	if c.KeyEnv != "" {
		v := strings.TrimSpace(os.Getenv(c.KeyEnv))
		if v == "" {
			return "", fmt.Errorf("env var %q is empty or unset", c.KeyEnv)
		}
		return v, nil
	}
	if c.KeyFile != "" {
		b, err := os.ReadFile(c.KeyFile)
		if err != nil {
			return "", fmt.Errorf("reading key file: %w", err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("key file is empty")
		}
		return v, nil
	}
	return "", fmt.Errorf("no key source: pass --key-env or --key-file with --live")
}

// Catalogue is the recorded public-GET result for one product.
type Catalogue struct {
	Product string `json:"product"`
	URL     string `json:"url"`
	Status  int    `json:"status"`
	Bytes   int    `json:"bytes"`
	SHA256  string `json:"sha256"`
	Count   int    `json:"count,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Probe is the recorded outcome of one family probe.
type Probe struct {
	Product      string `json:"product"`
	Protocol     string `json:"protocol"`
	Model        string `json:"model"`
	Status       int    `json:"status,omitempty"`
	LatencyMs    int64  `json:"latency_ms"`
	StreamDecode bool   `json:"stream_decode_ok"`
	ToolAccepted bool   `json:"tool_params_accepted"`
	ToolCalls    int    `json:"tool_calls_observed"`
	TextChars    int    `json:"text_chars,omitempty"`
	Error        string `json:"error,omitempty"`
}

// Receipt is the dated, credential-free run record.
type Receipt struct {
	Date       string      `json:"date"`
	Mode       string      `json:"mode"`
	Catalogues []Catalogue `json:"catalogues"`
	Probes     []Probe     `json:"probes"`
	Redacted   bool        `json:"redacted"`
}

// Run executes the probe and returns the process exit code. Output never
// contains key material. Exit codes: 0 = skipped/dry-run/completed
// matrix, 1 = live run could not fetch either catalogue.
func Run(ctx context.Context, c *Config) int {
	if !c.Live {
		fmt.Fprintln(c.stdout(), "SKIPPED: dry-run; pass --live together with --key-env or --key-file to run a bounded live probe (no network request made)")
		return 0
	}
	key, err := c.ResolveKey()
	if err != nil || key == "" {
		fmt.Fprintln(c.stdout(), "SKIPPED: no key (provide one via --key-env or --key-file; never as a literal)")
		return 0
	}
	rcpt := Receipt{Date: c.now().Format(time.RFC3339), Mode: "live", Redacted: true}

	metas, err := c.loadMetadata()
	if err != nil {
		fmt.Fprintf(c.stdout(), "FAIL: loading metadata: %s\n", gateway.Redact(err.Error()))
		return 1
	}

	okProducts := 0
	idsByProduct := map[gateway.Product][]string{}
	for _, p := range []struct {
		product gateway.Product
		base    string
	}{{gateway.ProductConsole, c.consoleBase()}, {gateway.ProductGo, c.goBase()}} {
		cat := c.fetchCatalogue(ctx, p.product, p.base)
		rcpt.Catalogues = append(rcpt.Catalogues, cat.Catalogue)
		if cat.Error == "" && cat.Status == 200 {
			okProducts++
			idsByProduct[p.product] = cat.IDs
		}
	}
	if okProducts == 0 {
		fmt.Fprintln(c.stdout(), "FAIL: no product catalogue reachable; no inference probes attempted")
		return 1
	}

	for _, family := range c.families() {
		for _, product := range []gateway.Product{gateway.ProductConsole, gateway.ProductGo} {
			ids := idsByProduct[product]
			if len(ids) == 0 {
				continue
			}
			model := c.modelFor(product, family, ids, metas)
			if model == "" {
				rcpt.Probes = append(rcpt.Probes, Probe{Product: string(product), Protocol: string(family), Error: "no eligible free model with this protocol"})
				continue
			}
			rcpt.Probes = append(rcpt.Probes, c.probeOne(ctx, product, family, model, key))
		}
	}

	if c.OutDir != "" {
		if err := c.writeReceipt(rcpt); err != nil {
			fmt.Fprintf(c.stdout(), "FAIL: writing receipt: %s\n", gateway.Redact(err.Error()))
			return 1
		}
		fmt.Fprintf(c.stdout(), "receipt: %s\n", c.receiptPath())
	}
	fmt.Fprintf(c.stdout(), "live probe complete: %d catalogues, %d family probes\n", len(rcpt.Catalogues), len(rcpt.Probes))
	return 0
}

func (c *Config) consoleBase() string {
	if c.ConsoleBase != "" {
		return c.ConsoleBase
	}
	return gateway.ConsoleBase
}

func (c *Config) goBase() string {
	if c.GoBase != "" {
		return c.GoBase
	}
	return gateway.GoBase
}

func (c *Config) families() []gateway.Protocol {
	if len(c.Families) > 0 {
		return c.Families
	}
	return ProtocolFamilies
}

func (c *Config) loadMetadata() (map[gateway.Product]map[string]catalog.Meta, error) {
	if c.MetadataPath == "" {
		return nil, fmt.Errorf("no --metadata path configured")
	}
	b, err := os.ReadFile(c.MetadataPath)
	if err != nil {
		return nil, err
	}
	return catalog.LoadMetadata(b)
}

// CatalogueResult extends Catalogue with the parsed ID set (not persisted).
type CatalogueResult struct {
	Catalogue
	IDs []string
}

func (c *Config) fetchCatalogue(ctx context.Context, product gateway.Product, base string) CatalogueResult {
	url := strings.TrimRight(base, "/") + "/models"
	out := CatalogueResult{Catalogue: Catalogue{Product: string(product), URL: url}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		out.Error = gateway.Redact(err.Error())
		return out
	}
	resp, err := c.client().Do(req)
	if err != nil {
		out.Error = gateway.Redact(err.Error())
		return out
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		out.Error = gateway.Redact(err.Error())
		return out
	}
	out.Status = resp.StatusCode
	out.Bytes = len(body)
	sum := sha256.Sum256(body)
	out.SHA256 = hex.EncodeToString(sum[:8])
	if resp.StatusCode != 200 {
		out.Error = gateway.Redact(fmt.Sprintf("status %d", resp.StatusCode))
		return out
	}
	shape, err := catalog.ParseCatalogue(body)
	if err != nil {
		out.Error = gateway.Redact(err.Error())
		return out
	}
	out.Count = shape.Count
	out.IDs = shape.IDs
	return out
}

func (c *Config) modelFor(product gateway.Product, family gateway.Protocol, ids []string, metas map[gateway.Product]map[string]catalog.Meta) string {
	if c.Models != nil {
		if m, ok := c.Models[string(product)+"/"+string(family)]; ok {
			return m
		}
	}
	productMetas := metas[product]
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for _, id := range sorted {
		meta, found := productMetas[id]
		if !found {
			continue
		}
		proto := catalog.ProtocolFor(meta, found, nil)
		if proto != family {
			continue
		}
		d := catalog.SuitableFree(product, id, productMetas, catalog.Options{MinContext: c.MinContext})
		if d.Eligible {
			return id
		}
	}
	return ""
}

func (c *Config) probeOne(ctx context.Context, product gateway.Product, family gateway.Protocol, model, key string) Probe {
	endpoint, err := gateway.EndpointFor(product, family)
	if err != nil {
		return Probe{Product: string(product), Protocol: string(family), Model: model, Error: gateway.Redact(err.Error())}
	}
	if c.ConsoleBase != "" || c.GoBase != "" {
		base := c.consoleBase()
		if product == gateway.ProductGo {
			base = c.goBase()
		}
		endpoint = rewriteBase(endpoint, base, family, model)
	}
	body, err := probeBody(family, model)
	if err != nil {
		return Probe{Product: string(product), Protocol: string(family), Model: model, Error: err.Error()}
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Probe{Product: string(product), Protocol: string(family), Model: model, Error: gateway.Redact(err.Error())}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+key)
	// PLAN 8.1: identify the client honestly and send a stable session id so
	// the Go product route can route and cache efficiently.
	req.Header.Set("User-Agent", "vibeshell-live-probe/0.1")
	req.Header.Set("x-opencode-session", "vibeshell-live-probe")

	start := c.now()
	resp, err := c.client().Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return Probe{Product: string(product), Protocol: string(family), Model: model, LatencyMs: latency, Error: gateway.Redact(err.Error())}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		perr := gateway.NormalizeError(resp.StatusCode, errBody)
		msg := fmt.Sprintf("status %d", resp.StatusCode)
		if perr != nil {
			msg = perr.Error()
		}
		return Probe{Product: string(product), Protocol: string(family), Model: model, Status: resp.StatusCode, LatencyMs: latency, Error: gateway.Redact(msg)}
	}
	result, err := gateway.DecodeStream(reqCtx, family, resp.Body, nil)
	latency = time.Since(start).Milliseconds()
	p := Probe{Product: string(product), Protocol: string(family), Model: model, Status: resp.StatusCode, LatencyMs: latency}
	if err != nil {
		p.Error = gateway.Redact(err.Error())
		return p
	}
	p.StreamDecode = true
	p.ToolAccepted = true // the request carried tool parameters and was accepted
	p.ToolCalls = len(result.ToolCalls)
	p.TextChars = len(result.Text)
	return p
}

// rewriteBase swaps the baked-in product base for the configured one.
func rewriteBase(endpoint string, base string, family gateway.Protocol, model string) string {
	trimmed := strings.TrimRight(base, "/")
	switch family {
	case gateway.ProtocolChat:
		return trimmed + "/chat/completions"
	case gateway.ProtocolResponses:
		return trimmed + "/responses"
	case gateway.ProtocolMessages:
		return trimmed + "/messages"
	case gateway.ProtocolGemini:
		return trimmed + "/models/" + model + ":streamGenerateContent?alt=sse"
	}
	return endpoint
}

func probeBody(family gateway.Protocol, model string) ([]byte, error) {
	const prompt = "Reply with the single word ok."
	switch family {
	case gateway.ProtocolChat:
		return json.Marshal(map[string]any{
			"model": model, "stream": true, "max_tokens": 16,
			"messages": []map[string]string{{"role": "user", "content": prompt}},
			"tools": []map[string]any{{"type": "function", "function": map[string]any{
				"name": "noop", "description": "no-op tool for capability acceptance",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}},
		})
	case gateway.ProtocolResponses:
		return json.Marshal(map[string]any{
			"model": model, "stream": true, "max_output_tokens": 16, "input": prompt,
			"tools": []map[string]any{{"type": "function", "name": "noop",
				"description": "no-op tool for capability acceptance",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}}}},
		})
	case gateway.ProtocolMessages:
		return json.Marshal(map[string]any{
			"model": model, "stream": true, "max_tokens": 16,
			"messages": []map[string]string{{"role": "user", "content": prompt}},
			"tools": []map[string]any{{"name": "noop", "description": "no-op tool for capability acceptance",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{}}}},
		})
	case gateway.ProtocolGemini:
		return json.Marshal(map[string]any{
			"contents":         []map[string]any{{"role": "user", "parts": []map[string]string{{"text": prompt}}}},
			"generationConfig": map[string]any{"maxOutputTokens": 16},
			"tools": []map[string]any{{"functionDeclarations": []map[string]any{{
				"name": "noop", "description": "no-op tool for capability acceptance",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}},
		})
	}
	return nil, fmt.Errorf("unknown protocol %q", family)
}

func (c *Config) receiptPath() string {
	return filepath.Join(c.OutDir, c.now().Format("2006-01-02T150405")+"-live-probe.json")
}

func (c *Config) writeReceipt(r Receipt) error {
	if err := os.MkdirAll(c.OutDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.receiptPath(), b, 0o600)
}
