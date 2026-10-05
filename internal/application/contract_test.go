package application

import (
	"context"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// The tests in this file cover the application-facing session contract an
// inbound transport adapter drives: the request and input shapes, the output and
// outcome shapes, and the Shell handle's behaviour under concurrency. They use
// only domain types, so they double as the specification B05's SSH adapter and a
// future local terminal adapter are written against.

// TestOpenSessionRequestValidation proves an unusable accept request is refused
// before any session state exists.
func TestOpenSessionRequestValidation(t *testing.T) {
	valid := OpenSessionRequest{
		Principal: testPrincipal,
		AuthMode:  AuthModeSecure,
		Terminal:  TerminalMetadata{Term: "xterm-256color", Size: DefaultTerminalSize},
		CWD:       "/home/alice",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a well-formed accept request was rejected: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(OpenSessionRequest) OpenSessionRequest
		wantErr string
	}{
		{
			name:    "unauthenticated principal",
			mutate:  func(r OpenSessionRequest) OpenSessionRequest { r.Principal = domain.UserID{}; return r },
			wantErr: domain.CodeInvalidIdentity,
		},
		{
			name:    "unknown auth mode",
			mutate:  func(r OpenSessionRequest) OpenSessionRequest { r.AuthMode = "trusted"; return r },
			wantErr: domain.CodeInvalidInput,
		},
		{
			name:    "no initial working directory",
			mutate:  func(r OpenSessionRequest) OpenSessionRequest { r.CWD = ""; return r },
			wantErr: domain.CodeInvalidPath,
		},
		{
			name: "absurd terminal size",
			mutate: func(r OpenSessionRequest) OpenSessionRequest {
				r.Terminal.Size = domain.TermSize{Cols: MaxTerminalCols + 1, Rows: 24}
				return r
			},
			wantErr: domain.CodeInvalidInput,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.mutate(valid).Validate()
			if got := errorsReported(t, err); got != testCase.wantErr {
				t.Fatalf("rejection code = %q, want %q", got, testCase.wantErr)
			}
		})
	}
}

// TestSessionInputValidation proves every accepted input kind carries what the
// application needs and nothing it must not be trusted with.
func TestSessionInputValidation(t *testing.T) {
	shell := &session{id: mustSessionID("ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")}
	size := domain.TermSize{Cols: 100, Rows: 40}
	paste := strings.Repeat("x", MaxPasteBytes+1)

	valid := []SessionInput{
		{Session: shell.id, Principal: testPrincipal, Sequence: 1, Kind: InputCommand, Command: "ls -la"},
		{Session: shell.id, Principal: testPrincipal, Sequence: 2, Kind: InputPaste, Text: "pasted"},
		{Session: shell.id, Principal: testPrincipal, Kind: InputKey, Key: "up"},
		{Session: shell.id, Principal: testPrincipal, Kind: InputResize, Size: &size},
		{Session: shell.id, Principal: testPrincipal, Kind: InputEOF},
		{Session: shell.id, Principal: testPrincipal, Kind: InputCancel},
	}
	for _, in := range valid {
		if err := in.Validate(); err != nil {
			t.Errorf("input %+v was rejected: %v", in, err)
		}
	}

	cases := []struct {
		name    string
		in      SessionInput
		wantErr string
	}{
		{"no session", SessionInput{Principal: testPrincipal, Kind: InputKey, Key: "up"}, domain.CodeInvalidIdentity},
		{"no principal", SessionInput{Session: shell.id, Kind: InputKey, Key: "up"}, domain.CodeInvalidIdentity},
		{"turn-starting input without a sequence", SessionInput{Session: shell.id, Principal: testPrincipal, Kind: InputCommand, Command: "ls"}, domain.CodeInvalidInput},
		{"empty command", SessionInput{Session: shell.id, Principal: testPrincipal, Sequence: 1, Kind: InputCommand, Command: "   "}, domain.CodeInvalidInput},
		{"oversized command", SessionInput{Session: shell.id, Principal: testPrincipal, Sequence: 1, Kind: InputCommand, Command: strings.Repeat("c", MaxCommandBytes+1)}, domain.CodeOutputTooLarge},
		{"empty paste", SessionInput{Session: shell.id, Principal: testPrincipal, Sequence: 1, Kind: InputPaste}, domain.CodeInvalidInput},
		{"oversized paste", SessionInput{Session: shell.id, Principal: testPrincipal, Sequence: 1, Kind: InputPaste, Text: paste}, domain.CodeOutputTooLarge},
		{"key without a key", SessionInput{Session: shell.id, Principal: testPrincipal, Kind: InputKey}, domain.CodeInvalidInput},
		{"resize without a size", SessionInput{Session: shell.id, Principal: testPrincipal, Kind: InputResize}, domain.CodeInvalidInput},
		{"resize out of bounds", SessionInput{Session: shell.id, Principal: testPrincipal, Kind: InputResize, Size: &domain.TermSize{Cols: 10, Rows: MaxTerminalRows + 1}}, domain.CodeInvalidInput},
		{"unknown kind", SessionInput{Session: shell.id, Principal: testPrincipal, Kind: "keystroke"}, domain.CodeInvalidInput},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := errorsReported(t, testCase.in.Validate()); got != testCase.wantErr {
				t.Fatalf("rejection code = %q, want %q", got, testCase.wantErr)
			}
		})
	}
}

// TestOnlySemanticInputStartsATurn keeps control traffic out of the turn
// lifecycle: a resize, an EOF, and a Ctrl-C must not create turns.
func TestOnlySemanticInputStartsATurn(t *testing.T) {
	shell := &session{id: mustSessionID("ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")}
	size := domain.TermSize{Cols: 80, Rows: 24}
	base := SessionInput{Session: shell.id, Principal: testPrincipal}

	if !(SessionInput{Kind: InputCommand, Command: "ls"}).StartsTurn() {
		t.Error("a command must start a turn")
	}
	if !(SessionInput{Kind: InputPaste, Text: "x"}).StartsTurn() {
		t.Error("a paste must start a turn")
	}
	for _, kind := range []InputKind{InputKey, InputResize, InputEOF, InputCancel} {
		if (SessionInput{Kind: kind, Size: &size}).StartsTurn() {
			t.Errorf("input kind %q must not start a turn", kind)
		}
	}
	if !(SessionInput{Session: base.Session, Principal: base.Principal, Kind: InputCommand, Command: "ls"}).StartsTurn() {
		t.Error("a command carrying a session and principal must still start a turn")
	}
}

// TestNormalizeInputMapsControlKeysToCancellation proves Ctrl-C and an explicit
// interrupt signal become one application intent, so a transport cannot invent
// a second cancellation path.
func TestNormalizeInputMapsControlKeysToCancellation(t *testing.T) {
	shell := &session{id: mustSessionID("ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")}
	for _, key := range []string{KeyCtrlC, KeyInterrupt} {
		normalized, err := normalizeInput(SessionInput{Session: shell.id, Principal: testPrincipal, Kind: InputKey, Key: key})
		if err != nil {
			t.Fatalf("key %q was rejected: %v", key, err)
		}
		if normalized.Kind != InputCancel {
			t.Errorf("key %q became kind %q, want %q", key, normalized.Kind, InputCancel)
		}
		if normalized.Key != key {
			t.Errorf("normalization dropped the reported key: %q became %q", key, normalized.Key)
		}
	}

	other, err := normalizeInput(SessionInput{Session: shell.id, Principal: testPrincipal, Kind: InputKey, Key: "up"})
	if err != nil {
		t.Fatalf("an ordinary key was rejected: %v", err)
	}
	if other.Kind != InputKey {
		t.Errorf("an ordinary key became kind %q", other.Kind)
	}
}

// TestForegroundStateValidation proves an interactive attachment must name the
// accepted app version it pins.
func TestForegroundStateValidation(t *testing.T) {
	if err := shellForeground().Validate(); err != nil {
		t.Fatalf("the shell foreground state must be valid: %v", err)
	}

	appID := mustAppID("app_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	version := mustAppVersionID("av_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err := (ForegroundState{Kind: ForegroundApp, AppID: appID, AppVersionID: version}).Validate(); err != nil {
		t.Fatalf("a pinned app attachment must be valid: %v", err)
	}

	cases := map[string]ForegroundState{
		"unknown kind":        {Kind: "detached"},
		"app without version": {Kind: ForegroundApp, AppID: appID},
		"version without app": {Kind: ForegroundApp, AppVersionID: version},
	}
	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			if err := state.Validate(); err == nil {
				t.Fatalf("foreground state %+v was accepted", state)
			}
		})
	}
}

// TestSessionStateAppliesOnlyAcceptedChanges proves a session patch moves only
// the fields it names, so an unchanged working directory cannot drift.
func TestSessionStateAppliesOnlyAcceptedChanges(t *testing.T) {
	state := newSessionState(mustSessionID("ses_01ARZ3NDEKTSV4RRFFQ69G5FAV"), testPrincipal, AuthModeSecure, TerminalMetadata{Size: DefaultTerminalSize}, "/home/alice", 1_700_000_000_000)

	empty := SessionPatch{}
	state.applyPatch(empty)
	if state.cwd != "/home/alice" {
		t.Errorf("an empty patch moved the working directory to %q", state.cwd)
	}

	moved := domain.ValidPath("/home/alice/work")
	status := 3
	state.applyPatch(SessionPatch{CWD: &moved, ExitStatus: &status})
	if state.cwd != moved {
		t.Errorf("working directory = %q, want %q", state.cwd, moved)
	}
	if state.exitStatus != status {
		t.Errorf("exit status = %d, want %d", state.exitStatus, status)
	}
	if prompt := state.prompt(); prompt.CWD != moved || prompt.ExitCode != status {
		t.Errorf("prompt = %+v, want cwd %q and exit code %d", prompt, moved, status)
	}

	// A resize is session-local state too.
	state.resize(domain.TermSize{Cols: 120, Rows: 50})
	if state.context().TerminalSize.Cols != 120 {
		t.Errorf("terminal size = %+v, want 120 columns", state.context().TerminalSize)
	}
}

// TestTerminalMetadataCarriesNoClientEnvironment proves the contract has no
// field through which a client could smuggle environment into the application.
func TestTerminalMetadataCarriesNoClientEnvironment(t *testing.T) {
	metadata := TerminalMetadata{
		Term:          "xterm-256color",
		Size:          domain.TermSize{Cols: 80, Rows: 24},
		ClientAddr:    "198.51.100.7:51000",
		ClientVersion: "SSH-2.0-OpenSSH_9.6",
	}
	if err := validateTerminalMetadata(metadata); err != nil {
		t.Fatalf("reported terminal metadata was rejected: %v", err)
	}
	// A zero size is legal at validation time and replaced at accept time.
	if err := validateTerminalMetadata(TerminalMetadata{}); err != nil {
		t.Fatalf("a missing terminal size must be replaceable, got %v", err)
	}
}

// TestOpenAppliesTheDefaultTerminalSize proves a client that reports no size
// still gets a deterministic layout rather than an unusable session.
func TestOpenAppliesTheDefaultTerminalSize(t *testing.T) {
	h := newHarness(t, testLimits())
	shell, err := h.coord.Open(context.Background(), OpenSessionRequest{
		Principal: testPrincipal,
		AuthMode:  AuthModePublic,
		Terminal:  TerminalMetadata{Term: "dumb"},
		CWD:       "/home/alice",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = shell.End(context.Background(), EndShutdown) })

	snapshot := shell.Snapshot()
	if snapshot.Terminal.Size != DefaultTerminalSize {
		t.Fatalf("terminal size = %+v, want %+v", snapshot.Terminal.Size, DefaultTerminalSize)
	}
	if snapshot.AuthMode != AuthModePublic {
		t.Errorf("auth mode = %q, want %q", snapshot.AuthMode, AuthModePublic)
	}
}

// TestShellSnapshotIsSafeToReadConcurrently proves the read-only view an adapter
// polls never races the session's writer goroutine.
func TestShellSnapshotIsSafeToReadConcurrently(t *testing.T) {
	h := newHarness(t, testLimits())
	_, shell := h.testSession(t)

	const readers = 8
	stop := make(chan struct{})
	done := make(chan struct{}, readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = shell.Snapshot()
			}
		}()
	}

	for seq := uint64(1); seq <= 5; seq++ {
		h.engine.scriptTurn(int(seq-1), textTurn("ok"))
		if _, err := shell.Accept(context.Background(), commandLine(shell, seq, "echo ok")); err != nil {
			t.Fatalf("Accept: %v", err)
		}
		// The session runs one turn at a time and bounds the queue behind it, so
		// each submission waits for the previous turn to retire. That is the
		// behaviour an interactive client sees.
		awaitIdle(t, shell, waitBudget)
	}

	close(stop)
	for i := 0; i < readers; i++ {
		<-done
	}
}

func mustSessionID(s string) domain.SessionID {
	id, err := domain.ParseSessionID(s)
	if err != nil {
		panic(err)
	}
	return id
}

func mustAppID(s string) domain.AppID {
	id, err := domain.ParseAppID(s)
	if err != nil {
		panic(err)
	}
	return id
}

func mustAppVersionID(s string) domain.AppVersionID {
	id, err := domain.ParseAppVersionID(s)
	if err != nil {
		panic(err)
	}
	return id
}
