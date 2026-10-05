package worldpaths

import (
	"sync"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

func mustNode(t *testing.T, s string) domain.NodeID {
	t.Helper()
	id, err := domain.ParseNodeID(s)
	if err != nil {
		t.Fatalf("ParseNodeID: %v", err)
	}
	return id
}

// TestConcurrentFirstMaterialization races two simulated writers that both
// read absence, then commit the same shared path. Exactly one must win and
// the loser must rebase to the winner's committed version.
func TestConcurrentFirstMaterialization(t *testing.T) {
	store := NewSimStore()
	const path = "/shared/first.txt"
	idA := mustNode(t, "nod_0123456789ABCDEFGHJKMNPQRS")
	idB := mustNode(t, "nod_0123456789ABCDEFGHJKMNPQRT")

	// Barrier: both writers snapshot before either commits.
	release := make(chan struct{})
	var snapA struct {
		abs bool
		dir domain.Revision
	}
	var snapB struct {
		abs bool
		dir domain.Revision
	}
	_, foundA, dirA, depsA := store.Snapshot(path)
	_, foundB, dirB, depsB := store.Snapshot(path)
	snapA = struct {
		abs bool
		dir domain.Revision
	}{!foundA, dirA}
	snapB = struct {
		abs bool
		dir domain.Revision
	}{!foundB, dirB}
	if len(depsA) != 1 || !depsA[0].IsAbsence {
		t.Fatalf("writer A must record an absence read dependency, got %+v", depsA)
	}
	if len(depsB) != 1 || !depsB[0].IsAbsence {
		t.Fatalf("writer B must record an absence read dependency, got %+v", depsB)
	}
	close(release)
	_ = release

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = store.CommitCreate(path, idA, "from-A", snapA.abs, snapA.dir)
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = store.CommitCreate(path, idB, "from-B", snapB.abs, snapB.dir)
	}()
	wg.Wait()

	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !domain.IsConflictError(err) {
			t.Fatalf("loser error must be a conflict, got %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("want exactly one winner, got %d (errs=%v)", wins, errs)
	}
	winner, ok := store.Current(path)
	if !ok {
		t.Fatal("no committed version after race")
	}
	if winner.ID != idA && winner.ID != idB {
		t.Fatalf("winner ID %v is neither candidate", winner.ID)
	}
	if winner.Rev != domain.InitialRevision {
		t.Fatalf("winner revision = %v, want initial", winner.Rev)
	}
	// Loser rebases to the winner's committed version.
	loserID := idA
	if winner.ID == idA {
		loserID = idB
	}
	_ = loserID
	cur, found, _, _ := store.Snapshot(path)
	if !found || cur.ID != winner.ID || cur.Content != winner.Content {
		t.Fatalf("rebase after conflict did not observe winner: %+v", cur)
	}
}

// TestStaleListingCannotValidateWrite proves a writer holding a stale
// directory-membership revision cannot commit after the directory changed.
func TestStaleListingCannotValidateWrite(t *testing.T) {
	store := NewSimStore()
	idA := mustNode(t, "nod_0123456789ABCDEFGHJKMNPQRS")
	idB := mustNode(t, "nod_0123456789ABCDEFGHJKMNPQRT")
	idC := mustNode(t, "nod_0123456789ABCDEFGHJKMNPQRV")

	_, _, dir0, _ := store.Snapshot("/shared/a.txt")
	if _, err := store.CommitCreate("/shared/a.txt", idA, "a", true, dir0); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	// Writer snapshots dir rev, then another commit advances it.
	_, _, staleDir, _ := store.Snapshot("/shared/b.txt")
	_, _, curDir, _ := store.Snapshot("/shared/c.txt")
	_ = curDir
	if _, err := store.CommitCreate("/shared/c.txt", idB, "c", true, staleDir); err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if _, err := store.CommitCreate("/shared/b.txt", idC, "b", true, staleDir); err == nil {
		t.Fatal("stale directory revision committed, want conflict")
	} else if !domain.IsConflictError(err) {
		t.Fatalf("stale commit error must be conflict, got %v", err)
	}
}
