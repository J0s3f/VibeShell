package simulation

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"testing"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// ---------------------------------------------------------------------------
// fakeWorld: in-memory WorldStore
// ---------------------------------------------------------------------------

type fakeWorld struct {
	mu      sync.Mutex
	nodes   map[domain.NodeID]domain.Node
	paths   map[domain.NamespaceID]map[domain.ValidPath]domain.NodeID
	commits []domain.ChangeSet
	nextID  int
}

func newFakeWorld() *fakeWorld {
	return &fakeWorld{
		nodes: map[domain.NodeID]domain.Node{},
		paths: map[domain.NamespaceID]map[domain.ValidPath]domain.NodeID{},
	}
}

// add creates a node and wires its parent link from the already-registered
// parent path, so directory listing and path walks behave like the world.
func (f *fakeWorld) add(ns domain.NamespaceID, path domain.ValidPath, kind domain.NodeKind, rev domain.Revision) domain.Node {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fakeNodeID(f.nextID)
	name := path.Base()
	if path.IsRoot() {
		name = ""
	}
	node := domain.Node{
		ID:          id,
		NamespaceID: ns,
		Name:        name,
		Kind:        kind,
		Revision:    rev,
		Metadata:    domain.NewNodeMetadata(0o644, 1000, 1000, 0),
	}
	if !path.IsRoot() {
		if pid, ok := f.paths[ns][path.Parent()]; ok {
			node.ParentID = &pid
		}
	}
	f.nodes[id] = node
	if f.paths[ns] == nil {
		f.paths[ns] = map[domain.ValidPath]domain.NodeID{}
	}
	f.paths[ns][path] = id
	return node
}

func (f *fakeWorld) GetNode(_ context.Context, _ domain.NamespaceID, id domain.NodeID) (domain.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	node, ok := f.nodes[id]
	if !ok {
		return domain.Node{}, domain.NewNotFoundError(domain.CodeNodeNotFound, "node not found", nil)
	}
	return node, nil
}

func (f *fakeWorld) LookupPath(_ context.Context, ns domain.NamespaceID, path domain.ValidPath) (domain.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.paths[ns][path]
	if !ok {
		return domain.Node{}, domain.NewNotFoundError(domain.CodeNodeNotFound, "path not found", nil)
	}
	return f.nodes[id], nil
}

func (f *fakeWorld) ListDirectory(_ context.Context, ns domain.NamespaceID, dirID domain.NodeID, limit int, cursor string) ([]domain.Node, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.nodes[dirID]
	if !ok {
		return nil, "", domain.NewNotFoundError(domain.CodeNodeNotFound, "directory not found", nil)
	}
	var children []domain.Node
	for _, node := range f.nodes {
		if node.NamespaceID != ns || node.ParentID == nil || *node.ParentID != dirID {
			continue
		}
		children = append(children, node)
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	offset := 0
	if cursor != "" {
		if n, err := strconv.Atoi(cursor); err == nil && n > 0 {
			offset = n
		}
	}
	if offset > len(children) {
		offset = len(children)
	}
	page := children[offset:]
	next := ""
	if len(page) > limit {
		page = page[:limit]
		next = strconv.Itoa(offset + limit)
	}
	return page, next, nil
}

func (f *fakeWorld) Commit(_ context.Context, cs domain.ChangeSet, _ domain.ScopePolicy) (domain.Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Verify read dependencies: absence checks and expected revisions.
	for _, dep := range cs.ReadDependencies {
		if dep.IsAbsence {
			if dep.NodeID.IsZero() {
				continue // path absence is verified by the create itself
			}
			if _, ok := f.nodes[dep.NodeID]; ok {
				return 0, domain.NewConflictError(domain.CodeRevisionMismatch, "expected absence but node exists", nil)
			}
			continue
		}
		node, ok := f.nodes[dep.NodeID]
		if !ok {
			return 0, domain.NewConflictError(domain.CodeRevisionMismatch, "dependency node missing", nil)
		}
		if node.Revision != dep.Revision {
			return 0, domain.NewConflictError(domain.CodeRevisionMismatch, "revision mismatch", nil)
		}
	}
	rev := domain.Revision(1)
	for _, mut := range cs.Mutations {
		switch mut.Type {
		case domain.MutationCreate:
			f.nextID++
			id := fakeNodeID(f.nextID)
			name := mut.Path.Base()
			if mut.Path.IsRoot() {
				name = ""
			}
			node := domain.Node{
				ID:          id,
				NamespaceID: mut.NamespaceID,
				Name:        name,
				Kind:        mut.Kind,
				Revision:    domain.InitialRevision,
				Metadata:    mut.Metadata,
				Content:     mut.Content,
			}
			if !mut.Path.IsRoot() {
				if parentID, ok := f.paths[mut.NamespaceID][mut.Path.Parent()]; ok {
					node.ParentID = &parentID
				}
			}
			f.nodes[id] = node
			if f.paths[mut.NamespaceID] == nil {
				f.paths[mut.NamespaceID] = map[domain.ValidPath]domain.NodeID{}
			}
			f.paths[mut.NamespaceID][mut.Path] = id
			rev = node.Revision
		case domain.MutationUpdate, domain.MutationDelete, domain.MutationTombstone:
			if mut.NodeID == nil {
				return 0, domain.NewConflictError(domain.CodeRevisionMismatch, "mutation missing node id", nil)
			}
			node, ok := f.nodes[*mut.NodeID]
			if !ok {
				return 0, domain.NewConflictError(domain.CodeRevisionMismatch, "mutation node missing", nil)
			}
			if node.Revision != mut.ExpectedRev {
				return 0, domain.NewConflictError(domain.CodeRevisionMismatch, "revision mismatch", nil)
			}
			if mut.Type == domain.MutationDelete || mut.Type == domain.MutationTombstone {
				delete(f.nodes, node.ID)
				delete(f.paths[node.NamespaceID], mut.Path)
			} else {
				node.Metadata = mut.Metadata
				node.Content = mut.Content
				node.Revision = node.Revision.Next()
				f.nodes[node.ID] = node
			}
			rev = node.Revision.Next()
		}
	}
	f.commits = append(f.commits, cs)
	return rev, nil
}

// ---------------------------------------------------------------------------
// fakeEvents: in-memory EventStore
// ---------------------------------------------------------------------------

type fakeEvents struct {
	mu        sync.Mutex
	bySession map[domain.SessionID][]domain.EventRecord
	all       []domain.EventRecord
	counters  map[domain.SessionID]uint64
	idSeq     int
}

func newFakeEvents() *fakeEvents {
	return &fakeEvents{
		bySession: map[domain.SessionID][]domain.EventRecord{},
		counters:  map[domain.SessionID]uint64{},
	}
}

func (f *fakeEvents) Append(_ context.Context, evt domain.EventEnvelope) (domain.EventRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idSeq++
	evt.EventID = fakeEventID(f.idSeq)
	f.counters[evt.SessionID]++
	evt.Sequence = f.counters[evt.SessionID]
	record := domain.EventRecord{
		Envelope:   evt,
		Payload:    evt.Payload.Inline,
		Provenance: evt.Provenance,
	}
	f.bySession[evt.SessionID] = append(f.bySession[evt.SessionID], record)
	f.all = append(f.all, record)
	return record, nil
}

func (f *fakeEvents) GetByID(_ context.Context, id domain.EventID) (domain.EventRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range f.all {
		if record.Envelope.EventID == id {
			return record, nil
		}
	}
	return domain.EventRecord{}, domain.NewNotFoundError(domain.CodeEventNotFound, "event not found", nil)
}

func (f *fakeEvents) List(_ context.Context, session domain.SessionID, minSeq uint64, limit int) ([]domain.EventRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.EventRecord
	for _, record := range f.bySession[session] {
		if record.Envelope.Sequence < minSeq {
			continue
		}
		out = append(out, record)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// fakeRetrieval: in-memory RetrievalStore with scope enforcement
// ---------------------------------------------------------------------------

type fakeRetrieval struct {
	mu     sync.Mutex
	events []domain.EventRecord
	scopes map[domain.EventID]domain.Scope
	owners map[domain.EventID]domain.UserID
}

func newFakeRetrieval() *fakeRetrieval {
	return &fakeRetrieval{
		scopes: map[domain.EventID]domain.Scope{},
		owners: map[domain.EventID]domain.UserID{},
	}
}

func (f *fakeRetrieval) add(evt domain.EventRecord, scope domain.Scope, owner domain.UserID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, evt)
	f.scopes[evt.Envelope.EventID] = scope
	f.owners[evt.Envelope.EventID] = owner
}

// authorize applies the same scope rules as the domain policy: session
// events are caller-local, user events need same-user or cross-user read,
// shared events need sharing enabled.
func (f *fakeRetrieval) authorize(evt domain.EventRecord, q domain.RetrievalQuery, policy domain.ScopePolicy) error {
	scope := f.scopes[evt.Envelope.EventID]
	if scope == domain.ScopeSession {
		for _, sid := range q.Scope.SessionIDs {
			if sid == evt.Envelope.SessionID {
				return nil
			}
		}
		return domain.NewDeniedError(domain.CodeScopeDenied, "session not in query scope", nil)
	}
	owner := f.owners[evt.Envelope.EventID]
	sameUser := false
	for _, uid := range q.Scope.UserIDs {
		if uid == owner {
			sameUser = true
		}
	}
	return policy.AuthorizeRead(scope, sameUser)
}

func (f *fakeRetrieval) Query(_ context.Context, q domain.RetrievalQuery, policy domain.ScopePolicy) (domain.RetrievalResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matched []domain.EventRecord
	for _, evt := range f.events {
		if err := f.authorize(evt, q, policy); err != nil {
			continue
		}
		if len(q.Filter.Kinds) > 0 {
			want := false
			for _, k := range q.Filter.Kinds {
				if k == evt.Envelope.Kind {
					want = true
				}
			}
			if !want {
				continue
			}
		}
		if q.Filter.TurnID != nil && (evt.Envelope.TurnID == nil || *evt.Envelope.TurnID != *q.Filter.TurnID) {
			continue
		}
		if q.Filter.FromTime != 0 && evt.Envelope.Timestamp < q.Filter.FromTime {
			continue
		}
		if q.Filter.ToTime != 0 && evt.Envelope.Timestamp >= q.Filter.ToTime {
			continue
		}
		matched = append(matched, evt)
	}
	sort.Slice(matched, func(i, j int) bool {
		if q.Pagination.Descending {
			return matched[i].Envelope.Sequence > matched[j].Envelope.Sequence
		}
		return matched[i].Envelope.Sequence < matched[j].Envelope.Sequence
	})
	truncated := false
	if q.Pagination.Limit > 0 && len(matched) > q.Pagination.Limit {
		matched = matched[:q.Pagination.Limit]
		truncated = true
	}
	return domain.RetrievalResult{Events: matched, Truncated: truncated}, nil
}

func (f *fakeRetrieval) Surrounding(_ context.Context, ref domain.EventReference, before, after int, policy domain.ScopePolicy) ([]domain.EventRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var preceding, following []domain.EventRecord
	for _, evt := range f.events {
		if evt.Envelope.SessionID != ref.SessionID {
			continue
		}
		if evt.Envelope.Sequence < ref.Sequence {
			preceding = append(preceding, evt)
		} else if evt.Envelope.Sequence > ref.Sequence {
			following = append(following, evt)
		}
	}
	sort.Slice(preceding, func(i, j int) bool { return preceding[i].Envelope.Sequence < preceding[j].Envelope.Sequence })
	sort.Slice(following, func(i, j int) bool { return following[i].Envelope.Sequence < following[j].Envelope.Sequence })
	if len(preceding) > before {
		preceding = preceding[len(preceding)-before:]
	}
	if len(following) > after {
		following = following[:after]
	}
	// Scope enforcement on the surrounding window: session-only references
	// are caller-local, so every event of the referenced session is allowed.
	var authorized []domain.EventRecord
	for _, evt := range append(preceding, following...) {
		if f.scopes[evt.Envelope.EventID] == domain.ScopeSession {
			authorized = append(authorized, evt)
		}
	}
	return authorized, nil
}

// ---------------------------------------------------------------------------
// fakeContent: in-memory ContentStore
// ---------------------------------------------------------------------------

type fakeContent struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newFakeContent() *fakeContent {
	return &fakeContent{data: map[string][]byte{}}
}

func (f *fakeContent) Put(_ context.Context, data []byte, _ string) (domain.ContentRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sum := sha256.Sum256(data)
	id, err := domain.ParseContentID("cnt_" + encodeID128(sum[:16]))
	if err != nil {
		return domain.ContentRef{}, err
	}
	f.data[id.String()] = data
	return domain.ContentRef{Hash: id, Size: int64(len(data))}, nil
}

func (f *fakeContent) Get(_ context.Context, ref domain.ContentRef, offset, length int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.data[ref.Hash.String()]
	if !ok {
		return nil, domain.NewNotFoundError(domain.CodeContentNotFound, "content not found", nil)
	}
	if offset >= int64(len(data)) {
		return []byte{}, nil
	}
	end := offset + length
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return data[offset:end], nil
}

// ---------------------------------------------------------------------------
// fakeApps: in-memory AppRegistry
// ---------------------------------------------------------------------------

type fakeApps struct {
	mu        sync.Mutex
	artifacts map[domain.AppVersionID]domain.AppArtifact
	current   map[domain.AppID]domain.AppVersionID
}

func newFakeApps() *fakeApps {
	return &fakeApps{
		artifacts: map[domain.AppVersionID]domain.AppArtifact{},
		current:   map[domain.AppID]domain.AppVersionID{},
	}
}

// seed registers an accepted artifact and makes it current for its app.
func (f *fakeApps) seed(artifact domain.AppArtifact) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.artifacts[artifact.VersionID] = artifact
	f.current[artifact.AppID] = artifact.VersionID
}

func (f *fakeApps) GetArtifact(_ context.Context, version domain.AppVersionID) (domain.AppArtifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	artifact, ok := f.artifacts[version]
	if !ok {
		return domain.AppArtifact{}, domain.NewNotFoundError(domain.CodeAppVersionNotFound, "app version not found", nil)
	}
	return artifact, nil
}

func (f *fakeApps) Current(_ context.Context, app domain.AppID) (domain.AppVersionID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	version, ok := f.current[app]
	if !ok {
		return domain.AppVersionID{}, domain.NewNotFoundError(domain.CodeAppNotFound, "app not found", nil)
	}
	return version, nil
}

func (f *fakeApps) RegisterCandidate(_ context.Context, artifact domain.AppArtifact) (domain.AppVersionID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.artifacts[artifact.VersionID] = artifact
	return artifact.VersionID, nil
}

func (f *fakeApps) Activate(_ context.Context, app domain.AppID, version domain.AppVersionID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.artifacts[version]; !ok {
		return domain.NewNotFoundError(domain.CodeAppVersionNotFound, "app version not found", nil)
	}
	f.current[app] = version
	return nil
}

func (f *fakeApps) Rollback(_ context.Context, app domain.AppID, to domain.AppVersionID, _ string) error {
	return f.Activate(context.Background(), app, to)
}

// ---------------------------------------------------------------------------
// fakeSandbox: canned AppSandbox
// ---------------------------------------------------------------------------

type fakeSandbox struct {
	mu     sync.Mutex
	runs   []ports.SandboxLimits
	result domain.AppResult
	err    error
}

func (f *fakeSandbox) Run(_ context.Context, _ domain.AppArtifact, _ domain.AppState, _ domain.AppEvent, limits ports.SandboxLimits) (domain.AppResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, limits)
	return f.result, f.err
}

// ---------------------------------------------------------------------------
// fakeRandom
// ---------------------------------------------------------------------------

type fakeRandom struct{ state int64 }

func newFakeRandom(seed int64) *fakeRandom { return &fakeRandom{state: seed} }

func (f *fakeRandom) Bytes(n int) ([]byte, error) {
	out := make([]byte, n)
	for i := range out {
		f.state = f.state*1103515245 + 12345
		out[i] = byte(f.state >> 16)
	}
	return out, nil
}

func (f *fakeRandom) Intn(n int) int {
	b, _ := f.Bytes(8)
	v := binary.BigEndian.Uint64(b)
	return int(v % uint64(n))
}

// ---------------------------------------------------------------------------
// fakeSummarizer / fakeGenerator / fakeRedactor
// ---------------------------------------------------------------------------

type fakeSummarizer struct {
	mu       sync.Mutex
	requests []SummaryRequest
	text     string
	err      error
}

func (f *fakeSummarizer) Summarize(_ context.Context, req SummaryRequest) (SummaryRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.err != nil {
		return SummaryRecord{}, f.err
	}
	return SummaryRecord{
		SummaryID:     deriveSummaryID(req.Events),
		Scope:         req.Scope,
		SourceEvents:  eventIDs(req.Events),
		SourceRange:   eventRange(req.Events),
		TokenCount:    estimateTokens(f.text),
		ModelRoute:    req.ModelRoute,
		PromptVersion: req.PromptVersion,
		CreatedAt:     req.NowUnixMilli,
		Text:          f.text,
	}, nil
}

type fakeGenerator struct {
	mu      sync.Mutex
	content []byte
	err     error
	calls   []domain.ValidPath
}

func (f *fakeGenerator) GenerateContent(_ context.Context, path domain.ValidPath, _ domain.NodeKind, _ string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, path)
	return f.content, f.err
}

type fakeRedactor struct {
	dropped map[string]bool
}

func newFakeRedactor() *fakeRedactor {
	return &fakeRedactor{dropped: map[string]bool{}}
}

func (f *fakeRedactor) RedactJSON(payload json.RawMessage) (json.RawMessage, bool) {
	if f.dropped[string(payload)] {
		return nil, true
	}
	return payload, false
}

func (f *fakeRedactor) RedactText(s string) string { return s }

// ---------------------------------------------------------------------------
// Shared test fixtures
// ---------------------------------------------------------------------------

var (
	testUser     = mustParseUserID("usr_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	testUser2    = mustParseUserID("usr_01ARZ3NDEKTSV4RRFFQ69G5FAW")
	testSession  = mustParseSessionID("ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	testSession2 = mustParseSessionID("ses_01ARZ3NDEKTSV4RRFFQ69G5FAW")
	testTurn     = mustParseTurnID("trn_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	testAttempt  = mustParseAttemptID("att_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	testApp      = mustParseAppID("app_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	testAppVer   = mustParseAppVersionID("av_01ARZ3NDEKTSV4RRFFQ69G5FAV")
)

func mustParseUserID(s string) domain.UserID {
	id, err := domain.ParseUserID(s)
	if err != nil {
		panic(err)
	}
	return id
}

func mustParseSessionID(s string) domain.SessionID {
	id, err := domain.ParseSessionID(s)
	if err != nil {
		panic(err)
	}
	return id
}

func mustParseTurnID(s string) domain.TurnID {
	id, err := domain.ParseTurnID(s)
	if err != nil {
		panic(err)
	}
	return id
}

func mustParseAttemptID(s string) domain.AttemptID {
	id, err := domain.ParseAttemptID(s)
	if err != nil {
		panic(err)
	}
	return id
}

func mustParseNodeID(s string) domain.NodeID {
	id, err := domain.ParseNodeID(s)
	if err != nil {
		panic(err)
	}
	return id
}

func mustParseNamespaceID(s string) domain.NamespaceID {
	id, err := domain.ParseNamespaceID(s)
	if err != nil {
		panic(err)
	}
	return id
}

func mustParseAppID(s string) domain.AppID {
	id, err := domain.ParseAppID(s)
	if err != nil {
		panic(err)
	}
	return id
}

func mustParseAppVersionID(s string) domain.AppVersionID {
	id, err := domain.ParseAppVersionID(s)
	if err != nil {
		panic(err)
	}
	return id
}

// testCallContext builds a trusted CallContext for the default test user
// with sharing enabled.
func testCallContext() CallContext {
	return CallContext{
		SessionID:     testSession,
		UserID:        testUser,
		CWD:           domain.MustParsePath("/home/alice"),
		Policy:        domain.DefaultScopePolicy(),
		Namespaces:    testNamespaces(),
		UID:           1000,
		GID:           1000,
		TurnID:        testTurn,
		AttemptID:     testAttempt,
		PromptVersion: "prompt-v1",
		NowUnixMilli:  1_700_000_000_000,
	}
}

func testNamespaces() CallNamespaces {
	return CallNamespaces{
		Session:  mustParseNamespaceID("nsp_01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		User:     mustParseNamespaceID("nsp_01ARZ3NDEKTSV4RRFFQ69G5FAW"),
		Shared:   mustParseNamespaceID("nsp_01ARZ3NDEKTSV4RRFFQ69G5FAX"),
		Baseline: mustParseNamespaceID("nsp_01ARZ3NDEKTSV4RRFFQ69G5FAY"),
	}
}

// newTestRegistry wires a registry over fresh fakes and seeds the minimal
// directory skeleton the tests resolve paths against.
func newTestRegistry() (*Registry, *fakeWorld, *fakeEvents, *fakeRetrieval, *fakeContent, *fakeApps, *fakeSandbox, *fakeSummarizer, *fakeGenerator, *fakeRedactor) {
	world := newFakeWorld()
	ns := testNamespaces()
	for _, id := range []domain.NamespaceID{ns.Session, ns.User, ns.Shared, ns.Baseline} {
		world.add(id, domain.RootPath(), domain.NodeKindDir, domain.InitialRevision)
	}
	for _, dir := range []string{"/home", "/home/alice"} {
		world.add(ns.User, domain.MustParsePath(dir), domain.NodeKindDir, domain.InitialRevision)
	}
	for _, dir := range []string{"/etc", "/facts", "/facts/package"} {
		world.add(ns.Shared, domain.MustParsePath(dir), domain.NodeKindDir, domain.InitialRevision)
	}

	events := newFakeEvents()
	retrieval := newFakeRetrieval()
	content := newFakeContent()
	apps := newFakeApps()
	sandbox := &fakeSandbox{}
	summarizer := &fakeSummarizer{text: "summary of older history"}
	generator := &fakeGenerator{content: []byte("generated")}
	redactor := newFakeRedactor()
	registry := NewRegistry(Dependencies{
		World:      world,
		Events:     events,
		Retrieval:  retrieval,
		Content:    content,
		Apps:       apps,
		Sandbox:    sandbox,
		Summarizer: summarizer,
		Generator:  generator,
		Redactor:   redactor,
		Random:     newFakeRandom(42),
	})
	return registry, world, events, retrieval, content, apps, sandbox, summarizer, generator, redactor
}

// seedFile registers file content in the given store and creates the node.
func seedFile(t *testing.T, world *fakeWorld, content *fakeContent, ns domain.NamespaceID, path string, data []byte) domain.Node {
	t.Helper()
	ref, err := content.Put(context.Background(), data, "application/octet-stream")
	if err != nil {
		t.Fatalf("seeding content: %v", err)
	}
	node := world.add(ns, domain.MustParsePath(path), domain.NodeKindFile, domain.InitialRevision)
	node.Content = ref
	world.mu.Lock()
	world.nodes[node.ID] = node
	world.mu.Unlock()
	return node
}

// mustExec runs one tool call and fails the test on error.
func mustExec(t *testing.T, registry *Registry, call CallContext, name string, args any) json.RawMessage {
	t.Helper()
	rawArgs, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	result, err := registry.Execute(context.Background(), call, ToolCall{Name: name, Arguments: rawArgs})
	if err != nil {
		t.Fatalf("execute %s: %v", name, err)
	}
	return result.Result
}

// mustExecErr runs one tool call and fails the test unless it errors.
func mustExecErr(t *testing.T, registry *Registry, call CallContext, name string, args any) error {
	t.Helper()
	rawArgs, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	_, err = registry.Execute(context.Background(), call, ToolCall{Name: name, Arguments: rawArgs})
	if err == nil {
		t.Fatalf("execute %s: expected error, got success", name)
	}
	return err
}

// eventWithID builds an event record with a deterministic ID and both the
// envelope payload reference and the model-facing inline payload.
func eventWithID(t *testing.T, session domain.SessionID, seq uint64, kind domain.EventKind, payload any) domain.EventRecord {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return domain.EventRecord{
		Envelope: domain.EventEnvelope{
			SchemaVersion: domain.EventSchemaVersion,
			EventID:       fakeEventID(int(seq)),
			SessionID:     session,
			Sequence:      seq,
			Timestamp:     1_700_000_000_000 + int64(seq),
			Kind:          kind,
			Payload:       domain.PayloadRef{Inline: raw},
		},
		Payload: raw,
	}
}

// fakeEventID builds a deterministic, valid EventID from a counter.
func fakeEventID(n int) domain.EventID {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b[8:], uint64(n))
	id, err := domain.ParseEventID("evt_" + encodeID128(b))
	if err != nil {
		panic(err)
	}
	return id
}

// fakeNodeID builds a deterministic, valid NodeID from a counter.
func fakeNodeID(n int) domain.NodeID {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b[8:], uint64(n))
	id, err := domain.ParseNodeID("nod_" + encodeID128(b))
	if err != nil {
		panic(err)
	}
	return id
}

// assertMatchFields checks the common provenance fields of a search match.
func assertMatchFields(t *testing.T, match historyMatch, wantEventID domain.EventID, wantMatchType string) {
	t.Helper()
	if match.EventID != wantEventID {
		t.Errorf("match event ID = %v, want %v", match.EventID, wantEventID)
	}
	if match.MatchType != wantMatchType {
		t.Errorf("match type = %q, want %q", match.MatchType, wantMatchType)
	}
	if match.SessionID != testSession {
		t.Errorf("match session = %v, want %v", match.SessionID, testSession)
	}
}
