package main

import (
	"context"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"j0s.at/vibeshell/internal/adapters/sqlite"
	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
	"j0s.at/vibeshell/internal/presentation"
	"j0s.at/vibeshell/internal/simulation"
)

// worldService resolves a principal's durable world namespaces. A user
// namespace is created on first use and cached, so a command never pays for a
// lookup it has already done.
type worldService struct {
	db    *sqlite.DB
	clock ports.Clock

	mu     sync.Mutex
	users  map[domain.UserID]domain.NamespaceID
	seeded map[domain.NamespaceID]bool
}

// newWorldService returns an empty resolver over the world store.
func newWorldService(db *sqlite.DB, clock ports.Clock) *worldService {
	return &worldService{
		db:     db,
		clock:  clock,
		users:  map[domain.UserID]domain.NamespaceID{},
		seeded: map[domain.NamespaceID]bool{},
	}
}

// userNamespace returns the principal's user namespace, creating it and seeding
// its baseline home tree on first use. home is the session's home directory.
func (w *worldService) userNamespace(ctx context.Context, user domain.UserID, home domain.ValidPath) (domain.NamespaceID, error) {
	w.mu.Lock()
	ns, ok := w.users[user]
	w.mu.Unlock()
	if ok {
		return ns, nil
	}
	created, err := w.db.EnsureNamespace(ctx, domain.NamespaceForUser(user, "user:"+user.Value()))
	if err != nil {
		return domain.NamespaceID{}, err
	}
	if err := w.seedHome(ctx, created.ID, home); err != nil {
		return domain.NamespaceID{}, err
	}
	w.mu.Lock()
	w.users[user] = created.ID
	w.mu.Unlock()
	return created.ID, nil
}

// seedHome creates the clean-install directory skeleton and the user's home
// tree once per namespace. It is idempotent: EnsureTree leaves existing nodes
// untouched, so a restart or a second session is a no-op.
func (w *worldService) seedHome(ctx context.Context, ns domain.NamespaceID, home domain.ValidPath) error {
	w.mu.Lock()
	done := w.seeded[ns]
	w.mu.Unlock()
	if done {
		return nil
	}
	if _, err := w.db.EnsureTree(ctx, ns, baselineSeedNodes(home)); err != nil {
		return err
	}
	w.mu.Lock()
	w.seeded[ns] = true
	w.mu.Unlock()
	return nil
}

// homeSubdirectories are the conventional directories a fresh account starts
// with.
var homeSubdirectories = []string{
	"Desktop", "Documents", "Downloads", "Music",
	"Pictures", "Public", "Templates", "Videos",
}

// homeNotes is the seeded content of the example file in a new home.
const homeNotes = "things to do this week:\n" +
	"- water the plants\n" +
	"- finish the shell prototype\n" +
	"- call the observatory about the moon-orchard reading\n"

// baselineSeedNodes builds the clean-install skeleton plus the user's home
// tree. The directory skeleton comes from the versioned baseline seed; the
// home files are deterministic so a first login has something to read.
func baselineSeedNodes(home domain.ValidPath) []sqlite.SeedNode {
	var nodes []sqlite.SeedNode
	for _, entry := range presentation.DefaultBaselineSeed().Directories {
		if entry.Path.IsRoot() {
			// EnsureNamespace already created the namespace root.
			continue
		}
		nodes = append(nodes, sqlite.SeedNode{Path: entry.Path, Kind: entry.Kind, Mode: entry.Mode})
	}
	nodes = append(nodes, sqlite.SeedNode{Path: home, Kind: domain.NodeKindDir})
	for _, name := range homeSubdirectories {
		nodes = append(nodes, sqlite.SeedNode{Path: home.Join(name), Kind: domain.NodeKindDir})
	}
	nodes = append(nodes, sqlite.SeedNode{Path: home.Join("notes.txt"), Kind: domain.NodeKindFile, Content: []byte(homeNotes)})
	return nodes
}

// namespaceResolver resolves the turn principal's user namespace for the world
// shell.
type namespaceResolver interface {
	namespaceFor(ctx context.Context, req application.TurnRequest) (domain.NamespaceID, error)
}

// userNamespaceResolver resolves a principal's user namespace for the
// generation engine's app world reads.
type userNamespaceResolver interface {
	userNamespace(ctx context.Context, user domain.UserID, home domain.ValidPath) (domain.NamespaceID, error)
}

// worldShell answers the shell's filesystem primitives against the durable
// simulated world. It is deterministic and never calls a model; a command it
// does not own is reported as unhandled so the caller keeps its other behavior
// (unknown programs still go to generation).
type worldShell struct {
	world   ports.WorldStore
	content ports.ContentStore
	names   namespaceResolver
	clock   ports.Clock
	// materializer generates the content of a missing file. Nil keeps the
	// truthful "no such file" behavior.
	materializer contentMaterializer
	maxRead      int64
}

// worldCommandMaxRead bounds one cat read.
const worldCommandMaxRead = 64 << 10

// worldListLimit bounds one directory listing.
const worldListLimit = 500

func newWorldShell(world ports.WorldStore, content ports.ContentStore, names namespaceResolver, clock ports.Clock) *worldShell {
	return &worldShell{world: world, content: content, names: names, clock: clock, maxRead: worldCommandMaxRead}
}

// now is the world command's clock; a nil clock falls back to wall time.
func (s *worldShell) now() int64 {
	if s.clock != nil {
		return s.clock.NowUnixMilli()
	}
	return time.Now().UnixMilli()
}

// run answers one filesystem command and reports whether it handled it.
func (s *worldShell) run(ctx context.Context, req application.TurnRequest, name string, args []string) (application.StepOutcome, bool, error) {
	switch name {
	case "ls":
		return s.runList(ctx, req, args)
	case "cat":
		return s.runRead(ctx, req, args)
	case "cd":
		return s.runChangeDir(ctx, req, args)
	case "mkdir":
		return s.runMakeDir(ctx, req, args)
	case "touch":
		return s.runTouch(ctx, req, args)
	case "rm":
		return s.runRemove(ctx, req, args)
	default:
		return application.StepOutcome{}, false, nil
	}
}

// isWorldCommand reports whether a command name is answered by the world shell.
func isWorldCommand(name string) bool {
	switch name {
	case "ls", "cat", "cd", "mkdir", "touch", "rm":
		return true
	default:
		return false
	}
}

// runList lists a directory. With no argument it lists the working directory;
// a file argument prints its name, matching ls.
func (s *worldShell) runList(ctx context.Context, req application.TurnRequest, args []string) (application.StepOutcome, bool, error) {
	ns, err := s.names.namespaceFor(ctx, req)
	if err != nil {
		return application.StepOutcome{}, true, err
	}
	target := req.Context.CWD
	if len(args) > 0 {
		resolved, perr := resolveWorldPath(req.Context.CWD, args[0])
		if perr != nil {
			return worldCandidate(req, "ls: "+args[0]+": No such file or directory", 1, application.SessionPatch{}, domain.ChangeSet{}), true, nil
		}
		target = resolved
	}
	node, err := s.world.LookupPath(ctx, ns, target)
	if err != nil {
		if domain.IsNotFoundError(err) {
			return worldCandidate(req, "ls: "+target.String()+": No such file or directory", 1, application.SessionPatch{}, domain.ChangeSet{}), true, nil
		}
		return application.StepOutcome{}, true, err
	}
	if !node.IsDir() {
		return worldCandidate(req, target.Base(), 0, application.SessionPatch{}, domain.ChangeSet{}), true, nil
	}
	children, _, err := s.world.ListDirectory(ctx, ns, node.ID, worldListLimit, "")
	if err != nil {
		return application.StepOutcome{}, true, err
	}
	names := make([]string, 0, len(children))
	for _, child := range children {
		names = append(names, child.Name)
	}
	sort.Strings(names)
	return worldCandidate(req, strings.Join(names, "\n"), 0, application.SessionPatch{}, domain.ChangeSet{}), true, nil
}

// runRead prints the content of one or more files, reporting each missing path
// on stderr-like output and setting a non-zero exit status.
func (s *worldShell) runRead(ctx context.Context, req application.TurnRequest, args []string) (application.StepOutcome, bool, error) {
	ns, err := s.names.namespaceFor(ctx, req)
	if err != nil {
		return application.StepOutcome{}, true, err
	}
	if len(args) == 0 {
		return worldCandidate(req, "cat: missing operand", 1, application.SessionPatch{}, domain.ChangeSet{}), true, nil
	}
	cs := domain.EmptyChangeSet(req.Turn, req.Attempt, s.now())
	var out strings.Builder
	exit := 0
	for _, arg := range args {
		target, perr := resolveWorldPath(req.Context.CWD, arg)
		if perr != nil {
			out.WriteString("cat: " + arg + ": No such file or directory\n")
			exit = 1
			continue
		}
		node, lerr := s.world.LookupPath(ctx, ns, target)
		if lerr != nil {
			if !domain.IsNotFoundError(lerr) {
				return application.StepOutcome{}, true, lerr
			}
			if data, ok := s.materialize(ctx, ns, target, &cs); ok {
				out.Write(data)
				continue
			}
			out.WriteString("cat: " + arg + ": No such file or directory\n")
			exit = 1
			continue
		}
		if !node.IsFile() {
			out.WriteString("cat: " + arg + ": Is a directory\n")
			exit = 1
			continue
		}
		data, gerr := s.content.Get(ctx, node.Content, 0, s.maxRead)
		if gerr != nil {
			return application.StepOutcome{}, true, gerr
		}
		out.Write(data)
	}
	return worldCandidate(req, strings.TrimRight(out.String(), "\n"), exit, application.SessionPatch{}, cs), true, nil
}

// materialize generates and stages the content of a missing file, returning it
// for this turn. It reports false when no materializer is wired, the parent is
// not a directory, or generation fails, so the caller keeps the truthful
// "no such file" message.
func (s *worldShell) materialize(ctx context.Context, ns domain.NamespaceID, target domain.ValidPath, cs *domain.ChangeSet) ([]byte, bool) {
	if s.materializer == nil {
		return nil, false
	}
	parent, err := s.world.LookupPath(ctx, ns, target.Parent())
	if err != nil || !parent.IsDir() {
		return nil, false
	}
	data, err := s.materializer.Materialize(ctx, target, "")
	if err != nil || len(data) == 0 {
		return nil, false
	}
	ref, err := s.content.Put(ctx, data, "text/plain; charset=utf-8")
	if err != nil {
		return nil, false
	}
	meta := domain.NewNodeMetadata(0o644, 0, 0, s.now())
	cs.AddCreate(ns, target, domain.NodeKindFile, meta, ref)
	cs.AddReadDep(parent.ID, parent.Revision, parent.Kind, false, domain.NodeID{})
	return data, true
}

// runChangeDir resolves a directory and proposes it as the new working
// directory. With no argument it returns to the session's home directory.
func (s *worldShell) runChangeDir(ctx context.Context, req application.TurnRequest, args []string) (application.StepOutcome, bool, error) {
	ns, err := s.names.namespaceFor(ctx, req)
	if err != nil {
		return application.StepOutcome{}, true, err
	}
	target := req.Context.Home
	if len(args) > 0 {
		resolved, perr := resolveWorldPath(req.Context.CWD, args[0])
		if perr != nil {
			return worldCandidate(req, "cd: "+args[0]+": No such file or directory", 1, application.SessionPatch{}, domain.ChangeSet{}), true, nil
		}
		target = resolved
	}
	node, lerr := s.world.LookupPath(ctx, ns, target)
	if lerr != nil {
		if domain.IsNotFoundError(lerr) {
			return worldCandidate(req, "cd: "+args[0]+": No such file or directory", 1, application.SessionPatch{}, domain.ChangeSet{}), true, nil
		}
		return application.StepOutcome{}, true, lerr
	}
	if !node.IsDir() {
		return worldCandidate(req, "cd: "+args[0]+": Not a directory", 1, application.SessionPatch{}, domain.ChangeSet{}), true, nil
	}
	return worldCandidate(req, "", 0, application.SessionPatch{CWD: &target}, domain.ChangeSet{}), true, nil
}

// runMakeDir creates one or more directories. Each creation is staged in the
// turn's change set and committed by the coordinator.
func (s *worldShell) runMakeDir(ctx context.Context, req application.TurnRequest, args []string) (application.StepOutcome, bool, error) {
	ns, err := s.names.namespaceFor(ctx, req)
	if err != nil {
		return application.StepOutcome{}, true, err
	}
	if len(args) == 0 {
		return worldCandidate(req, "mkdir: missing operand", 1, application.SessionPatch{}, domain.ChangeSet{}), true, nil
	}
	now := s.now()
	cs := domain.EmptyChangeSet(req.Turn, req.Attempt, now)
	var out strings.Builder
	exit := 0
	for _, arg := range args {
		target, perr := resolveWorldPath(req.Context.CWD, arg)
		if perr != nil {
			out.WriteString("mkdir: cannot create directory '" + arg + "': Invalid argument\n")
			exit = 1
			continue
		}
		if _, lerr := s.world.LookupPath(ctx, ns, target); lerr == nil {
			out.WriteString("mkdir: cannot create directory '" + arg + "': File exists\n")
			exit = 1
			continue
		} else if !domain.IsNotFoundError(lerr) {
			return application.StepOutcome{}, true, lerr
		}
		parent, perr := s.world.LookupPath(ctx, ns, target.Parent())
		if perr != nil {
			if domain.IsNotFoundError(perr) {
				out.WriteString("mkdir: cannot create directory '" + arg + "': No such file or directory\n")
				exit = 1
				continue
			}
			return application.StepOutcome{}, true, perr
		}
		cs.AddCreate(ns, target, domain.NodeKindDir, domain.NewNodeMetadata(0o755, 0, 0, now), domain.EmptyContentRef())
		cs.AddReadDep(parent.ID, parent.Revision, parent.Kind, false, domain.NodeID{})
	}
	return worldCandidate(req, strings.TrimRight(out.String(), "\n"), exit, application.SessionPatch{}, cs), true, nil
}

// runTouch creates empty files that do not exist. An existing file is left
// unchanged; its timestamp is not updated yet.
func (s *worldShell) runTouch(ctx context.Context, req application.TurnRequest, args []string) (application.StepOutcome, bool, error) {
	ns, err := s.names.namespaceFor(ctx, req)
	if err != nil {
		return application.StepOutcome{}, true, err
	}
	if len(args) == 0 {
		return worldCandidate(req, "touch: missing file operand", 1, application.SessionPatch{}, domain.ChangeSet{}), true, nil
	}
	now := s.now()
	cs := domain.EmptyChangeSet(req.Turn, req.Attempt, now)
	var out strings.Builder
	exit := 0
	for _, arg := range args {
		target, perr := resolveWorldPath(req.Context.CWD, arg)
		if perr != nil {
			out.WriteString("touch: cannot touch '" + arg + "': Invalid argument\n")
			exit = 1
			continue
		}
		if _, lerr := s.world.LookupPath(ctx, ns, target); lerr == nil {
			continue
		} else if !domain.IsNotFoundError(lerr) {
			return application.StepOutcome{}, true, lerr
		}
		parent, perr := s.world.LookupPath(ctx, ns, target.Parent())
		if perr != nil {
			if domain.IsNotFoundError(perr) {
				out.WriteString("touch: cannot touch '" + arg + "': No such file or directory\n")
				exit = 1
				continue
			}
			return application.StepOutcome{}, true, perr
		}
		cs.AddCreate(ns, target, domain.NodeKindFile, domain.NewNodeMetadata(0o644, 0, 0, now), domain.EmptyContentRef())
		cs.AddReadDep(parent.ID, parent.Revision, parent.Kind, false, domain.NodeID{})
	}
	return worldCandidate(req, strings.TrimRight(out.String(), "\n"), exit, application.SessionPatch{}, cs), true, nil
}

// runRemove deletes regular files. A directory is refused; there is no
// recursive flag yet.
func (s *worldShell) runRemove(ctx context.Context, req application.TurnRequest, args []string) (application.StepOutcome, bool, error) {
	ns, err := s.names.namespaceFor(ctx, req)
	if err != nil {
		return application.StepOutcome{}, true, err
	}
	if len(args) == 0 {
		return worldCandidate(req, "rm: missing operand", 1, application.SessionPatch{}, domain.ChangeSet{}), true, nil
	}
	now := s.now()
	cs := domain.EmptyChangeSet(req.Turn, req.Attempt, now)
	var out strings.Builder
	exit := 0
	for _, arg := range args {
		target, perr := resolveWorldPath(req.Context.CWD, arg)
		if perr != nil {
			out.WriteString("rm: cannot remove '" + arg + "': No such file or directory\n")
			exit = 1
			continue
		}
		node, lerr := s.world.LookupPath(ctx, ns, target)
		if lerr != nil {
			if domain.IsNotFoundError(lerr) {
				out.WriteString("rm: cannot remove '" + arg + "': No such file or directory\n")
				exit = 1
				continue
			}
			return application.StepOutcome{}, true, lerr
		}
		if node.IsDir() {
			out.WriteString("rm: cannot remove '" + arg + "': Is a directory\n")
			exit = 1
			continue
		}
		cs.AddDelete(ns, target, node.ID, node.Revision)
		cs.AddReadDep(node.ID, node.Revision, node.Kind, false, domain.NodeID{})
	}
	return worldCandidate(req, strings.TrimRight(out.String(), "\n"), exit, application.SessionPatch{}, cs), true, nil
}

// namespaceFor resolves the turn principal's user namespace.
func (w *worldService) namespaceFor(ctx context.Context, req application.TurnRequest) (domain.NamespaceID, error) {
	return w.userNamespace(ctx, req.Principal, req.Context.Home)
}

// Namespaces resolves the trusted namespace IDs the simulation tool layer
// addresses. It satisfies simulation.NamespaceResolver: the IDs are minted here
// from the turn's principal and session, never from model input.
func (w *worldService) Namespaces(ctx context.Context, req application.TurnRequest) (simulation.CallNamespaces, error) {
	user, err := w.userNamespace(ctx, req.Principal, req.Context.Home)
	if err != nil {
		return simulation.CallNamespaces{}, err
	}
	session, err := w.db.EnsureNamespace(ctx, domain.NamespaceForSession(req.Session))
	if err != nil {
		return simulation.CallNamespaces{}, err
	}
	shared, err := w.db.EnsureNamespace(ctx, domain.NamespaceShared())
	if err != nil {
		return simulation.CallNamespaces{}, err
	}
	return simulation.CallNamespaces{Session: session.ID, User: user, Shared: shared.ID}, nil
}

// resolveWorldPath resolves a command argument against the working directory and
// cleans it, so "..", "." and relative names behave like a shell while the
// stored path stays absolute and canonical.
func resolveWorldPath(cwd domain.ValidPath, arg string) (domain.ValidPath, error) {
	joined := arg
	if !strings.HasPrefix(arg, "/") {
		joined = path.Join(string(cwd), arg)
	}
	return domain.ParsePath(path.Clean(joined))
}

// worldCandidate builds a plain-text candidate for a world command. A command
// that mutates the world carries its change set, which the coordinator commits.
func worldCandidate(req application.TurnRequest, text string, exit int, patch application.SessionPatch, changes domain.ChangeSet) application.StepOutcome {
	return application.StepOutcome{
		Phase: application.StepCandidate,
		Candidate: &application.TurnCandidate{
			CommitKey:    application.CommitKey{Turn: req.Turn, Logical: "world"},
			Changes:      changes,
			Output:       application.CandidateOutput{Text: text},
			SessionPatch: patch,
			ExitStatus:   exit,
		},
	}
}
