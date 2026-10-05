package load

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"j0s.at/vibeshell/internal/adapters/sqlite"
	"j0s.at/vibeshell/internal/domain"
)

// worldScenarioConfig sizes the world contention scenario.
type worldScenarioConfig struct {
	// Writers is how many sessions attempt first-time materialisation of the
	// same private path at once.
	Writers int
	// SharedWriters is how many sessions attempt first-time materialisation of
	// the same shared path at once.
	SharedWriters int
	// Readers is how many sessions read the shared path concurrently with the
	// writers.
	Readers int
	// ReadRounds is how many reads each reader performs.
	ReadRounds int
	// ConflictingWriters is how many sessions save the same node from the same
	// stale revision.
	ConflictingWriters int
	// Operations is how many independent private writes each writer performs
	// alongside the contended ones, so throughput is measured too.
	Operations int
}

// runWorld exercises concurrent first-time path materialisation in private and
// shared scope, shared reads during those writes, and genuinely conflicting
// saves. It uses the real SQLite world adapter, so optimistic concurrency and
// scope enforcement are the shipped code paths.
func runWorld(ctx context.Context, sampler *Sampler, cfg worldScenarioConfig) ScenarioResult {
	result := ScenarioResult{
		Name: "world-contention",
		Description: fmt.Sprintf(
			"%d writers materialise one private path and %d writers one shared path concurrently while %d readers "+
				"read the shared path, followed by %d conflicting saves from the same stale revision; "+
				"the real SQLite world adapter resolves the outcome.",
			cfg.Writers, cfg.SharedWriters, cfg.Readers, cfg.ConflictingWriters),
		Config: map[string]any{
			"private_writers":     cfg.Writers,
			"shared_writers":      cfg.SharedWriters,
			"readers":             cfg.Readers,
			"read_rounds":         cfg.ReadRounds,
			"conflicting_writers": cfg.ConflictingWriters,
			"private_operations":  cfg.Operations,
		},
		Errors: map[string]int{},
		Passed: true,
	}
	started := time.Now()
	result.Samples = append(result.Samples, sampler.Sample("world:start"))

	dir, err := harnessTempDir("world")
	if err != nil {
		result.Passed = false
		result.Notes = append(result.Notes, err.Error())
		return result
	}
	db, err := sqlite.Open(dir+"/world.db", sqlite.Options{})
	if err != nil {
		result.Passed = false
		result.Notes = append(result.Notes, err.Error())
		return result
	}
	defer func() { _ = db.Close() }()

	policy := domain.DefaultScopePolicy()
	var minter idMinter
	// Namespaces are keyed by the harness username so every goroutine looks one
	// up the same way; the durable identity is inside the namespace record.
	const contendedUsername = "worlduser00"
	namespaces := map[string]domain.Namespace{}
	for i := 0; i < cfg.Writers+cfg.Readers+cfg.ConflictingWriters; i++ {
		username := fmt.Sprintf("worlduser%02d", i)
		user, idErr := publicIdentity(username)
		if idErr != nil {
			result.Passed = false
			result.Notes = append(result.Notes, idErr.Error())
			return result
		}
		ns, nsErr := db.EnsureNamespace(ctx, domain.NamespaceForUser(user, fmt.Sprintf("user:%s", user.Value())))
		if nsErr != nil {
			result.Passed = false
			result.Notes = append(result.Notes, nsErr.Error())
			return result
		}
		namespaces[username] = ns
	}
	shared, err := db.EnsureNamespace(ctx, domain.NamespaceShared())
	if err != nil {
		result.Passed = false
		result.Notes = append(result.Notes, err.Error())
		return result
	}
	result.Samples = append(result.Samples, sampler.Sample("world:namespaces_ready"))

	// One private namespace is contended by every writer: two first
	// materialisations of the same path must produce one authoritative object.
	contended := namespaces[contendedUsername]
	privatePath := domain.MustParsePath("/projects/loadharness/notes.txt")
	sharedPath := domain.MustParsePath("/srv/loadharness/status.txt")

	var (
		mu               sync.Mutex
		privateRaceWins  int
		privateRaceLosts int
		sharedMade       int
		conflicts        int
		wins             int
		reads            int
		readErrors       int
		writeErrors      int
		errSamples       []string
		writeLat         Latencies
		readLat          Latencies
	)
	// noteFailure records a failure and keeps a few samples, so a receipt names
	// the actual reason instead of only a count.
	noteFailure := func(err error) {
		writeErrors++
		if len(errSamples) < 5 {
			errSamples = append(errSamples, describeError(err))
		}
	}

	// materialize stages and commits a create for one path in one namespace.
	// Only the leaf is staged: the world adapter materialises the parent chain
	// itself, so staging a directory here would turn every concurrent writer
	// into a duplicate-key race on an ancestor instead of on the target path.
	materialize := func(ns domain.Namespace, path domain.ValidPath, body string) error {
		ref, putErr := db.Put(ctx, []byte(body), "text/plain")
		if putErr != nil {
			return putErr
		}
		cs := domain.EmptyChangeSet(minter.Turn(), minter.Attempt(), time.Now().UnixMilli())
		cs.AddCreate(ns.ID, path, domain.NodeKindFile, domain.NewNodeMetadata(0o644, 0, 0, time.Now().UnixMilli()), ref)
		_, commitErr := db.Commit(ctx, cs, policy)
		return commitErr
	}

	// ensureDirChain creates every directory of dir that does not exist yet,
	// outermost first, one commit per directory.
	ensureDirChain := func(ctx context.Context, db *sqlite.DB, policy domain.ScopePolicy, ns domain.Namespace, dir domain.ValidPath) error {
		var chain []domain.ValidPath
		for parent := dir; !parent.IsRoot(); parent = parent.Parent() {
			if _, err := db.LookupPath(ctx, ns.ID, parent); err == nil {
				break
			}
			chain = append(chain, parent)
		}
		for i := len(chain) - 1; i >= 0; i-- {
			cs := domain.EmptyChangeSet(minter.Turn(), minter.Attempt(), time.Now().UnixMilli())
			cs.AddCreate(ns.ID, chain[i], domain.NodeKindDir,
				domain.NewNodeMetadata(0o755, 0, 0, time.Now().UnixMilli()), domain.EmptyContentRef())
			if _, err := db.Commit(ctx, cs, policy); err != nil {
				return fmt.Errorf("create %s in %s: %w", chain[i], ns.Label, err)
			}
		}
		return nil
	}
	// The parent chain must exist before the contention starts, so it is created
	// here rather than inside materialize.
	if err := ensureDirChain(ctx, db, policy, contended, privatePath.Parent()); err != nil {
		result.Passed = false
		result.Notes = append(result.Notes, "could not pre-create the parent chain: "+err.Error())
		return result
	}
	if err := ensureDirChain(ctx, db, policy, shared, sharedPath.Parent()); err != nil {
		result.Passed = false
		result.Notes = append(result.Notes, "could not pre-create the shared parent chain: "+err.Error())
		return result
	}

	// Phase 1: concurrent first-time materialisation of one private path.
	var wg sync.WaitGroup
	for i := 0; i < cfg.Writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			at := time.Now()
			// Every writer materialises the same path in the same private
			// namespace, so exactly one may win: two first materialisations of
			// one path must produce one authoritative object.
			err := materialize(contended, privatePath, fmt.Sprintf("created by writer %d\n", index))
			mu.Lock()
			writeLat.Record(time.Since(at))
			switch {
			case err == nil:
				privateRaceWins++
			case isConflict(err):
				privateRaceLosts++
			default:
				noteFailure(err)
			}
			mu.Unlock()
		}(i)
	}

	// Phase 2: concurrent first-time materialisation of one shared path while
	// readers read it.
	readCtx, stopReads := context.WithCancel(ctx)
	var readWG sync.WaitGroup
	for r := 0; r < cfg.Readers; r++ {
		readWG.Add(1)
		go func(index int) {
			defer readWG.Done()
			username := fmt.Sprintf("worlduser%02d", index)
			ns := namespaces[username]
			for round := 0; round < cfg.ReadRounds; round++ {
				if readCtx.Err() != nil {
					return
				}
				at := time.Now()
				node, lookupErr := db.LookupPath(readCtx, ns.ID, sharedPath)
				mu.Lock()
				readLat.Record(time.Since(at))
				if lookupErr != nil {
					readErrors++
				} else if node.IsFile() {
					reads++
				}
				mu.Unlock()
				// Independent private writes run alongside the shared reads so
				// the measurement covers ordinary throughput too. The path is
				// root-level because a fresh namespace has no directories yet.
				own := domain.MustParsePath(fmt.Sprintf("/throughput-%02d-%02d.txt", index, round))
				if err := materialize(ns, own, strings.Repeat("x", 512)); err != nil {
					mu.Lock()
					if !isConflict(err) {
						noteFailure(err)
					}
					mu.Unlock()
				}
			}
		}(r)
	}
	for i := 0; i < cfg.SharedWriters; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			at := time.Now()
			err := materialize(shared, sharedPath, fmt.Sprintf("shared status %d\n", index))
			mu.Lock()
			writeLat.Record(time.Since(at))
			switch {
			case err == nil:
				sharedMade++
			case isConflict(err):
				sharedMade++
			default:
				noteFailure(err)
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	stopReads()
	readWG.Wait()
	result.Samples = append(result.Samples, sampler.Sample("world:first_time_and_reads"))

	// Phase 3: genuinely conflicting saves. Every writer reads the node at the
	// same revision and then saves, so exactly one may win.
	target, err := db.LookupPath(ctx, shared.ID, sharedPath)
	if err != nil {
		result.Passed = false
		result.Notes = append(result.Notes,
			fmt.Sprintf("shared path was not materialised (%v); writer failures: %s",
				err, strings.Join(errSamples, " | ")))
		return result
	}
	baseRevision := target.Revision
	for i := 0; i < cfg.ConflictingWriters; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ref, putErr := db.Put(ctx, []byte(fmt.Sprintf("conflicting save %d\n", index)), "text/plain")
			if putErr != nil {
				mu.Lock()
				noteFailure(putErr)
				mu.Unlock()
				return
			}
			cs := domain.EmptyChangeSet(minter.Turn(), minter.Attempt(), time.Now().UnixMilli())
			cs.AddUpdate(shared.ID, sharedPath, target.ID, baseRevision,
				domain.NewNodeMetadata(0o644, 0, 0, time.Now().UnixMilli()), ref)
			at := time.Now()
			_, commitErr := db.Commit(ctx, cs, policy)
			mu.Lock()
			writeLat.Record(time.Since(at))
			switch {
			case commitErr == nil:
				wins++
			case isConflict(commitErr):
				conflicts++
			default:
				noteFailure(err)
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	// The authoritative object must be exactly the winner's content, proving the
	// losers did not overwrite it.
	after, err := db.LookupPath(ctx, shared.ID, sharedPath)
	if err != nil {
		result.Passed = false
		result.Notes = append(result.Notes, "shared path vanished after conflicting saves: "+err.Error())
		return result
	}
	if after.Revision <= baseRevision {
		result.Passed = false
		result.Notes = append(result.Notes, "conflicting saves did not advance the node revision")
	}

	result.DurationMs = time.Since(started).Milliseconds()
	result.Samples = append(result.Samples, sampler.Sample("world:end"))
	writeSummary := writeLat.Summary()
	readSummary := readLat.Summary()
	readP95 := readSummary.P95Ms
	result.Latency = &writeSummary
	result.LatencyLabel = "world read (shared path during writes)"
	result.LocalP95Ms = &readP95

	result.Metrics = map[string]any{
		"private_race_writers":       cfg.Writers,
		"private_race_winners":       privateRaceWins,
		"private_race_conflicts":     privateRaceLosts,
		"shared_writers":             cfg.SharedWriters,
		"shared_first_time_accepted": sharedMade,
		"conflicting_saves_accepted": wins,
		"conflicts_detected":         conflicts,
		"write_errors":               writeErrors,
		"shared_reads_accepted":      reads,
		"shared_read_errors":         readErrors,
		"write_p95_ms":               writeSummary.P95Ms,
		"write_max_ms":               writeSummary.MaxMs,
		"read_p95_ms":                readSummary.P95Ms,
		"read_max_ms":                readSummary.MaxMs,
		"node_revision_before":       baseRevision,
		"node_revision_after":        after.Revision,
		"database_bytes":             databaseBytes(dir + "/world.db"),
	}

	if readErrors > 0 {
		// Readers start before the shared path exists, so a not-found result is
		// the expected answer for those attempts rather than a failure.
		result.Notes = append(result.Notes,
			fmt.Sprintf("%d of %d shared reads returned not-found because they ran before the first writer "+
				"materialised the path; they are expected and are not counted as read failures of the adapter",
				readErrors, readErrors+reads))
	}
	// Optimistic concurrency must be strict: with every conflicting writer
	// starting from one revision, all but one must be refused.
	if cfg.ConflictingWriters > 1 && conflicts != cfg.ConflictingWriters-1 {
		result.Passed = false
		result.Notes = append(result.Notes,
			fmt.Sprintf("%d writers saved from revision %d but only %d conflicts were detected; "+
				"expected exactly %d", cfg.ConflictingWriters, baseRevision, conflicts, cfg.ConflictingWriters-1))
	}
	// The same rule applies to first-time materialisation of one path: exactly
	// one writer may create it.
	if cfg.Writers > 1 {
		if privateRaceWins != 1 || privateRaceLosts != cfg.Writers-1 {
			result.Passed = false
			result.Notes = append(result.Notes,
				fmt.Sprintf("%d writers raced to create one private path: %d succeeded and %d were refused; "+
					"expected exactly 1 success and %d refusals",
					cfg.Writers, privateRaceWins, privateRaceLosts, cfg.Writers-1))
		}
	}
	if writeErrors > 0 {
		result.Passed = false
		result.Notes = append(result.Notes,
			fmt.Sprintf("%d world operations failed for reasons other than a conflict: %s",
				writeErrors, strings.Join(errSamples, " | ")))
	}
	return result
}

// describeError renders a failure with its unwrapped cause and the domain
// error's own message and details. The world adapter wraps a mutation failure
// with its index and path, which on its own hides the reason.
func describeError(err error) string {
	parts := []string{}
	for current := err; current != nil; current = errors.Unwrap(current) {
		var domainErr *domain.DomainError
		if errors.As(current, &domainErr) {
			parts = append(parts, fmt.Sprintf("%s: %s %v", domainErr.Code, domainErr.Message, domainErr.Details))
			continue
		}
		parts = append(parts, current.Error())
	}
	return strings.Join(parts, " <- ")
}

// isConflict reports whether err is the optimistic-concurrency conflict the
// world adapter raises for a stale expected revision or a duplicate creation.
// Both mean "someone else got there first", which is the correct outcome for
// every writer but one in a contended race.
func isConflict(err error) bool {
	if err == nil {
		return false
	}
	var domainErr *domain.DomainError
	if errors.As(err, &domainErr) {
		switch domainErr.Code {
		case domain.CodeRevisionMismatch, domain.CodeConcurrentModification, domain.CodeDuplicateKey:
			return true
		}
	}
	return errors.Is(err, domain.FailureWorldConflict)
}
