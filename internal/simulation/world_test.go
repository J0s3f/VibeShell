package simulation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// sharingOffCall returns a CallContext with the restricted policy: sharing
// disabled, so shared and cross-user reads and writes are denied.
func sharingOffCall() CallContext {
	call := testCallContext()
	call.Policy = domain.RestrictedScopePolicy()
	return call
}

// TestScopeDeniedWhenSharingOff verifies the tool boundary: with sharing
// disabled, shared-world reads are denied regardless of the model request,
// while the caller's own user scope stays readable.
func TestScopeDeniedWhenSharingOff(t *testing.T) {
	registry, world, _, _, content, _, _, _, _, _ := newTestRegistry()
	sharedNS := testNamespaces().Shared
	userNS := testNamespaces().User
	seedFile(t, world, content, sharedNS, "/etc/motd", []byte("shared motd"))
	seedFile(t, world, content, userNS, "/home/alice/.profile", []byte("user-profile"))

	call := sharingOffCall()

	// Shared read denied.
	err := mustExecErr(t, registry, call, "world.lookup", map[string]any{
		"scope": "shared",
		"path":  "/etc/motd",
	})
	if !domain.IsDeniedError(err) {
		t.Fatalf("shared lookup: error = %v, want denied", err)
	}
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Code != domain.CodeSharingDisabled {
		t.Fatalf("shared lookup: error = %v, want code %q", err, domain.CodeSharingDisabled)
	}

	// Shared write denied.
	err = mustExecErr(t, registry, call, "world.stage", map[string]any{
		"scope":   "shared",
		"path":    "/etc/motd2",
		"create":  true,
		"content": []byte("x"),
	})
	if !domain.IsDeniedError(err) {
		t.Fatalf("shared stage: error = %v, want denied", err)
	}

	// Shared materialization denied.
	err = mustExecErr(t, registry, call, "world.materialize", map[string]any{
		"scope": "shared",
		"path":  "/var/lib/new-thing",
	})
	if !domain.IsDeniedError(err) {
		t.Fatalf("shared materialize: error = %v, want denied", err)
	}

	// The caller's own user scope stays readable and writable.
	result := mustExec(t, registry, call, "world.lookup", map[string]any{
		"scope": "user",
		"path":  "/home/alice/.profile",
	})
	var lookup worldLookupResult
	if err := json.Unmarshal(result, &lookup); err != nil {
		t.Fatalf("unmarshal lookup: %v", err)
	}
	if lookup.Node.Kind != domain.NodeKindFile {
		t.Errorf("user lookup kind = %v, want file", lookup.Node.Kind)
	}
}

// TestSharingOffEnforcesEveryToolSurface is the PLAN 10.4 enforcement matrix:
// while sharing is disabled the service rejects shared-world reads and writes,
// transcript retrieval, summaries, and app access at the tool boundary,
// regardless of the model request. Each surface is denied by the injected
// policy, never by model-supplied scope names.
func TestSharingOffEnforcesEveryToolSurface(t *testing.T) {
	registry, world, events, retrieval, content, apps, _, _, _, _ := newTestRegistry()
	sharedNS := testNamespaces().Shared
	seedFile(t, world, content, sharedNS, "/etc/motd", []byte("shared motd"))
	seedFile(t, world, content, sharedNS, "/facts/package/tree", []byte("1.2.3"))
	seedApp(t, apps, testUser2, domain.ScopeUser)

	// A shared event recorded in another session.
	shared := eventWithID(t, testSession2, 100, domain.EventKindInputAccepted, domain.InputAcceptedPayload{
		Action: "command", Command: "shared-secret",
	})
	appended, err := events.Append(context.Background(), shared.Envelope)
	if err != nil {
		t.Fatalf("append shared event: %v", err)
	}
	retrieval.add(appended, domain.ScopeShared, testUser2)

	call := sharingOffCall()
	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"world.lookup shared", "world.lookup", map[string]any{"scope": "shared", "path": "/etc/motd"}},
		{"world.list shared", "world.list", map[string]any{"scope": "shared", "dir": "/etc"}},
		{"content.read shared", "content.read", map[string]any{"scope": "shared", "path": "/etc/motd"}},
		{"fact.lookup default shared", "fact.lookup", map[string]any{"category": "package", "key": "tree"}},
		{"world.stage shared", "world.stage", map[string]any{"scope": "shared", "path": "/etc/new", "create": true, "content": []byte("x")}},
		{"world.materialize shared", "world.materialize", map[string]any{"scope": "shared", "path": "/var/lib/x"}},
		{"history.search shared", "history.search", map[string]any{"query": "shared-secret", "match": "exact_command", "scope": "shared"}},
		{"history.context direct shared ID", "history.context", map[string]any{"event_id": appended.Envelope.EventID.String()}},
		{"app.lookup other user", "app.lookup", map[string]any{"app_id": testApp.String()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mustExecErr(t, registry, call, tc.tool, tc.args)
			if !domain.IsDeniedError(err) {
				t.Fatalf("%s while sharing off: error = %v, want denied", tc.tool, err)
			}
		})
	}
}

// TestWorldLookupAndList covers stat and directory listing, including
// pagination bounds.
func TestWorldLookupAndList(t *testing.T) {
	registry, world, _, _, content, _, _, _, _, _ := newTestRegistry()
	userNS := testNamespaces().User
	seedFile(t, world, content, userNS, "/home/alice/.profile", []byte("profile"))
	seedFile(t, world, content, userNS, "/home/alice/.bashrc", []byte("bashrc"))
	seedFile(t, world, content, userNS, "/home/alice/notes.txt", []byte("notes"))
	call := testCallContext()

	result := mustExec(t, registry, call, "world.lookup", map[string]any{
		"scope": "user",
		"path":  "/home/alice/notes.txt",
	})
	var lookup worldLookupResult
	if err := json.Unmarshal(result, &lookup); err != nil {
		t.Fatalf("unmarshal lookup: %v", err)
	}
	if lookup.Node.Kind != domain.NodeKindFile {
		t.Errorf("kind = %v, want file", lookup.Node.Kind)
	}
	if lookup.Node.Revision != domain.InitialRevision {
		t.Errorf("revision = %v, want initial", lookup.Node.Revision)
	}
	if lookup.Scope != domain.ScopeUser {
		t.Errorf("scope = %v, want user", lookup.Scope)
	}

	// Listing with a page size of two truncates and offers a cursor.
	result = mustExec(t, registry, call, "world.list", map[string]any{
		"scope": "user",
		"dir":   "/home/alice",
		"limit": 2,
	})
	var list worldListResult
	if err := json.Unmarshal(result, &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list.Nodes) != 2 {
		t.Fatalf("listed %d nodes, want 2", len(list.Nodes))
	}
	if !list.Truncated || list.NextCursor == "" {
		t.Errorf("expected truncation with next cursor, got truncated=%v cursor=%q", list.Truncated, list.NextCursor)
	}

	// The cursor returns the remaining child.
	result = mustExec(t, registry, call, "world.list", map[string]any{
		"scope":  "user",
		"dir":    "/home/alice",
		"limit":  2,
		"cursor": list.NextCursor,
	})
	var page2 worldListResult
	if err := json.Unmarshal(result, &page2); err != nil {
		t.Fatalf("unmarshal page 2: %v", err)
	}
	if len(page2.Nodes) != 1 {
		t.Fatalf("page 2 listed %d nodes, want 1", len(page2.Nodes))
	}
	if page2.Truncated {
		t.Error("page 2 should not be truncated")
	}

	// A file cannot be listed as a directory.
	err := mustExecErr(t, registry, call, "world.list", map[string]any{
		"scope": "user",
		"dir":   "/home/alice/notes.txt",
	})
	if !domain.IsValidationError(err) {
		t.Fatalf("listing a file: error = %v, want validation", err)
	}
}

// TestContentReadBounded verifies byte-range reads and truncation paging.
func TestContentReadBounded(t *testing.T) {
	registry, world, _, _, content, _, _, _, _, _ := newTestRegistry()
	userNS := testNamespaces().User
	data := []byte(strings.Repeat("abcdefghij", 100)) // 1000 bytes
	seedFile(t, world, content, userNS, "/home/alice/big.txt", data)
	call := testCallContext()

	// Default read is bounded by MaxContentReadBytes; here the file is
	// smaller, so the whole file comes back untruncated.
	result := mustExec(t, registry, call, "content.read", map[string]any{
		"scope": "user",
		"path":  "/home/alice/big.txt",
	})
	var read contentReadResult
	if err := json.Unmarshal(result, &read); err != nil {
		t.Fatalf("unmarshal read: %v", err)
	}
	if string(read.Content) != string(data) {
		t.Errorf("content length = %d, want %d", len(read.Content), len(data))
	}
	if read.Truncated {
		t.Error("read of a small file should not be truncated")
	}
	if read.Ref.Size != int64(len(data)) {
		t.Errorf("ref size = %d, want %d", read.Ref.Size, len(data))
	}

	// An explicit range returns exactly that window.
	result = mustExec(t, registry, call, "content.read", map[string]any{
		"scope":  "user",
		"path":   "/home/alice/big.txt",
		"offset": 10,
		"length": 20,
	})
	var window contentReadResult
	if err := json.Unmarshal(result, &window); err != nil {
		t.Fatalf("unmarshal window: %v", err)
	}
	if string(window.Content) != string(data[10:30]) {
		t.Errorf("window = %q, want %q", window.Content, data[10:30])
	}
	if window.Offset != 10 {
		t.Errorf("offset = %d, want 10", window.Offset)
	}
}

// TestWorldStageDoesNotCommit verifies staging has no immediate effect:
// the change set is returned, the world is untouched, and a later commit
// applies it.
func TestWorldStageDoesNotCommit(t *testing.T) {
	registry, world, _, _, _, _, _, _, _, _ := newTestRegistry()
	userNS := testNamespaces().User
	call := testCallContext()

	result := mustExec(t, registry, call, "world.stage", map[string]any{
		"scope":   "user",
		"path":    "/home/alice/new.txt",
		"create":  true,
		"content": []byte("hello"),
	})
	var staged worldStageResult
	if err := json.Unmarshal(result, &staged); err != nil {
		t.Fatalf("unmarshal staged: %v", err)
	}
	if len(staged.Staged.Mutations) != 1 {
		t.Fatalf("staged %d mutations, want 1", len(staged.Staged.Mutations))
	}
	if staged.Staged.Mutations[0].Type != domain.MutationCreate {
		t.Errorf("mutation type = %v, want create", staged.Staged.Mutations[0].Type)
	}
	if staged.Staged.TurnID != call.TurnID {
		t.Errorf("change set turn = %v, want %v", staged.Staged.TurnID, call.TurnID)
	}
	if staged.Staged.StagedCount != 1 {
		t.Errorf("staged count = %d, want 1", staged.Staged.StagedCount)
	}

	// No commit happened: the path is still missing and no change set was
	// recorded by the store.
	if len(world.commits) != 0 {
		t.Fatalf("world recorded %d commits, want 0", len(world.commits))
	}
	if _, err := world.LookupPath(context.Background(), userNS, domain.MustParsePath("/home/alice/new.txt")); !domain.IsNotFoundError(err) {
		t.Fatalf("staged path visible before commit: %v", err)
	}

	// Committing the staged change set makes it durable.
	stagedSets := registry.StagedChanges(call.TurnID)
	if len(stagedSets) != 1 {
		t.Fatalf("staged change sets = %d, want 1", len(stagedSets))
	}
	if _, err := world.Commit(context.Background(), stagedSets[0], call.Policy); err != nil {
		t.Fatalf("commit staged change set: %v", err)
	}
	node, err := world.LookupPath(context.Background(), userNS, domain.MustParsePath("/home/alice/new.txt"))
	if err != nil {
		t.Fatalf("path missing after commit: %v", err)
	}
	if node.Kind != domain.NodeKindFile {
		t.Errorf("kind = %v, want file", node.Kind)
	}
}

// TestWorldStageUpdateRequiresExpectedRev verifies optimistic concurrency:
// an update without an expected revision is rejected, and a stale revision
// fails at commit.
func TestWorldStageUpdateRequiresExpectedRev(t *testing.T) {
	registry, world, _, _, content, _, _, _, _, _ := newTestRegistry()
	userNS := testNamespaces().User
	node := seedFile(t, world, content, userNS, "/home/alice/file.txt", []byte("v1"))
	call := testCallContext()

	err := mustExecErr(t, registry, call, "world.stage", map[string]any{
		"scope":   "user",
		"path":    "/home/alice/file.txt",
		"content": []byte("v2"),
	})
	if !domain.IsValidationError(err) {
		t.Fatalf("update without expected_rev: error = %v, want validation", err)
	}

	result := mustExec(t, registry, call, "world.stage", map[string]any{
		"scope":        "user",
		"path":         "/home/alice/file.txt",
		"content":      []byte("v2"),
		"expected_rev": int(node.Revision),
	})
	var staged worldStageResult
	if err := json.Unmarshal(result, &staged); err != nil {
		t.Fatalf("unmarshal staged: %v", err)
	}
	if staged.Staged.Mutations[0].Type != domain.MutationUpdate {
		t.Errorf("mutation type = %v, want update", staged.Staged.Mutations[0].Type)
	}
	if staged.Staged.Mutations[0].ExpectedRev != node.Revision {
		t.Errorf("expected rev = %v, want %v", staged.Staged.Mutations[0].ExpectedRev, node.Revision)
	}

	// A stale expected revision fails at commit time.
	stagedSets := registry.StagedChanges(call.TurnID)
	if len(stagedSets) != 1 {
		t.Fatalf("staged change sets = %d, want 1", len(stagedSets))
	}
	stale := stagedSets[0]
	stale.Mutations[0].ExpectedRev = node.Revision + 99
	if _, err := world.Commit(context.Background(), stale, call.Policy); !domain.IsConflictError(err) {
		t.Fatalf("stale commit: error = %v, want conflict", err)
	}
}

// TestWorldMaterializeUnknownPath verifies that materializing an unexplored
// path stages the path and its missing parents as one change set without
// any immediate effect, and that an existing path is returned unchanged.
func TestWorldMaterializeUnknownPath(t *testing.T) {
	registry, world, _, _, _, _, _, _, generator, _ := newTestRegistry()
	userNS := testNamespaces().User
	call := testCallContext()

	result := mustExec(t, registry, call, "world.materialize", map[string]any{
		"scope": "user",
		"path":  "/home/alice/projects/demo/main.go",
		"hint":  "go run from /home/alice/projects/demo",
	})
	var materialized worldMaterializeResult
	if err := json.Unmarshal(result, &materialized); err != nil {
		t.Fatalf("unmarshal materialized: %v", err)
	}
	if materialized.Existed {
		t.Error("path should not exist before materialization")
	}
	if materialized.Staged == nil {
		t.Fatal("expected a staged change set")
	}
	// The target and both missing parents are staged.
	if len(materialized.Staged.Mutations) != 3 {
		t.Fatalf("staged %d mutations, want 3 (target + 2 parents)", len(materialized.Staged.Mutations))
	}
	var target *stagedMutation
	for i := range materialized.Staged.Mutations {
		if materialized.Staged.Mutations[i].Path == domain.MustParsePath("/home/alice/projects/demo/main.go") {
			target = &materialized.Staged.Mutations[i]
		}
	}
	if target == nil {
		t.Fatal("target path missing from staged change set")
	}
	if target.Kind != domain.NodeKindFile {
		t.Errorf("target kind = %v, want file", target.Kind)
	}
	if target.ContentSize == 0 {
		t.Error("target content reference is empty")
	}

	// The generator was consulted for the target path.
	if len(generator.calls) != 1 || generator.calls[0] != domain.MustParsePath("/home/alice/projects/demo/main.go") {
		t.Errorf("generator calls = %v, want the target path", generator.calls)
	}

	// No immediate effect: nothing was committed and the path is still
	// missing.
	if len(world.commits) != 0 {
		t.Fatalf("world recorded %d commits, want 0", len(world.commits))
	}
	if _, err := world.LookupPath(context.Background(), userNS, domain.MustParsePath("/home/alice/projects/demo/main.go")); !domain.IsNotFoundError(err) {
		t.Fatalf("materialized path visible before commit: %v", err)
	}

	// Committing the staged change set materializes the whole chain.
	stagedSets := registry.StagedChanges(call.TurnID)
	if len(stagedSets) != 1 {
		t.Fatalf("staged change sets = %d, want 1", len(stagedSets))
	}
	if _, err := world.Commit(context.Background(), stagedSets[0], call.Policy); err != nil {
		t.Fatalf("commit materialized change set: %v", err)
	}
	for _, p := range []string{"/home/alice/projects", "/home/alice/projects/demo", "/home/alice/projects/demo/main.go"} {
		if _, err := world.LookupPath(context.Background(), userNS, domain.MustParsePath(p)); err != nil {
			t.Errorf("path %q missing after commit: %v", p, err)
		}
	}

	// Materializing an existing path is idempotent: no new staging.
	result = mustExec(t, registry, call, "world.materialize", map[string]any{
		"scope": "user",
		"path":  "/home/alice/projects/demo/main.go",
	})
	var again worldMaterializeResult
	if err := json.Unmarshal(result, &again); err != nil {
		t.Fatalf("unmarshal second materialize: %v", err)
	}
	if !again.Existed || again.Staged != nil {
		t.Errorf("existing path: existed=%v staged=%v, want existed with no staging", again.Existed, again.Staged)
	}
}

// TestFactLookup covers typed fact retrieval, including the not-found case
// and the facts-root escape guard.
func TestFactLookup(t *testing.T) {
	registry, world, _, _, content, _, _, _, _, _ := newTestRegistry()
	sharedNS := testNamespaces().Shared
	seedFile(t, world, content, sharedNS, "/facts/package/tree", []byte("1.2.3"))
	call := testCallContext()

	result := mustExec(t, registry, call, "fact.lookup", map[string]any{
		"category": "package",
		"key":      "tree",
	})
	var fact factLookupResult
	if err := json.Unmarshal(result, &fact); err != nil {
		t.Fatalf("unmarshal fact: %v", err)
	}
	if !fact.Found {
		t.Fatal("fact should be found")
	}
	if string(fact.Content) != "1.2.3" {
		t.Errorf("fact content = %q, want %q", fact.Content, "1.2.3")
	}
	if fact.Path != domain.MustParsePath("/facts/package/tree") {
		t.Errorf("fact path = %v", fact.Path)
	}
	if fact.Scope != domain.ScopeShared {
		t.Errorf("fact scope = %v, want shared", fact.Scope)
	}

	// Unknown fact: found=false, not an error.
	result = mustExec(t, registry, call, "fact.lookup", map[string]any{
		"category": "package",
		"key":      "nonexistent",
	})
	var missing factLookupResult
	if err := json.Unmarshal(result, &missing); err != nil {
		t.Fatalf("unmarshal missing fact: %v", err)
	}
	if missing.Found {
		t.Error("unknown fact should not be found")
	}

	// A key that tries to escape the facts root is rejected.
	err := mustExecErr(t, registry, call, "fact.lookup", map[string]any{
		"category": "package",
		"key":      "../../etc/passwd",
	})
	if !domain.IsValidationError(err) {
		t.Fatalf("escaping key: error = %v, want validation", err)
	}

	// An unknown category is rejected.
	err = mustExecErr(t, registry, call, "fact.lookup", map[string]any{
		"category": "kernel",
		"key":      "version",
	})
	if !domain.IsValidationError(err) {
		t.Fatalf("unknown category: error = %v, want validation", err)
	}
}
