package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"j0s.at/vibeshell/internal/domain"
)

// The doubles in this file stand in for every outbound port and for the engine
// and tool executor, so the coordinator's rules are exercised without any
// adapter, database, provider, or terminal. They are deliberately observable:
// each one records what it was asked to do, so a test can assert ordering and
// counts instead of only the final state.

// orderLog records named milestones in the order they happened. It is how a
// test proves an ordering rule (for example that the world commit precedes the
// first byte handed to the terminal) without inspecting implementation state.
type orderLog struct {
	mu    sync.Mutex
	marks []string
}

func newOrderLog() *orderLog { return &orderLog{} }

func (l *orderLog) mark(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.marks = append(l.marks, name)
}

// marks_ returns the recorded milestones. The trailing underscore keeps the
// accessor from colliding with the field name above.
func (l *orderLog) marks_() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.marks...)
}

// indexOf returns the position of the first occurrence of a milestone, or -1.
func (l *orderLog) indexOf(name string) int {
	for i, mark := range l.marks_() {
		if mark == name {
			return i
		}
	}
	return -1
}

// stubClock is a deterministic clock. It advances by one millisecond per read so
// recorded timestamps are ordered without depending on wall-clock time.
type stubClock struct {
	mu        sync.Mutex
	milli     int64
	monotonic int64
}

func newStubClock(startMilli int64) *stubClock {
	return &stubClock{milli: startMilli, monotonic: 1_000_000}
}

func (c *stubClock) NowUnixMilli() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.milli++
	return c.milli
}

func (c *stubClock) MonotonicNanos() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.monotonic++
	return c.monotonic
}

// countingRandom mints distinct valid identities by counting. It never repeats
// a value, which is what the coordinator's identity contract requires.
type countingRandom struct {
	mu      sync.Mutex
	counter uint64
}

func (r *countingRandom) Bytes(n int) ([]byte, error) {
	if n <= 0 {
		return nil, errors.New("randomness requested a non-positive length")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	raw := make([]byte, n)
	for i := range raw {
		raw[i] = byte(r.counter >> (8 * (i % 8)))
	}
	// A non-zero leading byte keeps the first base32 character inside the
	// Crockford alphabet the identity format requires.
	raw[0] |= 1
	r.counter++
	return raw, nil
}

func (r *countingRandom) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counter++
	return int(r.counter % uint64(n))
}

// recordedEvent is one appended research event plus its decoded payload.
type recordedEvent struct {
	Envelope domain.EventEnvelope
	Payload  json.RawMessage
}

// stubEventStore records the research record in append order.
//
// The two fault injectors model different failures: failWith makes every
// subsequent append fail, and failAfter allows a fixed number of further
// appends to succeed first, so a test can break durable recording in the middle
// of a turn rather than only at accept time.
type stubEventStore struct {
	mu         sync.Mutex
	events     []recordedEvent
	failAppend error
	// allowed is how many more appends succeed before failAppend takes effect.
	allowed int

	// delay blocks every append until it is closed, so a test can observe that
	// a slow store cannot wedge cancellation.
	delay <-chan struct{}
}

func newStubEventStore() *stubEventStore { return &stubEventStore{} }

func (s *stubEventStore) Append(ctx context.Context, evt domain.EventEnvelope) (domain.EventRecord, error) {
	// The real store refuses a kind outside the registered vocabulary, so the
	// double refuses it too: otherwise a journal that emits an unregistered kind
	// would pass here and lose the record on write.
	if !domain.IsValidEventKind(evt.Kind) {
		return domain.EventRecord{}, domain.NewValidationError(domain.CodeInvalidInput,
			"stub event store refuses unregistered event kind", map[string]string{"kind": string(evt.Kind)})
	}
	if s.delay != nil {
		select {
		case <-s.delay:
		case <-ctx.Done():
			return domain.EventRecord{}, ctx.Err()
		}
	}
	s.mu.Lock()
	if s.failAppend != nil {
		if s.allowed > 0 {
			s.allowed--
		} else {
			failure := s.failAppend
			s.mu.Unlock()
			return domain.EventRecord{}, failure
		}
	}
	s.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	record := domain.EventRecord{Envelope: evt, Payload: evt.Payload.Inline, Provenance: evt.Provenance}
	s.events = append(s.events, recordedEvent{Envelope: evt, Payload: evt.Payload.Inline})
	return record, nil
}

func (s *stubEventStore) GetByID(context.Context, domain.EventID) (domain.EventRecord, error) {
	return domain.EventRecord{}, errors.New("stub event store does not implement GetByID")
}

func (s *stubEventStore) List(context.Context, domain.SessionID, uint64, int) ([]domain.EventRecord, error) {
	return nil, errors.New("stub event store does not implement List")
}

// count returns how many events were recorded, which a test uses to choose where
// to break durable recording.
func (s *stubEventStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func (s *stubEventStore) kinds() []domain.EventKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	kinds := make([]domain.EventKind, 0, len(s.events))
	for _, event := range s.events {
		kinds = append(kinds, event.Envelope.Kind)
	}
	return kinds
}

// transitions returns the recorded turn state transitions for one turn, in
// order. It is the observable history the lifecycle tests assert on.
func (s *stubEventStore) transitions(turn domain.TurnID) []domain.TurnTransitionPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	payloads := make([]domain.TurnTransitionPayload, 0, len(s.events))
	for _, event := range s.events {
		if event.Envelope.Kind != domain.EventKindTurnTransition {
			continue
		}
		if event.Envelope.TurnID == nil || *event.Envelope.TurnID != turn {
			continue
		}
		var payload domain.TurnTransitionPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

// transitionPath returns only the real state changes of one turn, dropping the
// self-transitions the coordinator records as lifecycle notes.
func (s *stubEventStore) transitionPath(turn domain.TurnID) []TurnState {
	payloads := s.transitions(turn)
	path := make([]TurnState, 0, len(payloads))
	for _, payload := range payloads {
		if payload.From != payload.To {
			path = append(path, payload.To)
		}
	}
	return path
}

// notedReasons returns the reasons of one turn's lifecycle notes, so a test can
// assert that a discarded late response or a skipped replayed commit was
// recorded rather than merely observed by its side effect.
func (s *stubEventStore) notedReasons(turn domain.TurnID) []string {
	var reasons []string
	for _, payload := range s.transitions(turn) {
		if payload.From == payload.To && payload.Reason != "" {
			reasons = append(reasons, payload.Reason)
		}
	}
	return reasons
}

// notedContaining returns the recorded reasons of one turn containing a phrase.
func (s *stubEventStore) notedContaining(turn domain.TurnID, phrase string) []string {
	var matches []string
	for _, reason := range s.notedReasons(turn) {
		if strings.Contains(reason, phrase) {
			matches = append(matches, reason)
		}
	}
	return matches
}

// payloadOf returns the decoded payload of the first event of a kind, or false.
func (s *stubEventStore) payloadOf(kind domain.EventKind, into domain.EventPayload) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range s.events {
		if event.Envelope.Kind != kind {
			continue
		}
		return json.Unmarshal(event.Payload, into) == nil
	}
	return false
}

// payloadFieldsOf returns the first event of a kind decoded into a generic map.
// A test uses it for a domain payload whose optional identity fields may be
// absent, because decoding such a payload into its typed form rejects the empty
// identity the domain format does not allow.
func (s *stubEventStore) payloadFieldsOf(kind domain.EventKind) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range s.events {
		if event.Envelope.Kind != kind {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(event.Payload, &fields); err != nil {
			return nil, false
		}
		return fields, true
	}
	return nil, false
}

func (s *stubEventStore) indexOfKind(kind domain.EventKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, event := range s.events {
		if event.Envelope.Kind == kind {
			return i
		}
	}
	return -1
}

func (s *stubEventStore) countOfKind(kind domain.EventKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, event := range s.events {
		if event.Envelope.Kind == kind {
			count++
		}
	}
	return count
}

// failWith makes every further append fail.
func (s *stubEventStore) failWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAppend = err
	s.allowed = 0
}

// failAfter allows allowed more appends to succeed, then fails every later one.
func (s *stubEventStore) failAfter(allowed int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAppend = err
	s.allowed = allowed
}

// commitRecord is one staged commit the world double accepted.
type commitRecord struct {
	ChangeSet domain.ChangeSet
	Revision  domain.Revision
	Policy    domain.ScopePolicy
}

// stubWorldStore is the durable world shared by every session of a process. It
// counts accepted mutations so a test can prove a replayed commit applied
// nothing twice.
type stubWorldStore struct {
	mu       sync.Mutex
	revision domain.Revision
	commits  []commitRecord
	// conflicts fails the next commit calls with these errors in order, so a
	// test can drive the rebase path.
	conflicts  []error
	commitHook func(call int, cs domain.ChangeSet) error
	calls      int
	order      *orderLog
}

func newStubWorldStore(order *orderLog) *stubWorldStore {
	return &stubWorldStore{revision: domain.InitialRevision, order: order}
}

func (s *stubWorldStore) GetNode(context.Context, domain.NamespaceID, domain.NodeID) (domain.Node, error) {
	return domain.Node{}, errors.New("stub world store does not implement GetNode")
}

func (s *stubWorldStore) LookupPath(context.Context, domain.NamespaceID, domain.ValidPath) (domain.Node, error) {
	return domain.Node{}, errors.New("stub world store does not implement LookupPath")
}

func (s *stubWorldStore) ListDirectory(context.Context, domain.NamespaceID, domain.NodeID, int, string) ([]domain.Node, string, error) {
	return nil, "", errors.New("stub world store does not implement ListDirectory")
}

func (s *stubWorldStore) Commit(ctx context.Context, cs domain.ChangeSet, policy domain.ScopePolicy) (domain.Revision, error) {
	s.order.mark("world.commit")
	s.mu.Lock()
	s.calls++
	call := s.calls
	hook := s.commitHook
	if len(s.conflicts) > 0 {
		next := s.conflicts[0]
		s.conflicts = s.conflicts[1:]
		s.mu.Unlock()
		return 0, next
	}
	s.mu.Unlock()

	if hook != nil {
		if err := hook(call, cs); err != nil {
			return 0, err
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision = s.revision.Next()
	s.commits = append(s.commits, commitRecord{ChangeSet: cs, Revision: s.revision, Policy: policy})
	return s.revision, nil
}

// conflictNext queues one conflict failure for the next Commit call.
func (s *stubWorldStore) conflictNext(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conflicts = append(s.conflicts, err)
}

// acceptedMutations counts the mutations the durable world actually applied.
func (s *stubWorldStore) acceptedMutations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, commit := range s.commits {
		total += len(commit.ChangeSet.Mutations)
	}
	return total
}

func (s *stubWorldStore) commitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.commits)
}

func (s *stubWorldStore) lastRevision() domain.Revision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

func (s *stubWorldStore) lastCommit() (commitRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.commits) == 0 {
		return commitRecord{}, false
	}
	return s.commits[len(s.commits)-1], true
}

// stubContentStore returns immutable references for stored bytes.
type stubContentStore struct {
	mu    sync.Mutex
	blobs map[string][]byte
	next  int
	err   error
}

func newStubContentStore() *stubContentStore {
	return &stubContentStore{blobs: make(map[string][]byte)}
}

func (s *stubContentStore) Put(ctx context.Context, data []byte, mediaType string) (domain.ContentRef, error) {
	if err := ctx.Err(); err != nil {
		return domain.ContentRef{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return domain.ContentRef{}, s.err
	}
	s.next++
	hash := testContentID(s.next)
	s.blobs[hash.String()] = append([]byte(nil), data...)
	return domain.ContentRef{Hash: hash, Size: int64(len(data)), MediaType: mediaType}, nil
}

func (s *stubContentStore) Get(_ context.Context, ref domain.ContentRef, offset, length int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	blob, ok := s.blobs[ref.Hash.String()]
	if !ok {
		return nil, errors.New("stub content store has no such content")
	}
	if offset > int64(len(blob)) {
		offset = int64(len(blob))
	}
	end := int64(len(blob))
	if length > 0 && offset+length < end {
		end = offset + length
	}
	return append([]byte(nil), blob[offset:end]...), nil
}

func (s *stubContentStore) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next
}

// stubRenderer accepts text and views without a real terminal. It records the
// frames and text it was asked to emit so emission ordering can be asserted.
type stubRenderer struct {
	mu      sync.Mutex
	frames  int
	texts   []string
	views   []string
	textErr error
	order   *orderLog
}

func newStubRenderer(order *orderLog) *stubRenderer { return &stubRenderer{order: order} }

func (r *stubRenderer) RenderView(_ context.Context, _ domain.SessionID, view domain.AppView) (domain.ContentRef, int64, error) {
	if err := domain.ValidateView(view); err != nil {
		return domain.ContentRef{}, 0, domain.WrapError(err, domain.CategoryValidation, domain.CodeInvalidAppView, "view is not valid")
	}
	r.order.mark("terminal.frame")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames++
	r.views = append(r.views, view.Mode)
	return domain.ContentRef{Hash: testContentID(1000 + r.frames), Size: int64(len(view.Mode)), MediaType: "application/x-vibeshell-frame"}, int64(len(view.Mode)), nil
}

func (r *stubRenderer) WriteText(_ context.Context, _ domain.SessionID, text string) (int64, error) {
	r.order.mark("terminal.text")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.textErr != nil {
		return 0, r.textErr
	}
	r.texts = append(r.texts, text)
	return int64(len(text)), nil
}

func (r *stubRenderer) renderedFrames() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.frames
}

func (r *stubRenderer) writtenText() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.texts...)
}

// stubSnapshots serves configuration snapshots. reload publishes a new revision
// so a test can prove a running turn keeps the snapshot it pinned.
type stubSnapshots struct {
	mu       sync.Mutex
	current  ConfigSnapshot
	revision int64
	prompts  int
}

func newStubSnapshots() *stubSnapshots {
	return &stubSnapshots{current: testSnapshot(), revision: 1}
}

func (s *stubSnapshots) CurrentSnapshot(context.Context) (ConfigSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts++
	return s.current, nil
}

// reload publishes a new configuration and prompt revision.
func (s *stubSnapshots) reload() ConfigSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision++
	s.current = testSnapshot()
	s.current.ConfigVersion = s.revision
	s.current.PromptVersion = fmt.Sprintf("prompt-v%d", s.revision)
	return s.current
}

// reloadWithPolicy publishes a new configuration revision carrying the given
// scope policy, so a test can present a live sharing change.
func (s *stubSnapshots) reloadWithPolicy(policy domain.ScopePolicy) ConfigSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision++
	s.current = testSnapshot()
	s.current.ConfigVersion = s.revision
	s.current.PromptVersion = fmt.Sprintf("prompt-v%d", s.revision)
	s.current.ScopePolicy = policy
	return s.current
}

func (s *stubSnapshots) currentRevision() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current.ConfigVersion
}

func (s *stubSnapshots) promptsIssued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prompts
}

// TurnScript is a test-authored script for one turn. Preparations answers
// successive Prepare calls within the turn (the last entry repeats), and Steps
// answers successive generations. Running past the end of Steps is an error, so
// a coordinator that retries more than a test expected fails loudly.
type TurnScript struct {
	Preparations []PreparedTurn
	PrepareErr   error
	Steps        []StepOutcome
	StepErrors   []error
	// StepFunc, when set, answers each generation from the request it received.
	// A test uses it when the scripted outcome must carry the real turn or
	// generation identity, which the coordinator mints after Accept returns.
	StepFunc func(GenerationInput, int) (StepOutcome, error)
	// Blocks maps a generation index to a gate that generation waits on.
	Blocks map[int]chan struct{}
	// Stubborn generations ignore the cancelled context and still answer. It
	// models a provider or SDK call that returns its result anyway, which is how
	// a late response from a cancelled attempt actually reaches the coordinator.
	Stubborn bool
}

// textTurn is the common shorthand for a scripted turn that answers its only
// generation with plain text, naming its own turn identity.
func textTurn(text string) TurnScript {
	return TurnScript{StepFunc: func(in GenerationInput, _ int) (StepOutcome, error) {
		return candidateStep(textCandidate(in.Request.Turn, text)), nil
	}}
}

// candidateStep is the common shorthand for a scripted generation that returns
// a complete structured outcome.
func candidateStep(candidate *TurnCandidate) StepOutcome {
	return StepOutcome{Phase: StepCandidate, Candidate: candidate}
}

// toolStep is the common shorthand for a scripted generation that requests more
// tool work.
func toolStep(name string) StepOutcome {
	return StepOutcome{Phase: StepAwaitingTool, ToolCalls: []ToolCallRequest{{Name: name}}}
}

// turnState is one turn's recorded state inside the engine double.
type turnState struct {
	turnID domain.TurnID

	prepareCount   int
	prepareReqs    []TurnRequest
	generationReqs []TurnRequest
	generationTool [][]ToolCallResult
}

// scriptedEngine answers Prepare and Generate from a per-turn script, so a test
// decides exactly how many preparations and generations each turn takes and what
// each one returns. Turns are addressed by ordinal in the order their first
// Prepare arrives, so a test can script a turn before it starts. Every call is
// recorded, which is how snapshot pinning, generation identity, tool-result
// handback, and context re-assembly after a rebase are observed.
type scriptedEngine struct {
	mu      sync.Mutex
	ordered []*turnState
	byTurn  map[domain.TurnID]*turnState
	scripts []TurnScript
	order   *orderLog
}

func newScriptedEngine(order *orderLog) *scriptedEngine {
	return &scriptedEngine{byTurn: make(map[domain.TurnID]*turnState), order: order}
}

// scriptTurn installs the script for the ordinal-th turn that reaches the engine
// (0-based).
func (e *scriptedEngine) scriptTurn(ordinal int, script TurnScript) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for len(e.scripts) <= ordinal {
		e.scripts = append(e.scripts, TurnScript{})
	}
	e.scripts[ordinal] = script
}

// scriptFor returns a turn's script. An unscripted turn yields an empty script,
// so its first unexpected generation fails loudly instead of panicking.
func (e *scriptedEngine) scriptFor(ordinal int) TurnScript {
	if ordinal < len(e.scripts) {
		return e.scripts[ordinal]
	}
	return TurnScript{}
}

// blockGeneration makes the nth generation of the ordinal-th turn wait until the
// returned channel is closed. That is how a test opens the window a Ctrl-C or a
// late response needs.
func (e *scriptedEngine) blockGeneration(ordinal, generation int) chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	for len(e.scripts) <= ordinal {
		e.scripts = append(e.scripts, TurnScript{})
	}
	if e.scripts[ordinal].Blocks == nil {
		e.scripts[ordinal].Blocks = make(map[int]chan struct{})
	}
	gate := make(chan struct{})
	e.scripts[ordinal].Blocks[generation] = gate
	return gate
}

// stateFor returns the recorded state of one turn, creating it on first sight.
func (e *scriptedEngine) stateFor(turn domain.TurnID) *turnState {
	if state, ok := e.byTurn[turn]; ok {
		return state
	}
	state := &turnState{turnID: turn}
	e.ordered = append(e.ordered, state)
	e.byTurn[turn] = state
	return state
}

// live returns the recorded state of the ordinal-th turn, or nil when that turn
// never reached the engine.
func (e *scriptedEngine) live(ordinal int) *turnState {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ordinal < 0 || ordinal >= len(e.ordered) {
		return nil
	}
	return e.ordered[ordinal]
}

// Prepare implements TurnEngine.
func (e *scriptedEngine) Prepare(_ context.Context, req TurnRequest) (PreparedTurn, error) {
	e.mu.Lock()
	state := e.stateFor(req.Turn)
	script := e.scriptFor(len(e.ordered) - 1)
	index := state.prepareCount
	state.prepareCount++
	state.prepareReqs = append(state.prepareReqs, req)
	e.mu.Unlock()

	e.order.mark("engine.prepare")
	if script.PrepareErr != nil {
		return PreparedTurn{}, script.PrepareErr
	}
	if len(script.Preparations) == 0 {
		return PreparedTurn{ContextBytes: 1}, nil
	}
	if index >= len(script.Preparations) {
		index = len(script.Preparations) - 1
	}
	return script.Preparations[index], nil
}

// Generate implements TurnEngine.
func (e *scriptedEngine) Generate(ctx context.Context, in GenerationInput) (StepOutcome, error) {
	e.mu.Lock()
	state := e.stateFor(in.Request.Turn)
	script := e.scriptFor(len(e.ordered) - 1)
	index := len(state.generationReqs)
	state.generationReqs = append(state.generationReqs, in.Request)
	state.generationTool = append(state.generationTool, in.ToolResults)
	gate := script.Blocks[index]
	e.mu.Unlock()

	e.order.mark("engine.generate")
	if gate != nil {
		if script.Stubborn {
			<-gate
		} else {
			select {
			case <-gate:
			case <-ctx.Done():
				return StepOutcome{}, ctx.Err()
			}
		}
	}
	if script.StepFunc != nil {
		return script.StepFunc(in, index)
	}
	if index >= len(script.Steps) {
		return StepOutcome{}, fmt.Errorf("scripted engine ran past its script at generation %d of turn %s", index, in.Request.Turn)
	}
	if index < len(script.StepErrors) && script.StepErrors[index] != nil {
		return StepOutcome{}, script.StepErrors[index]
	}
	return script.Steps[index], nil
}

// generationsOf returns the ordinal-th turn's generation requests, in order.
func (e *scriptedEngine) generationsOf(ordinal int) []TurnRequest {
	state := e.live(ordinal)
	if state == nil {
		return nil
	}
	return append([]TurnRequest(nil), state.generationReqs...)
}

// generationCount returns how many generations the ordinal-th turn ran.
func (e *scriptedEngine) generationCount(ordinal int) int {
	return len(e.generationsOf(ordinal))
}

// prepareCount returns how many times the ordinal-th turn's context was
// assembled. A conflict rebase must raise this to 2; a repair retry must not.
func (e *scriptedEngine) prepareCount(ordinal int) int {
	state := e.live(ordinal)
	if state == nil {
		return 0
	}
	return state.prepareCount
}

// prepareRequests returns the requests the ordinal-th turn's Prepare calls
// received, in order.
func (e *scriptedEngine) prepareRequests(ordinal int) []TurnRequest {
	state := e.live(ordinal)
	if state == nil {
		return nil
	}
	return append([]TurnRequest(nil), state.prepareReqs...)
}

// generationRequestsForSession returns the generation requests one session's
// turns produced, in order. It is how a test proves two sessions did not share
// their turn work.
func (e *scriptedEngine) generationRequestsForSession(session domain.SessionID) []TurnRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	var all []TurnRequest
	for _, state := range e.ordered {
		for _, request := range state.generationReqs {
			if request.Session == session {
				all = append(all, request)
			}
		}
	}
	return all
}

// toolResultsSeen returns, per generation across all turns, the tool results
// that generation received.
func (e *scriptedEngine) toolResultsSeen() [][]ToolCallResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	var all [][]ToolCallResult
	for _, state := range e.ordered {
		all = append(all, state.generationTool...)
	}
	return all
}

// stubTools answers tool batches from a script. It records the batches so a test
// can assert what the engine requested and under which snapshot.
type stubTools struct {
	mu      sync.Mutex
	result  ToolBatchResult
	err     error
	batches []ToolBatch
}

func newStubTools() *stubTools { return &stubTools{} }

func (s *stubTools) Execute(_ context.Context, batch ToolBatch) (ToolBatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, batch)
	return s.result, s.err
}

func (s *stubTools) batchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.batches)
}

func (s *stubTools) batchAt(index int) (ToolBatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.batches) {
		return ToolBatch{}, false
	}
	return s.batches[index], true
}

// testSnapshot is the snapshot a turn pins under test.
func testSnapshot() ConfigSnapshot {
	return ConfigSnapshot{
		ConfigVersion:    1,
		PromptVersion:    "prompt-v1",
		CatalogueVersion: "catalogue-v1",
		ScopePolicy:      domain.DefaultScopePolicy(),
		TurnDeadlineMs:   30_000,
		MaxAttempts:      3,
		MaxRebases:       2,
	}
}

// sessionOf returns the runtime behind a Shell handle. It lets a test drive the
// commit step directly, which is the only way to present the same logical commit
// twice inside one turn: the coordinator itself never offers that.
func (h *harness) sessionOf(t *testing.T, shell Shell) *session {
	t.Helper()
	runtime, ok := shell.(*session)
	if !ok {
		t.Fatalf("expected the harness coordinator to hand out *session, got %T", shell)
	}
	return runtime
}

// waitBudget bounds how long a test waits for a coordinator goroutine. It is
// generous compared with the deterministic doubles, so a failure means a real
// deadlock rather than a slow machine.
const waitBudget = 10 * time.Second

// testLimits keeps queues and budgets small so tests stay fast and bounded.
func testLimits() Limits {
	limits := DefaultLimits()
	limits.OutputQueue = 8
	limits.MaxQueuedTurns = 2
	limits.MaxAttempts = 2
	limits.MaxRebases = 1
	limits.MaxRepairs = 1
	limits.TurnDeadline = 20 * time.Second
	limits.JournalTimeout = time.Second
	return limits
}

// testNamespace is a namespace every test change set writes into.
var testNamespace = mustNamespaceID("nsp_01ARZ3NDEKTSV4RRFFQ69G5FAV")

// testPrincipal is the authenticated identity used across the session tests.
var testPrincipal = mustUserID("usr_01ARZ3NDEKTSV4RRFFQ69G5FAV")

// otherPrincipal is a second authenticated identity, so a test can prove a
// session never accepts another user's input.
var otherPrincipal = mustUserID("usr_01ARZ3NDEKTSV4RRFFQ69G5FBB")

func mustUserID(s string) domain.UserID {
	id, err := domain.ParseUserID(s)
	if err != nil {
		panic(err)
	}
	return id
}

func mustNamespaceID(s string) domain.NamespaceID {
	id, err := domain.ParseNamespaceID(s)
	if err != nil {
		panic(err)
	}
	return id
}

func testContentID(n int) domain.ContentID {
	return mustContentID("cnt_" + encodeBase32([]byte{
		byte(n >> 56), byte(n >> 48), byte(n >> 40), byte(n >> 32),
		byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n),
		byte(n >> 3), byte(n << 5), byte(n >> 11), byte(n >> 19),
		byte(n >> 27), byte(n << 1), byte(n >> 35), byte(n >> 43),
	}))
}

func mustContentID(s string) domain.ContentID {
	id, err := domain.ParseContentID(s)
	if err != nil {
		panic(err)
	}
	return id
}
