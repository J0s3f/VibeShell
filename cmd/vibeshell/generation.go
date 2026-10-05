package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/apps"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
	"j0s.at/vibeshell/internal/routing"
	"j0s.at/vibeshell/internal/simulation"
)

// The generation engine is the real turn path for commands that the
// deterministic fallback cannot answer. Identity commands still come from the
// fallback (they are facts about the configured presentation, not programs),
// but an unknown program name or an applied generated program is resolved
// through the merged application lifecycle:
//
//	internal/apps.Service   register candidate -> staging sandbox validation -> activate
//	internal/adapters/sandbox  bounded QuickJS/Wasm execution (the only place generated source runs)
//	ports.ModelGateway      the configured OpenCode gateway that proposes a candidate
//
// Generated source never runs outside sandbox.Adapter. When no provider
// account is configured the engine keeps the honest "generation unavailable"
// behavior instead of fabricating an artifact.

// appCommandIndex maps a visible command name to the app that implements it.
// It is the routing table that sends a second invocation to the stored app
// rather than generating again. The index is in memory and is filled as this
// process accepts apps, so an app stored by an earlier process is not found by
// name until a durable command-name index exists; its versions and current
// pointer are durable, and re-running the command produces a new version rather
// than losing the stored one. Keeping the index behind the engine avoids
// exporting another component's internals.
type appCommandIndex struct {
	mu       sync.RWMutex
	commands map[string]domain.AppID
}

func newAppCommandIndex() *appCommandIndex {
	return &appCommandIndex{commands: map[string]domain.AppID{}}
}

// record associates every command name of an app's manifest with the app.
func (x *appCommandIndex) record(appID domain.AppID, commandNames []string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, name := range commandNames {
		x.commands[name] = appID
	}
}

// lookup returns the app currently implementing a command name.
func (x *appCommandIndex) lookup(command string) (domain.AppID, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	appID, ok := x.commands[command]
	return appID, ok
}

// seed fills the index from the durable command map, so an app stored by an
// earlier process is found by name after a restart instead of being generated
// again.
func (x *appCommandIndex) seed(commands map[string]domain.AppID) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for name, appID := range commands {
		x.commands[name] = appID
	}
}

// generationEngine implements application.TurnEngine. It wraps the fallback
// engine so the wiring change is additive: without a configured provider the
// observable behavior is identical to the fallback build.
type generationEngine struct {
	fallback *localEngine

	// apps is the generated-application lifecycle. Nil when no provider
	// account is configured, in which case no generation is attempted.
	apps     *apps.Service
	registry ports.AppRegistry
	// runner executes an app and fulfils its world reads in bounded rounds.
	runner *simulation.AppRunner
	// world resolves the principal's user namespace for those reads.
	world userNamespaceResolver
	// useTools reports whether a real tool executor is wired. When true, the
	// engine gathers world context through it before generating.
	useTools bool
	// appDeadlineMs and appMemoryB bound one generated-application execution.
	// Zero selects the documented defaults.
	appDeadlineMs int64
	appMemoryB    int64
	// generationTemplate is the operator's configured generation brief, or ""
	// for the built-in one. systemName is the configured system identity.
	generationTemplate string
	systemName         string
	index              *appCommandIndex

	executor failoverExecutor

	clock  ports.Clock
	random ports.Random
}

var _ application.TurnEngine = (*generationEngine)(nil)

// Prepare delegates to the fallback: the fallback assembles no context, and
// the generation path builds its own request at generation time.
func (e *generationEngine) Prepare(ctx context.Context, req application.TurnRequest) (application.PreparedTurn, error) {
	return e.fallback.Prepare(ctx, req)
}

// Generate resolves one accepted command. Identity facts and empty input keep
// the fallback's behavior; an unfamiliar program enters generation only when
// a provider is configured.
func (e *generationEngine) Generate(ctx context.Context, in application.GenerationInput) (application.StepOutcome, error) {
	req := in.Request
	if req.Input.Kind != application.InputCommand {
		return e.fallback.Generate(ctx, in)
	}

	// While a line-based application is foreground, every submitted line is one
	// turn of that application, whatever it looks like: the application itself
	// decides whether the line is one of its commands.
	if req.Context.Foreground.Kind == application.ForegroundApp {
		return e.runForegroundApp(ctx, req)
	}

	fields := strings.Fields(req.Input.Command)
	if len(fields) == 0 {
		return e.fallback.Generate(ctx, in)
	}
	name := fields[0]

	// Identity/facts are never generated: they describe the configured
	// presentation and are answered deterministically.
	if isIdentityCommand(name) {
		return e.fallback.Generate(ctx, in)
	}

	// The filesystem primitives are answered by the shell against the world.
	if isWorldCommand(name) {
		return e.fallback.Generate(ctx, in)
	}

	// A previously accepted app serves every later invocation.
	if appID, ok := e.index.lookup(name); ok {
		if outcome, err := e.runApp(ctx, req, appID, fields[1:]); err == nil {
			return outcome, nil
		}
		// A failed app run is reported truthfully rather than silently
		// regenerated, which would risk a duplicate activation.
		return e.candidate(req.Turn, fmt.Sprintf("vibeshell: %s: the generated program failed to run", name), 1), nil
	}

	if e.apps == nil || e.executor == nil {
		return e.fallback.Generate(ctx, in)
	}

	// Gather the working directory's world context once through the tool
	// layer, so a generated program can be told what already exists around it.
	if e.useTools && len(in.ToolResults) == 0 {
		return worldContextStep(req), nil
	}

	appID, err := e.generate(ctx, req, name, in.ToolResults)
	if err != nil {
		// Generation failed: keep the honest service message. No active
		// version exists, so a later invocation may try again.
		return e.candidate(req.Turn, fmt.Sprintf("vibeshell: %s: %s", name, err.Error()), 127), nil
	}
	if outcome, err := e.runApp(ctx, req, appID, fields[1:]); err == nil {
		return outcome, nil
	}
	return e.candidate(req.Turn, fmt.Sprintf("vibeshell: %s: the generated program could not be started", name), 1), nil
}

// runForegroundApp runs one submitted line of the application the session has
// foreground. It resolves the version the session pinned when the interaction
// started, so a later activation cannot change the running program mid-line, and
// it always produces a candidate: a failure returns the session to the shell
// rather than trapping it in a program that cannot run.
func (e *generationEngine) runForegroundApp(ctx context.Context, req application.TurnRequest) (application.StepOutcome, error) {
	foreground := req.Context.Foreground
	if e.apps == nil {
		return e.shellOutcome(req, "vibeshell: the generated program is unavailable", 1), nil
	}
	resolution, err := e.apps.ResolvePinned(ctx, apps.ResolvePinnedRequest{
		SessionID: req.Session,
		UserID:    req.Principal,
		AppID:     foreground.AppID,
		Version:   foreground.AppVersionID,
		Policy:    req.Snapshot.ScopePolicy,
	})
	if err != nil {
		return e.shellOutcome(req, fmt.Sprintf("vibeshell: the generated program stopped: %v", err), 1), nil
	}
	outcome, err := e.runResolvedApp(ctx, req, foreground.AppID, resolution, strings.Fields(req.Input.Command))
	if err != nil {
		return e.shellOutcome(req, "vibeshell: the generated program failed to run", 1), nil
	}
	return outcome, nil
}

// runApp resolves the session's version of an app, runs one input event in
// the sandbox, and returns the app's text output as a candidate.
func (e *generationEngine) runApp(ctx context.Context, req application.TurnRequest, appID domain.AppID, args []string) (application.StepOutcome, error) {
	// ResolveSession pins the current version for a new session and returns
	// the version and state to run.
	resolution, err := e.apps.ResolveSession(ctx, apps.ResolveRequest{
		SessionID: req.Session,
		UserID:    req.Principal,
		AppID:     appID,
		Policy:    req.Snapshot.ScopePolicy,
	})
	if err != nil {
		return application.StepOutcome{}, err
	}
	return e.runResolvedApp(ctx, req, appID, resolution, args)
}

// runResolvedApp executes one input event of a resolved application version and
// builds the candidate that carries its text, its exit status, and the
// foreground change its result asks for.
func (e *generationEngine) runResolvedApp(ctx context.Context, req application.TurnRequest, appID domain.AppID, resolution apps.Resolution, args []string) (application.StepOutcome, error) {
	artifact, err := e.artifact(ctx, resolution.Version)
	if err != nil {
		return application.StepOutcome{}, err
	}

	namespace, err := e.world.userNamespace(ctx, req.Principal, req.Context.Home)
	if err != nil {
		return application.StepOutcome{}, err
	}
	now := e.clock.NowUnixMilli()
	result, err := e.runner.Run(ctx, simulation.AppRunRequest{
		Artifact:     artifact,
		State:        resolution.State,
		Command:      req.Input.Command,
		Args:         args,
		CWD:          req.Context.CWD,
		Namespace:    namespace,
		NowUnixMilli: now,
		Limits:       e.runLimits(now),
	})
	if err != nil {
		return application.StepOutcome{}, err
	}

	// Advance durable state after a successful run; a save failure is not
	// fatal to the output but must not be silently ignored either.
	if saveErr := e.apps.SaveSessionState(ctx, apps.SaveStateRequest{
		SessionID: req.Session,
		UserID:    req.Principal,
		AppID:     appID,
		State:     result.NewState,
	}); saveErr != nil {
		return application.StepOutcome{}, saveErr
	}

	return application.StepOutcome{
		Phase: application.StepCandidate,
		Candidate: &application.TurnCandidate{
			CommitKey:    application.CommitKey{Turn: req.Turn, Logical: "app"},
			Output:       application.CandidateOutput{Text: appText(result)},
			ExitStatus:   result.ExitCode,
			Usage:        domain.Usage{},
			SessionPatch: appSessionPatch(appID, resolution.Version, artifact, result),
		},
	}, nil
}

// appSessionPatch is the session-state change an accepted application result
// asks for. An app awaiting input keeps the session foreground with its
// continuation prompt; a one-shot app, or one that exited, returns the session
// to the shell.
func appSessionPatch(appID domain.AppID, version domain.AppVersionID, artifact domain.AppArtifact, result domain.AppResult) application.SessionPatch {
	if !result.AwaitingInput {
		return shellPatch()
	}
	app := application.ForegroundState{Kind: application.ForegroundApp, AppID: appID, AppVersionID: version}
	return application.SessionPatch{
		Foreground: &app,
		AppPrompt:  &application.AppPrompt{AppID: appID, Name: appPromptName(artifact), Prompt: result.Prompt},
	}
}

// appPromptName is the human-readable name of a foreground application, shown
// when the app declares no prompt of its own. It prefers the primary command
// name and never leaves the prompt empty.
func appPromptName(artifact domain.AppArtifact) string {
	if len(artifact.Manifest.CommandNames) > 0 {
		return artifact.Manifest.CommandNames[0]
	}
	return artifact.AppID.String()
}

// shellPatch returns the session to the shell, dropping any app prompt.
func shellPatch() application.SessionPatch {
	shell := application.ForegroundState{Kind: application.ForegroundShell}
	return application.SessionPatch{Foreground: &shell}
}

// shellOutcome is a candidate that reports a failure and returns the session to
// the shell, so a foreground application that cannot run does not trap the user.
func (e *generationEngine) shellOutcome(req application.TurnRequest, text string, exit int) application.StepOutcome {
	return application.StepOutcome{
		Phase: application.StepCandidate,
		Candidate: &application.TurnCandidate{
			CommitKey:    application.CommitKey{Turn: req.Turn, Logical: "app"},
			Output:       application.CandidateOutput{Text: text},
			ExitStatus:   exit,
			SessionPatch: shellPatch(),
		},
	}
}

// artifact fetches an immutable artifact for a version.
func (e *generationEngine) artifact(ctx context.Context, version domain.AppVersionID) (domain.AppArtifact, error) {
	return e.registry.GetArtifact(ctx, version)
}

// generate asks the gateway for a candidate artifact, validates and activates
// it through the application lifecycle, and records its command names. It
// returns the activated app ID. An invalid candidate is never activated and
// the index is left untouched.
func (e *generationEngine) generate(ctx context.Context, req application.TurnRequest, name string, toolResults []application.ToolCallResult) (domain.AppID, error) {
	deadline := e.clock.NowUnixMilli() + generationTimeoutMs
	result, err := e.executor.Execute(ctx, routing.ExecuteRequest{
		Session:    req.Session,
		Turn:       req.Turn,
		Purpose:    domain.PurposeGeneration,
		Messages:   []domain.Message{generationPrompt(name, req, toolResults, e.generationTemplate, e.systemName)},
		MaxTokens:  generationMaxTokens,
		DeadlineMs: deadline,
		Accept: func(resp domain.ModelResponse) error {
			// A response without a usable artifact is a route failure, so the
			// router cools it and tries the next eligible model.
			_, perr := parseGenerationProposal(resp.Message.Content)
			return perr
		},
	})
	if err != nil {
		return domain.AppID{}, fmt.Errorf("model generation is unavailable: %w", err)
	}

	proposal, err := parseGenerationProposal(result.Response.Message.Content)
	if err != nil {
		return domain.AppID{}, fmt.Errorf("the generated program was not usable: %w", err)
	}

	manifest := domain.AppManifest{
		ABIVersion:   domain.AppABIVersion,
		CommandNames: proposal.CommandNames,
		Description:  proposal.Description,
		StateSchema:  json.RawMessage(`{}`),
		Capabilities: []string{domain.CapabilityTime},
		Entrypoint:   proposal.Entrypoint,
		Version:      "0.1.0",
	}
	versionID, err := e.apps.RegisterCandidate(ctx, apps.CandidateRequest{
		Manifest: manifest,
		Source:   proposal.Source,
		Owner:    req.Principal,
		Scope:    domain.ScopeUser,
		Provenance: domain.Provenance{
			Source:        "model",
			Actor:         "generation",
			PromptVersion: req.Snapshot.PromptVersion,
			ConfigVersion: fmt.Sprint(req.Snapshot.ConfigVersion),
		},
		Policy: req.Snapshot.ScopePolicy,
	})
	if err != nil {
		return domain.AppID{}, fmt.Errorf("the generated program failed validation: %w", err)
	}
	artifact, err := e.registry.GetArtifact(ctx, versionID)
	if err != nil {
		return domain.AppID{}, err
	}
	if !artifact.Validation.Passed {
		return domain.AppID{}, fmt.Errorf("the generated program failed staging validation: %s",
			strings.Join(artifact.Validation.Issues, "; "))
	}
	if _, err := e.apps.Activate(ctx, apps.ActivateRequest{
		AppID:   artifact.AppID,
		Version: versionID,
		Actor:   req.Principal,
		Policy:  req.Snapshot.ScopePolicy,
	}); err != nil {
		return domain.AppID{}, fmt.Errorf("the generated program could not be activated: %w", err)
	}
	e.index.record(artifact.AppID, manifest.CommandNames)
	for _, commandName := range manifest.CommandNames {
		// A durable-index failure must not fail a turn whose app is already
		// active: the in-memory index still serves this process, and a later
		// restart regenerates rather than corrupting anything.
		_ = e.apps.RecordCommand(ctx, commandName, artifact.AppID)
	}
	return artifact.AppID, nil
}

// candidate builds a plain-text candidate with the given exit status. It
// carries the turn so the coordinator can commit it: a zero turn is rejected
// as an invalid commit key.
func (e *generationEngine) candidate(turn domain.TurnID, text string, exit int) application.StepOutcome {
	return application.StepOutcome{
		Phase: application.StepCandidate,
		Candidate: &application.TurnCandidate{
			CommitKey:  application.CommitKey{Turn: turn, Logical: "generation"},
			Output:     application.CandidateOutput{Text: text},
			ExitStatus: exit,
		},
	}
}

// isIdentityCommand reports whether a command is a deterministic identity fact
// the fallback engine answers without generation.
func isIdentityCommand(name string) bool {
	switch name {
	case "pwd", "whoami", "uname", "echo", "env", "exit", "quit", "logout":
		return true
	default:
		return false
	}
}

// appText extracts the textual output of an app result. Generated programs
// return their display text in view.metadata.output, with status_line as the
// fallback channel.
func appText(result domain.AppResult) string {
	if len(result.View.Metadata) > 0 {
		var meta struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal(result.View.Metadata, &meta); err == nil && meta.Output != "" {
			return meta.Output
		}
	}
	return result.View.StatusLine
}

// generationTimeoutMs bounds one generation request end to end. It must cover
// the provider's reasoning time as well as the artifact: current reasoning
// models spend most of their budget thinking before emitting content, so a
// short deadline rejects otherwise valid proposals.
const generationTimeoutMs int64 = 180000

// generationMaxTokens bounds the candidate artifact a provider may propose. It
// is deliberately much larger than the artifact itself because reasoning models
// consume the same completion budget for their chain of thought; a budget sized
// only for the artifact truncates the response before any content is emitted.
// Live-observed 2026-10-05: the interactive generation prompt made
// deepseek-v4.1-flash reason for about 17.3k tokens before emitting the artifact,
// so 16384 returned empty content while 32768 returned a usable artifact.
const generationMaxTokens = 32768

// runLimits bound one generated application execution, using the configured
// bounds when set and the documented defaults otherwise.
func (e *generationEngine) runLimits(nowUnixMilli int64) ports.SandboxLimits {
	deadline := int64(5000)
	if e.appDeadlineMs > 0 {
		deadline = e.appDeadlineMs
	}
	memory := int64(32 << 20)
	if e.appMemoryB > 0 {
		memory = e.appMemoryB
	}
	return ports.SandboxLimits{
		DeadlineMs:  deadline,
		MaxMemoryB:  memory,
		MaxOutputB:  256 << 10,
		MaxEvents:   64,
		SeededRand:  0,
		SimNowMilli: nowUnixMilli,
	}
}

// generationProposal is the JSON artifact shape the model is asked to return.
type generationProposal struct {
	CommandNames []string `json:"command_names"`
	Description  string   `json:"description"`
	Entrypoint   string   `json:"entrypoint"`
	Source       string   `json:"source"`
}

// parseGenerationProposal extracts one JSON artifact from the model response.
// Reasoning models commonly prefix the artifact with a chain-of-thought block
// and/or wrap it in a fenced code block. Rather than guess at those markers,
// the artifact is located by scanning for the first JSON object that decodes
// and carries the required keys; prose, reasoning, and fences around it are
// ignored. Anything without such an object is rejected rather than guessed at.
func parseGenerationProposal(content string) (generationProposal, error) {
	text := strings.TrimSpace(content)
	for i := 0; i < len(text); {
		start := strings.IndexByte(text[i:], '{')
		if start < 0 {
			break
		}
		start += i
		var proposal generationProposal
		// Decode reads exactly one JSON value, so trailing prose after the
		// artifact is ignored while a truncated object still fails.
		if err := json.NewDecoder(strings.NewReader(text[start:])).Decode(&proposal); err == nil {
			if len(proposal.CommandNames) > 0 && proposal.Source != "" && proposal.Entrypoint != "" {
				return proposal, nil
			}
		}
		i = start + 1
	}
	return generationProposal{}, fmt.Errorf("proposal contains no usable JSON artifact")
}

// generationABI is the built-in artifact contract: the exact output shape and
// the event and world-read protocol the parser and runner require. It is always
// sent, whether or not an operator brief is configured.
const generationABI = "You write small JavaScript programs for a simulated Unix-like shell. " +
	"Return only a JSON object (no prose and no code fences) with keys: command_names (array of strings), " +
	"description (string), entrypoint (a JS function name), and source (JavaScript defining that function). " +
	"The function is called as function handle(state, event). The event is an object: event.event_type is the string " +
	"\"input\"; for a command it also carries event.command (the full command line), event.args (an array of the " +
	"arguments after the command name, possibly empty), and event.cwd (the working directory). Read event.args on " +
	"every call; never assume a fixed argument or reuse a previous result. " +
	"To read the simulated filesystem, return world_reads as an array of " +
	"{request_id: <string>, scope: \"user\", path: \"/absolute/path\"}. The shell then calls handle again with " +
	"event.event_type equal to \"world_change\" and event.payload.results (also promoted to event.results), an array of " +
	"{request_id, path, found, kind, content, entries}: kind is \"file\" or \"dir\"; content is the file's text; " +
	"entries is the directory listing as [{name, kind, size}]. On a world_change event, render the result " +
	"(for a file command, print content; for a directory command, print the entry names) and do not request the same " +
	"read again. When no world read is needed, return world_reads as an empty array. " +
	"An app is one-shot or interactive; choose one. A one-shot app returns its output and finishes. " +
	"An interactive app returns awaiting_input: true and a short prompt; the shell then keeps the app foreground and " +
	"calls handle again for each line the user types, with event.event_type \"input\" and event.line (the full line). " +
	"Return awaiting_input: true again to keep going, or exited: true to finish. Choose interactive for a REPL, game, " +
	"monitor, or wizard; choose one-shot for a filter, lister, or formatter. Either is valid. " +
	"It must return an object of exactly this shape: " +
	"{new_state: <object>, view: {mode: \"text\", status_line: <string>, metadata: {output: <string>}}, effects: [], world_reads: [], awaiting_input: <bool>, prompt: <string>, exited: <bool>}. " +
	"new_state is the state the app receives back as state on its next call; it must use the scope keys session_state (per-session and ephemeral), " +
	"user_state (durable for the user), and shared_state (durable and shared), each an object, and the app reads them as state.session_state, " +
	"state.user_state, and state.shared_state. A new_state that does not use those keys is not carried to the next call. " +
	"view.mode must be exactly the string \"text\". Put the text the user should see in view.metadata.output. " +
	"Set awaiting_input true to stay interactive (with prompt, a short line prompt such as \"> \"); set exited true to finish. " +
	"A full-screen terminal UI is reserved for a future revision; do not use it yet. " +
	"Use only plain JavaScript available in a small embedded engine: no imports, no require, no async, no host APIs. " +
	"Do not access files, the network, the environment, or the host: the program runs in an isolated simulated-world sandbox."

// generationSystemPrompt is the operator's configured generation brief, when one
// is readable and renders, followed by the built-in artifact contract.
func generationSystemPrompt(template string, req application.TurnRequest, name, systemName string) string {
	if template == "" {
		return generationABI
	}
	brief, err := renderGenerationTemplate(template, req, name, systemName)
	if err != nil || strings.TrimSpace(brief) == "" {
		return generationABI
	}
	return brief + "\n\n" + generationABI
}

// generationPrompt asks for one bounded application artifact. The command name
// is data, not an instruction: the model chooses a plausible implementation for
// a program that does not exist yet (PLAN 5.5).
func generationPrompt(name string, req application.TurnRequest, toolResults []application.ToolCallResult, template, systemName string) domain.Message {
	system := generationSystemPrompt(template, req, name, systemName)
	user := fmt.Sprintf("Write a plausible implementation of the simulated command %q invoked as %q in %s.",
		name, req.Input.Command, string(req.Context.CWD))
	if facts := worldContext(toolResults); facts != "" {
		user += "\n\nFacts about the simulated world around the command (use them; do not invent contradicting ones):\n" + facts
	}
	return domain.Message{
		Role:    domain.RoleUser,
		Content: system + "\n\n" + user,
	}
}

// worldContextStep asks the tool layer for the working directory's contents
// before generation, so the model sees a consistent world without spending a
// generation on discovery.
func worldContextStep(req application.TurnRequest) application.StepOutcome {
	args, _ := json.Marshal(map[string]string{"scope": "user", "dir": req.Context.CWD.String()})
	return application.StepOutcome{
		Phase: application.StepAwaitingTool,
		ToolCalls: []application.ToolCallRequest{{
			Name:      "world.list",
			Arguments: args,
			Scope:     domain.ScopeUser,
		}},
	}
}

// worldContext renders the bounded result of the context tool batch for the
// generation prompt. It is advisory data, never an instruction.
func worldContext(toolResults []application.ToolCallResult) string {
	var b strings.Builder
	for _, result := range toolResults {
		if result.Error != nil || len(result.Result) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(result.Name)
		b.WriteString(": ")
		b.WriteString(truncateForPrompt(string(result.Result), 2048))
	}
	return b.String()
}

// truncateForPrompt bounds one advisory string in a prompt.
func truncateForPrompt(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// buildTurnEngine assembles the real turn engine. It always returns a working
// engine: without a configured provider account it is the deterministic
// fallback, so an unconfigured build behaves exactly as before.
func buildTurnEngine(ctx context.Context, c *component) application.TurnEngine {
	shell := newWorldShell(c.db, c.db, c.world, c.clock)
	if c.router != nil {
		shell.materializer = &worldGenerator{executor: c.router, clock: c.clock}
	}
	fallback := &localEngine{
		identity:            c.identityFields(),
		generationAvailable: hasAccounts(c.cfg),
		shell:               shell,
	}
	if !hasAccounts(c.cfg) || c.router == nil || c.durable == nil || c.appService == nil {
		return fallback
	}
	index := newAppCommandIndex()
	if names, err := c.appService.CommandIndex(ctx); err == nil {
		index.seed(names)
	}
	return &generationEngine{
		fallback: fallback,
		apps:     c.appService,
		registry: c.durable.apps,
		runner: &simulation.AppRunner{
			Sandbox: c.sandbox,
			Reader:  &simulation.WorldReader{World: c.db, Content: c.db},
		},
		world:              c.world,
		useTools:           c.useTools,
		appDeadlineMs:      appLimitDeadline(c.cfg),
		appMemoryB:         appLimitMemory(c.cfg),
		generationTemplate: loadGenerationTemplate(c.cfg, c.configDir),
		systemName:         c.identity.SystemName,
		index:              index,
		executor:           c.router,
		clock:              c.clock,
		random:             c.random,
	}
}
