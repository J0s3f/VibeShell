package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/inference"
	"j0s.at/vibeshell/internal/ports"
	"j0s.at/vibeshell/internal/presentation"
)

// TestPublicIdentityIsDeterministicAndDistinct verifies that a public
// username always maps to the same durable identity and that two names never
// collide.
func TestPublicIdentityIsDeterministicAndDistinct(t *testing.T) {
	alice, err := publicIdentity("alice")
	if err != nil {
		t.Fatalf("publicIdentity(alice): %v", err)
	}
	again, err := publicIdentity("alice")
	if err != nil {
		t.Fatalf("publicIdentity(alice) second: %v", err)
	}
	if alice != again {
		t.Fatalf("publicIdentity(alice) not deterministic: %s then %s", alice, again)
	}
	if !strings.HasPrefix(alice.String(), domain.PrefixUser+"_") {
		t.Fatalf("publicIdentity did not produce a user identity: %s", alice)
	}
	bob, err := publicIdentity("bob")
	if err != nil {
		t.Fatalf("publicIdentity(bob): %v", err)
	}
	if alice == bob {
		t.Fatal("alice and bob mapped to the same identity")
	}
}

// TestHomePath verifies the visible home directory convention.
func TestHomePath(t *testing.T) {
	if got := homePath("alice"); string(got) != "/home/alice" {
		t.Fatalf("homePath(alice) = %q, want /home/alice", got)
	}
	if got := homePath("root"); string(got) != "/root" {
		t.Fatalf("homePath(root) = %q, want /root", got)
	}
}

// TestLocalEngineRespondsDeterministically verifies the fallback engine
// returns stable, truthful output and never fabricates a result for an
// unknown command.
func TestLocalEngineRespondsDeterministically(t *testing.T) {
	engine := &localEngine{
		identity:            presentationIdentity{System: "VibeOS", Shell: "VibeShell", Hostname: "vibeos", HomePrefix: "/home"},
		generationAvailable: false,
	}
	base := application.TurnRequest{
		Context: application.SessionContext{CWD: domain.MustParsePath("/home/alice")},
	}
	base.Input.Kind = application.InputCommand

	cases := []struct {
		command string
		want    string
		exit    int
	}{
		{"pwd", "/home/alice", 0},
		{"whoami", "alice", 0},
		{"echo hello world", "hello world", 0},
		{"uname", "VibeOS", 0},
	}
	for _, tc := range cases {
		req := base
		req.Input.Command = tc.command
		got, exit := engine.respond(req)
		if got != tc.want || exit != tc.exit {
			t.Errorf("respond(%q) = (%q, %d), want (%q, %d)", tc.command, got, exit, tc.want, tc.exit)
		}
	}

	req := base
	req.Input.Command = "definitely-not-a-command"
	got, exit := engine.respond(req)
	if exit != 127 {
		t.Errorf("unknown command exit = %d, want 127", exit)
	}
	if !strings.Contains(got, "unavailable") {
		t.Errorf("unknown command response = %q, want a truthful unavailable message", got)
	}
	if strings.Contains(got, "command not found") {
		t.Errorf("fallback must not claim a fabricated shell behavior: %q", got)
	}
}

// TestLocalEngineGenerateProducesValidCandidate verifies Generate returns a
// candidate the coordinator's validation accepts.
func TestLocalEngineGenerateProducesValidCandidate(t *testing.T) {
	engine := &localEngine{identity: presentationIdentity{System: "VibeOS", Hostname: "vibeos"}}
	turn, err := domain.ParseTurnID(domain.PrefixTurn + "_0123456789ABCDEFGHJKMNPQRS")
	if err != nil {
		t.Fatalf("parse turn: %v", err)
	}
	out, err := engine.Generate(context.Background(), application.GenerationInput{
		Request: application.TurnRequest{
			Turn:    turn,
			Input:   application.SessionInput{Kind: application.InputCommand, Command: "pwd"},
			Context: application.SessionContext{CWD: domain.MustParsePath("/home/alice")},
		},
	})
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}
	if out.Phase != application.StepCandidate || out.Candidate == nil {
		t.Fatalf("Generate phase = %v candidate = %v, want a candidate", out.Phase, out.Candidate)
	}
	if out.Candidate.CommitKey.Turn != turn {
		t.Fatalf("candidate commit key turn = %s, want %s", out.Candidate.CommitKey.Turn, turn)
	}
}

// TestPromptString verifies the familiar prompt and home abbreviation.
func TestPromptString(t *testing.T) {
	identity := presentation.DefaultSystemIdentity()
	if got := promptString(identity, "alice", application.PromptState{CWD: domain.MustParsePath("/home/alice")}); got != "alice@"+identity.Hostname+":~$ " {
		t.Fatalf("prompt = %q", got)
	}
	if got := promptString(identity, "root", application.PromptState{CWD: domain.MustParsePath("/")}); got != "root@"+identity.Hostname+":/# " {
		t.Fatalf("root prompt = %q", got)
	}
}

// TestPromptStringAppPrompt verifies a foreground application owns the prompt
// and that its generated text is reduced to a single printable line.
func TestPromptStringAppPrompt(t *testing.T) {
	identity := presentation.DefaultSystemIdentity()
	appID := domain.MustParseAppID("app_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	cases := []struct {
		name string
		app  application.AppPrompt
		want string
	}{
		{"declared prompt", application.AppPrompt{AppID: appID, Name: "grove", Prompt: "grove> "}, "grove> "},
		{"trailing space added", application.AppPrompt{AppID: appID, Name: "grove", Prompt: "grove>"}, "grove> "},
		{"name fallback", application.AppPrompt{AppID: appID, Name: "grove"}, "grove> "},
		{"control runes stripped", application.AppPrompt{AppID: appID, Name: "grove", Prompt: "a\r\n\x1b[31mb"}, "a[31mb "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := application.PromptState{CWD: domain.MustParsePath("/home/alice"), App: &tc.app}
			if got := promptString(identity, "alice", state); got != tc.want {
				t.Fatalf("prompt = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEndsSessionHonorsTheForegroundOwner verifies the shell's own exit words
// close the session only while the shell is foreground: a foreground
// application owns its lines, so the app's "quit" must not end the session.
func TestEndsSessionHonorsTheForegroundOwner(t *testing.T) {
	shell := application.ForegroundState{Kind: application.ForegroundShell}
	app := application.ForegroundState{Kind: application.ForegroundApp}
	cases := []struct {
		command    string
		foreground application.ForegroundState
		want       bool
	}{
		{"exit", shell, true},
		{"quit", shell, true},
		{"logout", shell, true},
		{"quit now", shell, true},
		{"exit", app, false},
		{"quit", app, false},
		{"logout", app, false},
		{"look", shell, false},
		{"look", app, false},
	}
	for _, tc := range cases {
		if got := endsSession(tc.command, tc.foreground); got != tc.want {
			t.Errorf("endsSession(%q, %s) = %v, want %v", tc.command, tc.foreground.Kind, got, tc.want)
		}
	}
}

// TestScopePolicyMapsSharing verifies the sharing configuration becomes the
// domain scope policy that gates cross-user access.
func TestScopePolicyMapsSharing(t *testing.T) {
	on := scopePolicy(&config.Config{Sharing: &config.Sharing{Enabled: true}})
	if !on.SharingEnabled {
		t.Fatal("sharing enabled in config produced a disabled policy")
	}
	off := scopePolicy(&config.Config{Sharing: &config.Sharing{Enabled: false}})
	if off.SharingEnabled {
		t.Fatal("sharing disabled in config produced an enabled policy")
	}
	if off.CrossUserReadAllowed {
		t.Fatal("sharing-off policy must deny cross-user reads")
	}
}

// TestConfigSnapshotIsValid verifies the pinned snapshot can bound a turn.
func TestConfigSnapshotIsValid(t *testing.T) {
	snapshot := buildConfigSnapshot(&config.Config{})
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("buildConfigSnapshot produced an invalid snapshot: %v", err)
	}
}

// testAccountConfig is a minimal valid configuration with exactly one
// account whose secret is resolved from the environment.
const testAccountConfig = `{
  "version": 1,
  "identity": {"system_name": "VibeOS", "shell_name": "VibeShell", "hostname": "vibeshell.test"},
  "sharing": {"enabled": false},
  "ssh": {"listen_port": 2222, "host_key_file": "/etc/vibeshell/host_key"},
  "auth": {"mode": "public"},
  "providers": [{"name": "opencode", "products": [{"name": "console", "base_url": "https://opencode.example.internal", "protocols": ["chat"], "default_protocol": "chat"}]}],
  "routes": [{"id": "rte_0123456789ABCDEFGHJKMNPQRS", "provider": "opencode", "product": "console", "model": "example-free", "protocol": "chat"}],
  "tiers": [{"name": "named-free", "routes": ["rte_0123456789ABCDEFGHJKMNPQRS"]}],
  "accounts": [{"id": "acc_0123456789ABCDEFGHJKMNPQRS", "quota_group": "team-a", "permitted_products": ["console"], "secret_ref": "{env:VIBESHELL_TEST_KEY}"}],
  "persistence": {"database_path": "/var/lib/vibeshell/world.db"}
}`

// TestGatewaySecretsResolveOnlyConfiguredAccounts verifies the gateway secret
// adapter returns resolved material for a configured account's key reference
// and refuses to invent a secret for any other reference.
func TestGatewaySecretsResolveOnlyConfiguredAccounts(t *testing.T) {
	t.Setenv("VIBESHELL_TEST_KEY", "test-secret-value")
	snapshot, err := config.NewLoader(t.TempDir(), config.Options{}).Load([]byte(testAccountConfig))
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	secrets := gatewaySecrets{snapshot: snapshot, cfg: snapshot.Config}
	ref := accountKeyRef(snapshot.Config.Accounts[0])
	if ref.IsZero() {
		t.Fatal("accountKeyRef produced a zero reference")
	}
	resolved, err := secrets.ResolveKey(ref)
	if err != nil {
		t.Fatalf("ResolveKey(configured): %v", err)
	}
	if resolved != "test-secret-value" {
		t.Fatalf("ResolveKey(configured) = %q, want the configured secret", resolved)
	}
	unknown, err := domain.ParseKeyRef(domain.PrefixKeyRef + "_0123456789ABCDEFGHJKMNPQRW")
	if err != nil {
		t.Fatalf("parse unknown key reference: %v", err)
	}
	if _, err := secrets.ResolveKey(unknown); err == nil {
		t.Fatal("expected a fail-closed error for an unconfigured key reference")
	}
}

// TestGatewaySecretsFailClosedWhenSecretUnresolved verifies the composition
// root refuses to start rather than wiring an account whose secret could not
// be resolved (PLAN 4.2 fail-closed secret references).
func TestGatewaySecretsFailClosedWhenSecretUnresolved(t *testing.T) {
	t.Setenv("VIBESHELL_TEST_KEY", "")
	if _, err := config.NewLoader(t.TempDir(), config.Options{}).Load([]byte(testAccountConfig)); err == nil {
		t.Fatal("configuration loading accepted an account with an unset secret reference")
	}
}

// TestNewFilePromptProviderRequiresConfiguredPrompt verifies a configuration
// without prompts.motd is reported instead of silently substituting a prompt.
func TestNewFilePromptProviderRequiresConfiguredPrompt(t *testing.T) {
	if _, err := newFilePromptProvider(&config.Config{}, "/etc/vibeshell"); err == nil {
		t.Fatal("expected an error when prompts.motd is absent")
	}
}

// countingGateway is a ports.ModelGateway double that reports how many requests
// reached the provider adapter behind the admission gate, and holds each request
// until the test releases it.
type countingGateway struct {
	block chan struct{}
	// arrived is closed when the first request reaches the adapter, so a test
	// can wait for admission instead of polling.
	arrived   chan struct{}
	arrivedMu sync.Once

	mu    sync.Mutex
	calls int
}

func (g *countingGateway) Request(ctx context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	g.arrivedMu.Do(func() {
		if g.arrived != nil {
			close(g.arrived)
		}
	})
	if g.block != nil {
		<-g.block
	}
	return domain.ModelResponse{
		RequestID:    req.RequestID,
		RouteID:      req.RouteID,
		AccountID:    req.AccountID,
		FinishReason: domain.FinishReasonStop,
	}, nil
}

func (g *countingGateway) reached() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// TestComposedGatewayEnforcesInferenceLimits verifies the composition root puts
// the admission gate in front of the provider gateway and that the configured
// inference limits are the ones enforced (PLAN 9.3).
func TestComposedGatewayEnforcesInferenceLimits(t *testing.T) {
	// One global slot and no wait queue: the second concurrent request must be
	// refused before it can reach the provider.
	inner := &countingGateway{block: make(chan struct{}), arrived: make(chan struct{})}
	gateway, err := buildAdmittedGateway(&config.Config{
		Inference: &config.Inference{GlobalConcurrency: 1, WaitQueueDepth: 0},
	}, inner)
	if err != nil {
		t.Fatalf("buildAdmittedGateway: %v", err)
	}
	if _, ok := gateway.(*inference.Gate); !ok {
		t.Fatalf("composed gateway = %T, want the inference gate to wrap the provider", gateway)
	}

	req := domain.ModelRequest{
		RouteID:   domain.MustParseRouteID(domain.PrefixRoute + "_0123456789ABCDEFGHJKMNPQRS"),
		AccountID: domain.MustParseAccountID(domain.PrefixAccount + "_0123456789ABCDEFGHJKMNPQRS"),
	}

	// Hold the only slot so the next request cannot be admitted.
	held := make(chan struct{})
	go func() {
		defer close(held)
		_, _ = gateway.Request(context.Background(), req)
	}()
	<-inner.arrived

	_, err = gateway.Request(context.Background(), req)
	if !errors.Is(err, inference.ErrQueueFull) {
		t.Fatalf("second request error = %v, want it to wrap %v", err, inference.ErrQueueFull)
	}
	if reached := inner.reached(); reached != 1 {
		t.Errorf("provider requests = %d, want 1: a refused request must never reach the provider", reached)
	}
	close(inner.block)
	<-held
}

// TestComposedGatewayWithoutGlobalConcurrency verifies the gate is only applied
// when the operator configured an instance-wide bound: a gate without one
// protects nothing, so the gateway is used unchanged rather than reporting a
// limit that was never configured.
func TestComposedGatewayWithoutGlobalConcurrency(t *testing.T) {
	inner := &countingGateway{}
	gateway, err := buildAdmittedGateway(&config.Config{}, inner)
	if err != nil {
		t.Fatalf("buildAdmittedGateway: %v", err)
	}
	if gateway != ports.ModelGateway(inner) {
		t.Fatalf("gateway = %T, want the undecorated provider when no global concurrency is configured", gateway)
	}
}

// TestComposedGatewayFailsStartupOnUnusableLimit verifies a configured limit the
// gate cannot honor is reported at the composition boundary rather than
// silently meaning "no limit" once requests are already running.
func TestComposedGatewayFailsStartupOnUnusableLimit(t *testing.T) {
	_, err := buildAdmittedGateway(&config.Config{
		Inference: &config.Inference{GlobalConcurrency: 1, MaxAccountConcurrency: 4},
	}, &countingGateway{})
	if err == nil {
		t.Fatal("expected startup to fail when max_account_concurrency exceeds global_concurrency")
	}
}
