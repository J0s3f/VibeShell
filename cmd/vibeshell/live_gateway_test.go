package main

import (
	"context"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/adapters/opencode"
	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// liveGateway loads the operator's live configuration and builds the same
// gateway the service builds. It skips unless VIBESHELL_LIVE_CONFIG points at
// a valid configuration with a resolvable secret, so the offline suite never
// reaches a provider.
func liveGateway(t *testing.T) (ports.ModelGateway, domain.RouteID, domain.AccountID, *config.Config) {
	t.Helper()
	configPath := os.Getenv("VIBESHELL_LIVE_CONFIG")
	if configPath == "" {
		t.Skip("set VIBESHELL_LIVE_CONFIG to run the live gateway probe")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	snapshot, err := config.NewLoader(path.Dir(configPath), config.Options{}).Load(raw)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg := snapshot.Config
	gw := buildGateway(cfg, snapshot)
	route, account := firstRouteAccount(cfg)
	if route.IsZero() || account.IsZero() {
		t.Fatal("no route/account configured")
	}
	rc := gw.Routes[route]
	t.Logf("route=%s product=%s protocol=%s model=%s base=%q", route, rc.Product, rc.Protocol, rc.Model, rc.BaseURL)
	return gw, route, account, cfg
}

func liveCall(t *testing.T, gw ports.ModelGateway, route domain.RouteID, account domain.AccountID, messages []domain.Message, maxTokens int) (domain.ModelResponse, error) {
	t.Helper()
	deadline := 80 * time.Second
	if v := os.Getenv("VIBESHELL_LIVE_DEADLINE_S"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			deadline = time.Duration(n) * time.Second
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline+10*time.Second)
	defer cancel()
	return gw.Request(ctx, domain.ModelRequest{
		RouteID:    route,
		AccountID:  account,
		Messages:   messages,
		MaxTokens:  maxTokens,
		DeadlineMs: time.Now().Add(deadline).UnixMilli(),
		RequestID:  "live-probe",
	})
}

// TestLiveGatewayMOTD sends the real configured MOTD prompt through the
// composed gateway and requires a non-empty welcome.
func TestLiveGatewayMOTD(t *testing.T) {
	gw, route, account, cfg := liveGateway(t)
	motdText := "Write one short welcome line."
	if cfg.Prompts != nil && cfg.Prompts.Motd != "" {
		if b, err := os.ReadFile(cfg.Prompts.Motd); err == nil {
			motdText = string(b)
		}
	}
	resp, err := liveCall(t, gw, route, account, []domain.Message{
		{Role: domain.RoleSystem, Content: motdText},
		{Role: domain.RoleUser, Content: `{"username":"alice"}`},
	}, 256)
	if err != nil {
		t.Fatalf("live MOTD inference failed: %v", err)
	}
	t.Logf("finish=%s usage=%+v content=%q", resp.FinishReason, resp.Usage, resp.Message.Content)
	if strings.TrimSpace(resp.Message.Content) == "" {
		t.Fatalf("live MOTD returned empty content (finish=%s)", resp.FinishReason)
	}
}

// TestLiveGenerationProposal sends the real generation prompt and requires the
// model to return a proposal this build can parse and validate.
func TestLiveGenerationProposal(t *testing.T) {
	gw, route, account, _ := liveGateway(t)
	turnReq := application.TurnRequest{
		Input: application.SessionInput{
			Kind:    application.InputCommand,
			Command: "moon-orchard --interactive",
		},
		Context: application.SessionContext{CWD: domain.ValidPath("/home/alice")},
	}
	resp, err := liveCall(t, gw, route, account, []domain.Message{
		generationPrompt("moon-orchard", turnReq, nil, "", "VibeOS"),
	}, generationMaxTokens)
	if err != nil {
		t.Fatalf("live generation inference failed: %v", err)
	}
	content := resp.Message.Content
	t.Logf("finish=%s usage=%+v content_len=%d", resp.FinishReason, resp.Usage, len(content))
	if len(content) > 800 {
		content = content[:800]
	}
	t.Logf("content=%s", content)
	proposal, err := parseGenerationProposal(resp.Message.Content)
	if err != nil {
		t.Fatalf("generated proposal not usable: %v", err)
	}
	t.Logf("proposal: commands=%v entrypoint=%s source_len=%d", proposal.CommandNames, proposal.Entrypoint, len(proposal.Source))
}

// TestLiveModelMatrix runs the real MOTD prompt against every model named in
// VIBESHELL_LIVE_MODELS and reports the finish reason and content length. It
// exists to find a non-reasoning model that answers within the MOTD token
// budget; reasoning models spend the whole budget on reasoning_content and
// return no content.
func TestLiveModelMatrix(t *testing.T) {
	configPath := os.Getenv("VIBESHELL_LIVE_CONFIG")
	models := strings.Split(os.Getenv("VIBESHELL_LIVE_MODELS"), ",")
	if configPath == "" || len(models) == 0 || models[0] == "" {
		t.Skip("set VIBESHELL_LIVE_CONFIG and VIBESHELL_LIVE_MODELS to run the matrix")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	snapshot, err := config.NewLoader(path.Dir(configPath), config.Options{}).Load(raw)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg := snapshot.Config
	gw := buildGateway(cfg, snapshot)
	route, account := firstRouteAccount(cfg)
	base := gw.Routes[route]
	baseURL := base.BaseURL
	if v := os.Getenv("VIBESHELL_LIVE_BASE"); v != "" {
		baseURL = v
	}
	motdText := "Write one short welcome line."
	if cfg.Prompts != nil && cfg.Prompts.Motd != "" {
		if b, err := os.ReadFile(cfg.Prompts.Motd); err == nil {
			motdText = string(b)
		}
	}
	maxTokens := 256
	if v := os.Getenv("VIBESHELL_LIVE_MAXTOK"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxTokens = n
		}
	}
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		gw.Routes[route] = opencode.RouteConfig{
			Product: base.Product, Protocol: base.Protocol, Model: m, BaseURL: baseURL,
		}
		resp, err := liveCall(t, gw, route, account, []domain.Message{
			{Role: domain.RoleSystem, Content: motdText},
			{Role: domain.RoleUser, Content: `{"username":"alice"}`},
		}, maxTokens)
		if err != nil {
			t.Logf("%-32s ERROR %v", m, err)
			continue
		}
		t.Logf("%-32s finish=%-6s completion=%-5d content_len=%d", m, resp.FinishReason, resp.Usage.CompletionTokens, len(resp.Message.Content))
		if c := resp.Message.Content; len(c) > 0 {
			head, tail := c, c
			if len(head) > 400 {
				head = head[:400]
			}
			if len(tail) > 400 {
				tail = tail[len(tail)-400:]
			}
			t.Logf("%-32s head=%q", m, head)
			t.Logf("%-32s tail=%q", m, tail)
			t.Logf("%-32s head_hex=% x", m, []byte(head[:min(24, len(head))]))
			t.Logf("%-32s tail_hex=% x", m, []byte(tail[max(0, len(tail)-24):]))
		}
	}
}

// TestLiveGenerationMatrix runs the real generation prompt against every model
// named in VIBESHELL_LIVE_MODELS and reports whether the response parses into a
// usable proposal. It is the generation analogue of TestLiveModelMatrix.
func TestLiveGenerationMatrix(t *testing.T) {
	configPath := os.Getenv("VIBESHELL_LIVE_CONFIG")
	models := strings.Split(os.Getenv("VIBESHELL_LIVE_MODELS"), ",")
	if configPath == "" || len(models) == 0 || models[0] == "" {
		t.Skip("set VIBESHELL_LIVE_CONFIG and VIBESHELL_LIVE_MODELS to run the matrix")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	snapshot, err := config.NewLoader(path.Dir(configPath), config.Options{}).Load(raw)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg := snapshot.Config
	gw := buildGateway(cfg, snapshot)
	route, account := firstRouteAccount(cfg)
	base := gw.Routes[route]
	baseURL := base.BaseURL
	if v := os.Getenv("VIBESHELL_LIVE_BASE"); v != "" {
		baseURL = v
	}
	turnReq := application.TurnRequest{
		Input:   application.SessionInput{Kind: application.InputCommand, Command: "moon-orchard --interactive"},
		Context: application.SessionContext{CWD: domain.ValidPath("/home/alice")},
	}
	prompt := generationPrompt("moon-orchard", turnReq, nil, "", "VibeOS")
	maxTokens := generationMaxTokens
	if v := os.Getenv("VIBESHELL_LIVE_MAXTOK"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxTokens = n
		}
	}
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		gw.Routes[route] = opencode.RouteConfig{
			Product: base.Product, Protocol: base.Protocol, Model: m, BaseURL: baseURL,
		}
		resp, err := liveCall(t, gw, route, account, []domain.Message{prompt}, maxTokens)
		if err != nil {
			t.Logf("%-32s ERROR %v", m, err)
			continue
		}
		proposal, perr := parseGenerationProposal(resp.Message.Content)
		t.Logf("%-32s finish=%-6s completion=%-5d content_len=%-5d parse_err=%v commands=%v",
			m, resp.FinishReason, resp.Usage.CompletionTokens, len(resp.Message.Content), perr, proposal.CommandNames)
		if len(resp.Message.Content) > 0 {
			snippet := resp.Message.Content
			head := snippet
			if len(head) > 300 {
				head = head[:300]
			}
			tail := snippet
			if len(tail) > 300 {
				tail = tail[len(tail)-300:]
			}
			t.Logf("%-32s head=%q", m, head)
			t.Logf("%-32s tail=%q", m, tail)
			t.Logf("%-32s head_hex=% x", m, []byte(head[:min(24, len(head))]))
			t.Logf("%-32s tail_hex=% x", m, []byte(tail[max(0, len(tail)-24):]))
			if brace := strings.IndexByte(resp.Message.Content, '{'); brace >= 0 {
				start := brace - 200
				if start < 0 {
					start = 0
				}
				t.Logf("%-32s before-json=%q", m, resp.Message.Content[start:brace])
			}
		}
	}
}
