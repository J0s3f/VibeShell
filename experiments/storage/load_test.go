package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Load shape for gate 9. The numbers are deliberately modest: the gate answers
// whether the qualified settings sustain a plausible turn rate on this storage, not
// where the limit is. One turn is one small file write plus its event and lookup
// rows, which is the shape of an accepted shell command in the world state.
const (
	loadTurns       = 240
	loadSubmitters  = 4
	loadQueueDepth  = 8
	loadPayloadSize = 4096
)

// loadSettings are the durability settings the writer rate is compared across.
// FULL is the qualified production setting; NORMAL is the cheaper one, measured
// only to show what FULL costs.
var loadSettings = []struct {
	name    string
	pragmas []Pragma
}{
	{"synchronous-FULL", nil},
	{"synchronous-NORMAL", []Pragma{{"synchronous", "NORMAL"}}},
}

// LoadResult is one measurement, in a shape both the text receipt and the JSON
// receipt can carry.
type LoadResult struct {
	Setting          string  `json:"setting"`
	Turns            int64   `json:"turns"`
	Submitters       int     `json:"submitters"`
	QueueCapacity    int     `json:"queueCapacity"`
	PayloadBytes     int     `json:"payloadBytes"`
	WallSeconds      float64 `json:"wallSeconds"`
	TurnsPerSecond   float64 `json:"turnsPerSecond"`
	Completed        int64   `json:"completed"`
	Failures         int64   `json:"failures"`
	Rejected         int64   `json:"rejected"`
	MaxQueueDepth    int64   `json:"maxQueueDepth"`
	MeanCommitMillis float64 `json:"meanCommitMillis"`
	MaxCommitMillis  float64 `json:"maxCommitMillis"`
	BodyP50Millis    float64 `json:"bodyP50Millis"`
	BodyP95Millis    float64 `json:"bodyP95Millis"`
	BodyP99Millis    float64 `json:"bodyP99Millis"`
	DatabaseBytes    int64   `json:"databaseBytes"`
	PageSize         int64   `json:"pageSize"`
	PageCount        int64   `json:"pageCount"`
}

// TestWriterLoadSustainsAPlausibleTurnRate qualifies gate 9. The number is a rough
// measurement on this machine, not a capacity figure: it says the qualified
// settings commit turns at a rate the design can assume and shows what the durable
// setting costs.
func TestWriterLoadSustainsAPlausibleTurnRate(t *testing.T) {
	label := RunLabel()
	receipt := newReport("Gate 9: writer load")
	receipt.add("captured (UTC)", time.Now().UTC().Format(time.RFC3339))
	receipt.add("run label", label)
	receipt.add("turn shape", fmt.Sprintf("one %d byte file, one event, its command and path index rows",
		loadPayloadSize))
	receipt.add("turns per run", loadTurns)
	receipt.add("concurrent submitters", loadSubmitters)
	receipt.add("queue capacity", loadQueueDepth)

	results := make([]LoadResult, 0, len(loadSettings))
	for _, setting := range loadSettings {
		result := measureWriterLoad(t, setting.name, setting.pragmas)
		results = append(results, result)

		receipt.section(setting.name)
		receipt.add("turns committed", fmt.Sprintf("%d in %.3fs", result.Completed, result.WallSeconds))
		receipt.add("turns per second", fmt.Sprintf("%.1f", result.TurnsPerSecond))
		receipt.add("commit latency", fmt.Sprintf("mean %.2fms, max %.2fms", result.MeanCommitMillis, result.MaxCommitMillis))
		receipt.add("turn body latency", fmt.Sprintf("p50 %.2fms, p95 %.2fms, p99 %.2fms",
			result.BodyP50Millis, result.BodyP95Millis, result.BodyP99Millis))
		receipt.add("queue", fmt.Sprintf("capacity %d, max depth %d, rejected %d, failures %d",
			result.QueueCapacity, result.MaxQueueDepth, result.Rejected, result.Failures))
		receipt.add("volume", fmt.Sprintf("%d bytes, %d pages of %d", result.DatabaseBytes, result.PageCount, result.PageSize))
	}

	if full, normal := results[0], results[1]; normal.TurnsPerSecond > 0 {
		receipt.add("FULL costs", fmt.Sprintf("%.1f%% of the NORMAL rate on this machine",
			100*(1-full.TurnsPerSecond/normal.TurnsPerSecond)))
	}
	receipt.add("limits", "rough figures on this container's volume; not a capacity limit, and the race-detector run's timings are inflated")
	receipt.write(t, "gate9-writer-load-"+label)
	writeMeasurements(t, label, results)
}

// measureWriterLoad submits loadTurns turns through the bounded writer queue and
// reports what the qualified settings sustained.
func measureWriterLoad(t *testing.T, settingName string, pragmas []Pragma) LoadResult {
	t.Helper()
	path := NewDatabasePath(t, "load-"+settingName)
	db, err := OpenWriter(path, Options{Pragmas: pragmas})
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(Schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	applySearchSchema(t, db)
	seedWorld(t, db, "ns-load", "root-load")
	insertDirectory(t, db, "dir", "root-load", "dir")

	// Each turn gets its own sequence, so the turn counter is shared rather than
	// per submitter.
	var issued sync.Mutex
	next := 0
	take := func() int {
		issued.Lock()
		defer issued.Unlock()
		next++
		return next
	}

	body := &latencyRecorder{}
	queue := NewWriterQueue(db, loadQueueDepth)

	started := time.Now()
	var done sync.WaitGroup
	for submitter := 0; submitter < loadSubmitters; submitter++ {
		done.Add(1)
		go func(id int) {
			defer done.Done()
			for turn := 0; turn < loadTurns/loadSubmitters; turn++ {
				sequence := take()
				// The expectation is read inside the writer's transaction, which is
				// where a real turn reads it, so a contended turn sees a conflict
				// rather than a lost update.
				commit := func(ctx context.Context, tx *sql.Tx) error {
					bodyStart := time.Now()
					defer func() { body.add(time.Since(bodyStart)) }()

					revision, err := ReadDirectoryRevision(ctx, tx, "dir")
					if err != nil {
						return err
					}
					payload := pseudoRandomBlob(loadPayloadSize, uint32(sequence))
					hash := ContentHash(payload)
					name := fmt.Sprintf("turn-%06d.bin", sequence)
					if err := CommitChangeSet(ctx, tx, ChangeSet{
						NamespaceID: "ns-load",
						Contents:    []Content{{Hash: hash, Bytes: payload}},
						Changes: []StagedChange{{
							NodeID: "node-" + name, ParentID: "dir", Name: name,
							Kind: "file", ContentHash: hash,
							ExpectAbsent: true, ExpectedParentRevision: revision,
						}},
						Events: []AcceptedEvent{{
							ID: "ev-" + name, SessionID: fmt.Sprintf("session-%d", id), Sequence: sequence + 1,
							Kind: "accepted", OccurredAt: fixedTimestamp(sequence),
							Command: "write " + name, Path: "/dir/" + name,
						}},
					}); err != nil {
						return err
					}
					return IndexEventForSearch(ctx, tx, "ev-"+name, "write "+name, "/dir/"+name)
				}
				if err := queue.Submit(context.Background(), commit); err != nil {
					t.Errorf("submit turn %d: %v", sequence, err)
					return
				}
			}
		}(submitter)
	}
	done.Wait()
	queue.Close()
	elapsed := time.Since(started)

	stats := queue.Stats()
	if err := queue.LastError(); err != nil {
		t.Errorf("a turn failed to commit: %v", err)
	}
	if count := queryInt(t, db, `SELECT COUNT(*) FROM nodes WHERE kind = 'file'`); count != stats.Completed {
		t.Errorf("%d files for %d completed turns: a turn was lost", count, stats.Completed)
	}
	if got := countRows(t, db, "contents"); got != stats.Completed {
		t.Errorf("%d content rows for %d completed turns: distinct payloads must not deduplicate", got, stats.Completed)
	}

	// Checkpoint so the reported size is the database, not a log that has not been
	// folded into it yet.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	pageSize := queryInt(t, db, `PRAGMA page_size`)
	pageCount := queryInt(t, db, `PRAGMA page_count`)

	meanMillis := 0.0
	if stats.Completed > 0 {
		meanMillis = float64(stats.CommitNanos) / float64(stats.Completed) / float64(time.Millisecond)
	}
	result := LoadResult{
		Setting:          settingName,
		Turns:            stats.Completed,
		Submitters:       loadSubmitters,
		QueueCapacity:    loadQueueDepth,
		PayloadBytes:     loadPayloadSize,
		WallSeconds:      elapsed.Seconds(),
		TurnsPerSecond:   float64(stats.Completed) / elapsed.Seconds(),
		Completed:        stats.Completed,
		Failures:         stats.Failures,
		Rejected:         stats.Rejected,
		MaxQueueDepth:    stats.MaxDepth,
		MeanCommitMillis: meanMillis,
		MaxCommitMillis:  float64(stats.MaxCommit.Microseconds()) / 1000,
		BodyP50Millis:    millis(body.quantile(0.50)),
		BodyP95Millis:    millis(body.quantile(0.95)),
		BodyP99Millis:    millis(body.quantile(0.99)),
		DatabaseBytes:    databaseFileSize(t, path),
		PageSize:         pageSize,
		PageCount:        pageCount,
	}
	if result.TurnsPerSecond <= 0 || result.Turns == 0 {
		t.Fatalf("no turns completed under %s", settingName)
	}
	if result.Failures != 0 || result.Rejected != 0 {
		t.Errorf("%s: %d failures and %d rejections", settingName, result.Failures, result.Rejected)
	}
	return result
}

// writeMeasurements records the numbers as JSON as well, so a later reader can
// compare runs without parsing text.
func writeMeasurements(t *testing.T, label string, results []LoadResult) {
	t.Helper()
	dir, err := ModuleDir()
	if err != nil {
		t.Fatalf("locate spike module: %v", err)
	}
	body, err := json.MarshalIndent(struct {
		RunLabel string       `json:"runLabel"`
		Results  []LoadResult `json:"results"`
	}{RunLabel: label, Results: results}, "", "  ")
	if err != nil {
		t.Fatalf("encode measurements: %v", err)
	}

	path := filepath.Join(dir, ReceiptsDir, "measurements-"+label+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("receipt %s", path)
}

func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
