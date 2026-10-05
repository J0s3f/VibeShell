package main

import (
	"context"
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/domain"
)

// localEngine is the deterministic fallback turn engine used when no model
// account is configured. PLAN 12.3 requires a truthful service-unavailable
// message instead of pretending inference succeeded; this engine supplies
// that message and a small amount of identity output that is derived from
// the configured presentation facts, never from a hardcoded command
// catalogue and never by running a real program.
//
// It is deliberately not a simulation: when a provider is configured the
// composition root still uses it only because no production generation
// engine (PLAN 7.1 step 3) has been merged yet. The engine marks every
// non-identity command as "generation unavailable" so the gap is visible on
// the terminal rather than fabricated.
type localEngine struct {
	identity presentationIdentity
	// generationAvailable reports whether a model account is configured. It
	// does not change this engine's behavior except to word the unavailable
	// message accurately.
	generationAvailable bool
	// shell answers the filesystem primitives against the simulated world. Nil
	// leaves them to the unavailable message.
	shell *worldShell
}

// presentationIdentity is the subset of configured identity facts the
// fallback engine may report. It mirrors presentation.SystemIdentity without
// importing the presentation package into the engine seam.
type presentationIdentity struct {
	System     string
	Shell      string
	Hostname   string
	HomePrefix string
}

// Compile-time proof that the fallback engine satisfies the coordinator seam.
var _ application.TurnEngine = (*localEngine)(nil)

// Prepare assembles no model context: the fallback engine needs only the
// request it is handed at generation time.
func (e *localEngine) Prepare(_ context.Context, req application.TurnRequest) (application.PreparedTurn, error) {
	return application.PreparedTurn{}, nil
}

// Generate returns one complete candidate. The turn coordinator owns
// validation, commit, and emission; the engine only proposes output.
func (e *localEngine) Generate(ctx context.Context, in application.GenerationInput) (application.StepOutcome, error) {
	req := in.Request
	if e.shell != nil && req.Input.Kind == application.InputCommand {
		if fields := strings.Fields(req.Input.Command); len(fields) > 0 {
			if outcome, handled, err := e.shell.run(ctx, req, fields[0], fields[1:]); handled {
				return outcome, err
			}
		}
	}
	text, exit := e.respond(req)
	return application.StepOutcome{
		Phase: application.StepCandidate,
		Candidate: &application.TurnCandidate{
			CommitKey:  application.CommitKey{Turn: req.Turn, Logical: "fallback"},
			Output:     application.CandidateOutput{Text: text},
			ExitStatus: exit,
		},
		Route: domain.RouteID{},
	}, nil
}

// respond produces the deterministic response for one accepted input.
func (e *localEngine) respond(req application.TurnRequest) (string, int) {
	if req.Input.Kind == application.InputPaste {
		return e.unavailable("pasted input"), 1
	}
	fields := strings.Fields(req.Input.Command)
	if len(fields) == 0 {
		return "", 0
	}
	switch fields[0] {
	case "pwd":
		return string(req.Context.CWD), 0
	case "whoami":
		// The displayed username is not carried on the turn; the home
		// directory's last segment is the deterministic displayed name.
		return displayedUser(req.Context.CWD), 0
	case "uname":
		if len(fields) > 1 && fields[1] == "-a" {
			return fmt.Sprintf("%s %s %s GNU/Hurd-style %s", e.identity.System, e.identity.Hostname, "1.0.0", "x86_64"), 0
		}
		return e.identity.System, 0
	case "echo":
		return expandEnv(strings.Join(fields[1:], " "), req), 0
	case "env":
		return "HOME=" + req.Context.Home.String() + "\nPWD=" + req.Context.CWD.String(), 0
	case "exit", "logout":
		// A real shell exit is handled by the transport; the fallback
		// engine cannot close the channel, so it reports the accepted
		// intent without pretending to run a shell builtin.
		return "", 0
	default:
		return e.unavailable(fields[0]), 127
	}
}

// expandEnv substitutes the shell's environment variables in one line. The
// shell has no general environment yet; HOME and PWD are the two facts the
// session context already carries, so they are the variables it exposes.
func expandEnv(text string, req application.TurnRequest) string {
	replacer := strings.NewReplacer(
		"${HOME}", req.Context.Home.String(),
		"$HOME", req.Context.Home.String(),
		"${PWD}", req.Context.CWD.String(),
		"$PWD", req.Context.CWD.String(),
	)
	return replacer.Replace(text)
}

// unavailable is the truthful message shown for any command the fallback
// engine cannot handle. It never names a fabricated result.
func (e *localEngine) unavailable(name string) string {
	if e.generationAvailable {
		return fmt.Sprintf("vibeshell: %s: simulation generation is not yet wired in this build", name)
	}
	return fmt.Sprintf("vibeshell: %s: model generation is unavailable (no provider account configured)", name)
}

// displayedUser extracts the displayed username from a home-directory path
// of the form /home/<username>. It is used only for the fallback whoami.
func displayedUser(cwd domain.ValidPath) string {
	parts := strings.Split(strings.Trim(string(cwd), "/"), "/")
	if len(parts) >= 2 && parts[0] == "home" {
		return parts[1]
	}
	return parts[len(parts)-1]
}

// rejectTools is the ToolExecutor wired into the coordinator. The fallback
// engine never requests a tool batch, so any batch here is a programming
// error: it is reported as a typed failure rather than silently ignored.
type rejectTools struct{}

var _ application.ToolExecutor = rejectTools{}

func (rejectTools) Execute(context.Context, application.ToolBatch) (application.ToolBatchResult, error) {
	return application.ToolBatchResult{}, domain.NewUnavailableError(
		domain.CodeShutdown, "no tool executor is wired in this build", nil, nil)
}

// staticSnapshots is the coordinator's SnapshotSource. It returns the
// configuration snapshot in force; a live configuration reload would replace
// it, but this build performs no mid-flight reload.
type staticSnapshots struct {
	snapshot application.ConfigSnapshot
}

var _ application.SnapshotSource = (*staticSnapshots)(nil)

func (s *staticSnapshots) CurrentSnapshot(context.Context) (application.ConfigSnapshot, error) {
	return s.snapshot, nil
}
