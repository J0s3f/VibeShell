package application

import (
	"context"
	"testing"
)

// The tests in this file cover the line-based foreground application loop
// (plan interactive-apps-line-based, task L2): an app result that keeps running
// leaves the application foreground so the next line reaches the app, a result
// that exits returns to the shell, and end of input leaves a foreground app
// without ending the session. The scripted candidates stand for the engine's
// translation of an AppResult that sets awaiting_input or exited; building that
// translation from the domain fields is task L3.

const (
	// testAppName and testAppPrompt are the identity and continuation prompt of
	// the application the foreground tests enter.
	testAppName   = "moon-orchard"
	testAppPrompt = "> "
)

// testAppForeground is the pinned app attachment a foreground entry names.
func testAppForeground() ForegroundState {
	return ForegroundState{
		Kind:         ForegroundApp,
		AppID:        mustAppID("app_01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		AppVersionID: mustAppVersionID("av_01ARZ3NDEKTSV4RRFFQ69G5FAV"),
	}
}

// testAppPromptState is the app prompt the engine sets alongside the foreground
// entry.
func testAppPromptState() *AppPrompt {
	return &AppPrompt{AppID: testAppForeground().AppID, Name: testAppName, Prompt: testAppPrompt}
}

// foregroundTurn answers a generation with a candidate whose session patch
// moves the session to the given foreground state and carries the app prompt
// the engine sets with it.
func foregroundTurn(foreground ForegroundState, prompt *AppPrompt, text string) TurnScript {
	return TurnScript{StepFunc: func(in GenerationInput, _ int) (StepOutcome, error) {
		candidate := textCandidate(in.Request.Turn, text)
		candidate.SessionPatch = SessionPatch{Foreground: &foreground, AppPrompt: prompt}
		return candidateStep(candidate), nil
	}}
}

// acceptEOF submits end of input the way a transport adapter does.
func acceptEOF(t *testing.T, shell Shell) {
	t.Helper()
	in := SessionInput{Session: shell.ID(), Principal: shell.Principal(), Kind: InputEOF}
	if _, err := shell.Accept(context.Background(), in); err != nil {
		t.Fatalf("Accept EOF: %v", err)
	}
}

// TestAwaitingInputKeepsTheAppForegroundAndDeliversTheNextLine proves an app
// result that stays running keeps the application foreground: the emitted
// prompt carries the app prompt, and the next submitted line reaches the
// engine as an app turn whose context carries the foreground.
func TestAwaitingInputKeepsTheAppForegroundAndDeliversTheNextLine(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.engine.scriptTurn(0, foregroundTurn(testAppForeground(), testAppPromptState(), "Moon Orchard 0.3.7."))
	h.engine.scriptTurn(1, foregroundTurn(testAppForeground(), testAppPromptState(), "Silver trees; a low bright moon."))

	first, err := shell.Accept(context.Background(), commandLine(shell, 1, "moon-orchard"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if outcome := drain.awaitOutcome(t, first, waitBudget); outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want %q (failure %+v)", outcome.Kind, OutcomeCompleted, outcome.Failure)
	}
	prompt := awaitPromptOutput(t, drain.awaitOutputs(t, 2, waitBudget))
	if prompt.App == nil {
		t.Fatal("the emitted prompt carries no app prompt while an app is foreground")
	}
	if prompt.App.AppID != testAppForeground().AppID {
		t.Errorf("app prompt id = %s, want %s", prompt.App.AppID, testAppForeground().AppID)
	}
	if prompt.App.Name != testAppName || prompt.App.Prompt != testAppPrompt {
		t.Errorf("app prompt = %+v, want name %q prompt %q", prompt.App, testAppName, testAppPrompt)
	}
	if got := shell.Snapshot().Foreground; got.Kind != ForegroundApp {
		t.Fatalf("foreground = %+v, want the app to stay foreground", got)
	}

	second, err := shell.Accept(context.Background(), commandLine(shell, 2, "look"))
	if err != nil {
		t.Fatalf("Accept second line: %v", err)
	}
	if outcome := drain.awaitOutcome(t, second, waitBudget); outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want %q (failure %+v)", outcome.Kind, OutcomeCompleted, outcome.Failure)
	}
	requests := h.engine.generationsOf(1)
	if len(requests) != 1 {
		t.Fatalf("second line reached the engine in %d generations, want 1", len(requests))
	}
	if requests[0].Context.Foreground.Kind != ForegroundApp {
		t.Errorf("engine context foreground = %+v, want the app foreground", requests[0].Context.Foreground)
	}
	if requests[0].Context.Foreground.AppID != testAppForeground().AppID {
		t.Errorf("engine context foreground app = %s, want %s", requests[0].Context.Foreground.AppID, testAppForeground().AppID)
	}
	if requests[0].Input.Command != "look" {
		t.Errorf("engine input command = %q, want the submitted line", requests[0].Input.Command)
	}
}

// TestExitedResultReturnsToTheShell proves an app result that finishes moves
// the session back to the shell: the foreground clears and the emitted prompt
// no longer carries an app prompt.
func TestExitedResultReturnsToTheShell(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.engine.scriptTurn(0, foregroundTurn(testAppForeground(), testAppPromptState(), "Moon Orchard 0.3.7."))
	h.engine.scriptTurn(1, foregroundTurn(shellForeground(), nil, "The orchard sleeps."))

	first, err := shell.Accept(context.Background(), commandLine(shell, 1, "moon-orchard"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if outcome := drain.awaitOutcome(t, first, waitBudget); outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want %q (failure %+v)", outcome.Kind, OutcomeCompleted, outcome.Failure)
	}
	second, err := shell.Accept(context.Background(), commandLine(shell, 2, "quit"))
	if err != nil {
		t.Fatalf("Accept quit: %v", err)
	}
	if outcome := drain.awaitOutcome(t, second, waitBudget); outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want %q (failure %+v)", outcome.Kind, OutcomeCompleted, outcome.Failure)
	}

	prompt := awaitPromptOutput(t, drain.awaitOutputs(t, 4, waitBudget))
	if prompt.App != nil {
		t.Errorf("app prompt = %+v, want nil once the shell is foreground", prompt.App)
	}
	snapshot := shell.Snapshot()
	if snapshot.Foreground.Kind != ForegroundShell {
		t.Errorf("foreground = %+v, want the shell", snapshot.Foreground)
	}
	if snapshot.Ended {
		t.Error("an app exit must not end the session")
	}
}

// TestEOFWhileAnAppIsForegroundExitsTheApp proves end of input leaves the
// foreground application instead of ending the session: the shell prompt comes
// back without an app prompt and the session still accepts commands.
func TestEOFWhileAnAppIsForegroundExitsTheApp(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.engine.scriptTurn(0, foregroundTurn(testAppForeground(), testAppPromptState(), "Moon Orchard 0.3.7."))
	h.engine.scriptTurn(1, textTurn("ok"))

	first, err := shell.Accept(context.Background(), commandLine(shell, 1, "moon-orchard"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if outcome := drain.awaitOutcome(t, first, waitBudget); outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want %q (failure %+v)", outcome.Kind, OutcomeCompleted, outcome.Failure)
	}

	acceptEOF(t, shell)

	prompt := awaitPromptOutput(t, drain.awaitOutputs(t, 3, waitBudget))
	if prompt.App != nil {
		t.Errorf("app prompt = %+v, want nil after EOF left the app", prompt.App)
	}
	snapshot := shell.Snapshot()
	if snapshot.Ended {
		t.Fatal("EOF inside a foreground app must not end the session")
	}
	if snapshot.Foreground.Kind != ForegroundShell {
		t.Errorf("foreground = %+v, want the shell after EOF", snapshot.Foreground)
	}

	// The shell is foreground again, so a new command runs as a shell turn.
	after, err := shell.Accept(context.Background(), commandLine(shell, 2, "echo ok"))
	if err != nil {
		t.Fatalf("Accept after EOF: %v", err)
	}
	if outcome := drain.awaitOutcome(t, after, waitBudget); outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want %q (failure %+v)", outcome.Kind, OutcomeCompleted, outcome.Failure)
	}
	if requests := h.engine.generationsOf(1); len(requests) != 1 || requests[0].Context.Foreground.Kind != ForegroundShell {
		t.Errorf("post-EOF turn context foreground = %+v, want the shell", requests)
	}
}

// TestEOFAtTheShellEndsTheSession keeps the shell's own end-of-input behaviour:
// without a foreground app, EOF disconnects the session.
func TestEOFAtTheShellEndsTheSession(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)

	acceptEOF(t, shell)
	// The delivery channels close when the session goroutine has shut down, so
	// this waits for the end to complete rather than racing it.
	drain.wait(t, waitBudget)

	snapshot := shell.Snapshot()
	if !snapshot.Ended {
		t.Fatal("EOF at the shell must end the session")
	}
	if snapshot.EndReason != EndDisconnect {
		t.Errorf("end reason = %q, want %q", snapshot.EndReason, EndDisconnect)
	}
}

// TestSessionPatchCarriesTheAppPromptWithTheForeground proves the app prompt
// travels with the foreground transition at the commit point: entering the app
// stores it, an unrelated patch leaves it, and a return to the shell clears it
// even when a patch carries a prompt along.
func TestSessionPatchCarriesTheAppPromptWithTheForeground(t *testing.T) {
	state := newSessionState(mustSessionID("ses_01ARZ3NDEKTSV4RRFFQ69G5FAV"), testPrincipal, AuthModeSecure,
		TerminalMetadata{Size: DefaultTerminalSize}, "/home/alice", 1_700_000_000_000)

	app := testAppForeground()
	state.applyPatch(SessionPatch{Foreground: &app, AppPrompt: testAppPromptState()})
	if got := state.prompt().App; got == nil || got.Name != testAppName {
		t.Fatalf("prompt app = %+v, want the app prompt set with the foreground", got)
	}

	state.applyPatch(SessionPatch{})
	if state.prompt().App == nil {
		t.Fatal("an unrelated patch cleared the app prompt")
	}

	updated := &AppPrompt{AppID: testAppForeground().AppID, Name: testAppName, Prompt: "moon> "}
	state.applyPatch(SessionPatch{AppPrompt: updated})
	if got := state.prompt().App; got == nil || got.Prompt != "moon> " {
		t.Fatalf("prompt app = %+v, want the updated app prompt", got)
	}

	shell := shellForeground()
	state.applyPatch(SessionPatch{Foreground: &shell, AppPrompt: testAppPromptState()})
	if got := state.prompt().App; got != nil {
		t.Fatalf("prompt app = %+v, want nil once the shell is foreground", got)
	}
	if state.foreground.Kind != ForegroundShell {
		t.Errorf("foreground = %+v, want the shell", state.foreground)
	}
}
