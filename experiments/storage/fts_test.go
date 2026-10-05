package storage

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// searchDocuments is the fixture used by the search gate. It mixes exact names,
// Unicode names, punctuation that FTS5 must not split, and a near-duplicate that
// differs only in case.
var searchDocuments = []struct {
	eventID string
	command string
	path    string
}{
	{"ev-ls-home", "ls -la /home", "/home"},
	{"ev-ls-alpha", "ls -la /home/Alpha", "/home/Alpha"},
	{"ev-ls-alpha-lower", "ls -la /home/alpha", "/home/alpha"},
	{"ev-grep-ls", "grep -r ls /home/Alpha", "/home/Alpha/notes.txt"},
	{"ev-cat-unicode", "cat /home/zoë/文件.txt", "/home/zoë/文件.txt"},
	{"ev-percent", "cat /home/100%/report.txt", "/home/100%/report.txt"},
	{"ev-underscore", "cat /home/a_b/c.txt", "/home/a_b/c.txt"},
	{"ev-emoji", "cat /home/🌍/notes.txt", "/home/🌍/notes.txt"},
}

// rankingFixture is a separate corpus for the bm25 check. bm25 combines term
// frequency, document length, and inverse document frequency, so ranking cannot
// be judged on two documents: the term has to be rare in a corpus of some size.
var rankingFixture = []struct {
	eventID string
	command string
	path    string
}{
	{"ev-rank-many", "grep zzz zzz zzz zzz zzz", "/tmp/many"},
	{"ev-rank-few", "grep zzz", "/tmp/few"},
	{"ev-rank-fill-1", "cat /tmp/one", "/tmp/one"},
	{"ev-rank-fill-2", "cat /tmp/two", "/tmp/two"},
	{"ev-rank-fill-3", "cat /tmp/three", "/tmp/three"},
	{"ev-rank-fill-4", "cat /tmp/four", "/tmp/four"},
	{"ev-rank-fill-5", "cat /tmp/five", "/tmp/five"},
	{"ev-rank-fill-6", "cat /tmp/six", "/tmp/six"},
}

// TestFTS5IsAvailable qualifies gate 2. If FTS5 is missing this fails with the
// reason, and TestIndexOnlySearchFallback documents what the plan would use
// instead.
func TestFTS5IsAvailable(t *testing.T) {
	db, _ := newSpikeDB(t, "fts")
	receipt := newReport("Gate 2: FTS5 availability")

	var options []string
	rows, err := db.Query(`PRAGMA compile_options`)
	if err != nil {
		t.Fatalf("read compile options: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var option string
		if err := rows.Scan(&option); err != nil {
			t.Fatalf("scan compile option: %v", err)
		}
		options = append(options, option)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate compile options: %v", err)
	}
	enabled := slices.Contains(options, "ENABLE_FTS5")
	receipt.add("ENABLE_FTS5", enabled)
	if !enabled {
		t.Fatal("the pinned driver build does not report ENABLE_FTS5; the full-text projection is unavailable")
	}

	if _, err := db.Exec(SchemaSearch); err != nil {
		t.Fatalf("create the FTS5 projection: %v", err)
	}
	receipt.add("virtual table", "transcript_fts created")
	receipt.add("content model", "external content over search_docs")
	receipt.add("tokenizer", "unicode61 remove_diacritics 2")
	receipt.add("triggers", "search_docs_ai, search_docs_ad, search_docs_au")

	receipt.write(t, "gate2-fts5")
}

// TestFTS5Retrieval covers the queries the research store needs: term, column,
// phrase, and prefix matching, plus ranking and snippets. The cases also pin down
// what the tokenizer cannot do, because a documented limitation is cheaper than
// a surprise in a search feature.
func TestFTS5Retrieval(t *testing.T) {
	db, _ := newSpikeDB(t, "fts-query")
	if _, err := db.Exec(SchemaSearch); err != nil {
		t.Fatalf("create the FTS5 projection: %v", err)
	}
	seedSearchDocuments(t, db)

	receipt := newReport("Gate 2b: FTS5 retrieval, ranking, and Unicode")

	cases := []struct {
		name  string
		match string
		want  []string
	}{
		{"bare term", "ls", []string{"ev-grep-ls", "ev-ls-alpha", "ev-ls-alpha-lower", "ev-ls-home"}},
		{"column filter", "command:grep", []string{"ev-grep-ls"}},
		{"column and term", "path:report", []string{"ev-percent"}},
		{"path conjunction", "path:notes", []string{"ev-emoji", "ev-grep-ls"}},
		{"phrase", `"ls -la"`, []string{"ev-ls-alpha", "ev-ls-alpha-lower", "ev-ls-home"}},
		{"prefix token", "ca*", []string{"ev-cat-unicode", "ev-emoji", "ev-percent", "ev-underscore"}},
		{"diacritics folded", "zoe", []string{"ev-cat-unicode"}},
		{"CJK token", "文件", []string{"ev-cat-unicode"}},
		{"punctuation term", "100", []string{"ev-percent"}},
		// unicode61 keeps letters and digits, so an emoji is a separator and
		// never a searchable token. A path containing one is still reachable
		// through the subtree key range; it is not reachable through FTS5.
		{"emoji is not a token", "🌍", []string{}},
	}
	for _, tc := range cases {
		got := ftsHits(t, db, tc.match)
		if !slices.Equal(got, tc.want) {
			t.Errorf("MATCH %q returned %v, want %v", tc.match, got, tc.want)
		}
		receipt.addf("%-20s %-16q %v", tc.name, tc.match, got)
	}

	// bm25 must put the document with the higher term frequency for a rare term
	// first. The fixture lives in its own session so that the corpus size and
	// inverse document frequency are meaningful.
	seedRankingFixture(t, db)
	ranked := ftsRanked(t, db, `grep AND zzz`)
	receipt.add("ranked (grep AND zzz)", ranked)
	if len(ranked) != 2 {
		t.Fatalf("expected two ranked hits, got %v", ranked)
	}
	if ranked[0].EventID != "ev-rank-many" {
		t.Errorf("best bm25 hit is %s, want ev-rank-many", ranked)
	}
	if !(ranked[0].Score < ranked[1].Score) {
		t.Errorf("bm25 scores are not ordered: %v", ranked)
	}

	// snippet reads the content table, which only works because the projection is
	// external-content rather than contentless.
	snippet, err := ftsSnippet(t, db, `path:report`)
	if err != nil {
		t.Fatalf("snippet: %v", err)
	}
	if !strings.Contains(snippet, "<") {
		t.Errorf("snippet %q does not contain the requested markers", snippet)
	}
	receipt.add("snippet", snippet)

	receipt.section("query plans")
	receipt.addAll("fts match", explain(t, db, `SELECT rowid FROM transcript_fts WHERE transcript_fts MATCH ?`, `command:ls`))
	receipt.addAll("content lookup", explain(t, db, `SELECT event_id FROM search_docs WHERE event_id = ?`, "ev-ls-alpha"))

	receipt.write(t, "gate2-fts5-queries")
}

// TestFTS5ProjectionTracksItsContent proves the derived index can be repaired
// from the authoritative rows, which is what makes it safe to treat as derived
// rather than as a second source of truth.
func TestFTS5ProjectionTracksItsContent(t *testing.T) {
	db, _ := newSpikeDB(t, "fts-sync")
	if _, err := db.Exec(SchemaSearch); err != nil {
		t.Fatalf("create the FTS5 projection: %v", err)
	}
	seedSearchDocuments(t, db)

	receipt := newReport("Gate 2c: FTS5 projection maintenance")
	seeded := countRows(t, db, `search_docs`)
	receipt.add("documents seeded", seeded)

	// An update through the content table must reach the index.
	if _, err := db.Exec(`UPDATE search_docs SET path = '/home/renamed.txt' WHERE event_id = 'ev-percent'`); err != nil {
		t.Fatalf("update search doc: %v", err)
	}
	if hits := ftsHits(t, db, `path:renamed`); !slices.Equal(hits, []string{"ev-percent"}) {
		t.Errorf("after update, MATCH path:renamed returned %v", hits)
	}
	if hits := ftsHits(t, db, `path:report`); len(hits) != 0 {
		t.Errorf("the replaced token is still indexed after an update: %v", hits)
	}
	receipt.add("update propagated", true)

	// A delete through the content table must reach the index.
	if _, err := db.Exec(`DELETE FROM search_docs WHERE event_id = 'ev-underscore'`); err != nil {
		t.Fatalf("delete search doc: %v", err)
	}
	if hits := ftsHits(t, db, `path:underscore`); len(hits) != 0 {
		t.Errorf("a deleted document is still searchable: %v", hits)
	}
	if hits := ftsHits(t, db, `command:cat`); !slices.Equal(hits, []string{"ev-cat-unicode", "ev-emoji", "ev-percent"}) {
		t.Errorf("after delete, command:cat returned %v", hits)
	}
	receipt.add("delete propagated", true)

	// Damage the index behind the content table's back, then rebuild it. This is
	// the repair path an operator would use after an unclean shutdown.
	if _, err := db.Exec(`INSERT INTO transcript_fts (transcript_fts) VALUES ('delete-all')`); err != nil {
		t.Fatalf("empty the projection: %v", err)
	}
	if hits := ftsHits(t, db, `path:home`); len(hits) != 0 {
		t.Fatalf("expected an empty index after delete-all, got %v", hits)
	}
	receipt.add("index emptied on purpose", true)

	if _, err := db.Exec(`INSERT INTO transcript_fts (transcript_fts) VALUES ('rebuild')`); err != nil {
		t.Fatalf("rebuild the projection: %v", err)
	}
	rebuilt := len(ftsHits(t, db, `path:home`))
	if rebuilt == 0 {
		t.Fatal("rebuild left the projection empty")
	}
	receipt.add("hits after rebuild", rebuilt)
	receipt.add("documents expected", seeded-1)
	if want := seeded - 1; countRows(t, db, `search_docs`) != want {
		t.Errorf("search_docs holds %d rows, want %d", countRows(t, db, `search_docs`), want)
	}
	receipt.addAll("integrity", integrityReport(t, db))
	receipt.addAll("contentless alternative", contentlessDeleteProbe(t, db))

	receipt.write(t, "gate2-fts5-sync")
}

// TestIndexOnlySearchFallback evaluates the alternative the plan would use if
// FTS5 were unavailable: exact and normalized command indexes plus the subtree
// key range, with no full-text ranking.
func TestIndexOnlySearchFallback(t *testing.T) {
	db, _ := newSpikeDB(t, "index-fallback")
	seedSearchDocuments(t, db)

	receipt := newReport("Gate 2d: index-only search fallback (no FTS5)")

	exact := queryStrings(t, db, `SELECT DISTINCT event_id FROM command_index WHERE command = ? ORDER BY event_id`, "grep -r ls /home/Alpha")
	receipt.add("exact command", exact)
	receipt.addAll("  plan", explain(t, db, `SELECT event_id FROM command_index WHERE command = ?`, "grep -r ls /home/Alpha"))

	normalized := queryStrings(t, db,
		`SELECT DISTINCT event_id FROM command_index WHERE normalized_command = ? AND cwd = ? ORDER BY event_id`,
		"grep   -r   ls /home/Alpha", "/")
	receipt.add("normalized command", normalized)
	receipt.addAll("  plan", explain(t, db,
		`SELECT event_id FROM command_index WHERE normalized_command = ? AND cwd = ?`, "grep -r ls /home/Alpha", "/"))

	low, high := SubtreeRange("/home/Alpha")
	subtree := queryStrings(t, db, `SELECT DISTINCT path FROM path_index WHERE path_key >= ? AND path_key < ? ORDER BY path`, low, high)
	receipt.add("subtree of /home/Alpha", subtree)
	receipt.addAll("  plan", explain(t, db, `SELECT path FROM path_index WHERE path_key >= ? AND path_key < ?`, low, high))

	// What the fallback cannot do: find a term in the middle of a command, which
	// needs a scan rather than an index.
	receipt.section("limits of the fallback")
	likePlan := explain(t, db, `SELECT event_id FROM command_index WHERE command LIKE ?`, "%ls%")
	receipt.addAll("substring scan plan", likePlan)
	scanned := false
	for _, line := range likePlan {
		if strings.HasPrefix(line, "SCAN") {
			scanned = true
		}
	}
	if !scanned {
		t.Errorf("expected a full scan for a leading-wildcard LIKE, plan was %v", likePlan)
	}
	receipt.add("conclusion", "exact, normalized, and subtree path queries stay index-backed; mid-token search degrades to a scan and there is no ranking, stemming, or phrase support")

	receipt.write(t, "gate2-index-fallback")
}

// contentlessDeleteProbe records whether a contentless FTS5 table can be used
// with the trigger-free alternative the plan could otherwise choose.
func contentlessDeleteProbe(t *testing.T, db *sql.DB) []string {
	t.Helper()
	if _, err := db.Exec(`CREATE VIRTUAL TABLE contentless_fts USING fts5(command, content='')`); err != nil {
		return []string{"contentless fts5: unavailable: " + err.Error()}
	}
	if _, err := db.Exec(`INSERT INTO contentless_fts (rowid, command) VALUES (1, 'ls -la')`); err != nil {
		return []string{"contentless insert: " + err.Error()}
	}
	_, plainDelete := db.Exec(`DELETE FROM contentless_fts WHERE rowid = 1`)
	_, markedDelete := db.Exec(
		`INSERT INTO contentless_fts (contentless_fts, rowid, command) VALUES ('delete', 1, 'ls -la')`,
	)
	return []string{
		"plain DELETE: " + describeErr(plainDelete),
		"'delete' command: " + describeErr(markedDelete),
		"conclusion: a contentless table cannot be maintained by a simple DELETE trigger, so the external-content form is the one to build",
	}
}

func describeErr(err error) string {
	if err == nil {
		return "accepted"
	}
	return "rejected: " + strings.SplitN(err.Error(), "\n", 2)[0]
}

func seedSearchDocuments(t *testing.T, db *sql.DB) {
	t.Helper()
	seedDocuments(t, db, "session-search", searchDocuments)
}

func seedRankingFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	seedDocuments(t, db, "session-ranking", rankingFixture)
}

func seedDocuments(t *testing.T, db *sql.DB, sessionID string, docs []struct {
	eventID string
	command string
	path    string
}) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin seed documents: %v", err)
	}
	defer tx.Rollback()

	for i, doc := range docs {
		event := AcceptedEvent{
			ID:         doc.eventID,
			SessionID:  sessionID,
			Sequence:   i + 1,
			Kind:       "accepted",
			OccurredAt: fixedTimestamp(i + 1),
			Payload:    []byte("payload"),
			Command:    doc.command,
			Path:       doc.path,
		}
		if err := appendAcceptedEvent(context.Background(), tx, event); err != nil {
			t.Fatalf("append event %s: %v", event.ID, err)
		}
		if err := IndexEventForSearch(context.Background(), tx, event.ID, event.Command, event.Path); err != nil {
			t.Fatalf("index %s for search: %v", event.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed documents: %v", err)
	}
}

// ftsHits returns the event identifiers matching an FTS5 query, in ascending
// identifier order.
func ftsHits(t *testing.T, db *sql.DB, match string) []string {
	t.Helper()
	rows, err := db.Query(`
		SELECT d.event_id
		FROM transcript_fts f
		JOIN search_docs d ON d.rowid = f.rowid
		WHERE f.transcript_fts MATCH ?
		ORDER BY d.event_id`, match)
	if err != nil {
		t.Fatalf("FTS5 MATCH %q: %v", match, err)
	}
	defer rows.Close()

	var hits []string
	for rows.Next() {
		var eventID string
		if err := rows.Scan(&eventID); err != nil {
			t.Fatalf("scan FTS5 hit: %v", err)
		}
		hits = append(hits, eventID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate FTS5 hits: %v", err)
	}
	if hits == nil {
		return []string{}
	}
	return hits
}

type rankHit struct {
	EventID string
	Score   float64
}

func (r rankHit) String() string {
	return strings.TrimSpace(r.EventID)
}

// ftsRanked returns FTS5 hits in bm25 order, where a smaller score is a better
// match.
func ftsRanked(t *testing.T, db *sql.DB, match string) []rankHit {
	t.Helper()
	rows, err := db.Query(`
		SELECT d.event_id, bm25(transcript_fts)
		FROM transcript_fts f
		JOIN search_docs d ON d.rowid = f.rowid
		WHERE f.transcript_fts MATCH ?
		ORDER BY rank`, match)
	if err != nil {
		t.Fatalf("ranked FTS5 MATCH %q: %v", match, err)
	}
	defer rows.Close()

	var hits []rankHit
	for rows.Next() {
		var hit rankHit
		if err := rows.Scan(&hit.EventID, &hit.Score); err != nil {
			t.Fatalf("scan ranked FTS5 hit: %v", err)
		}
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ranked FTS5 hits: %v", err)
	}
	return hits
}

// ftsSnippet returns a marked excerpt of the indexed path column.
func ftsSnippet(t *testing.T, db *sql.DB, match string) (string, error) {
	t.Helper()
	var snippet string
	err := db.QueryRow(`
		SELECT snippet(transcript_fts, 1, '<', '>', '...', 6)
		FROM transcript_fts
		WHERE transcript_fts MATCH ?`, match).Scan(&snippet)
	return snippet, err
}

// queryStrings runs a query returning one text column.
func queryStrings(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()

	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan result: %v", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate results: %v", err)
	}
	return values
}
