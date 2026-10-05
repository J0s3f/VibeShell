package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// pathFixture is one recorded event: the command that ran and the path it touched.
// Paths are absolute and normalized, so no fixture contains a "." or ".." part and
// none ends in a separator.
var pathFixtures = []struct{ command, path string }{
	{"ls /", "/"},
	{"cat /some/file.txt", "/some/file.txt"},
	{"cat /some/place/file.txt", "/some/place/file.txt"},
	{"cat /some/place/deep/file.txt", "/some/place/deep/file.txt"},
	{"mkdir /some/place", "/some/place"},
	{"cat /somewhere/file.txt", "/somewhere/file.txt"},
	{"cat /some-else/file.txt", "/some-else/file.txt"},
	{"cat /some_thing/file.txt", "/some_thing/file.txt"},
	{"cat /sometext", "/sometext"},
	{"cat /x.txt", "/x.txt"},
	// Two names contain LIKE wildcards, so a prefix query has to escape or ignore
	// them to stay literal.
	{"cat /100%/report.txt", "/100%/report.txt"},
	{"cat /100%file", "/100%file"},
	{"cat /a_b/file.txt", "/a_b/file.txt"},
	{"cat /plain%dir", "/plain%dir"},
	{"cat /under_score", "/under_score"},
}

// subtreeCases state, by hand, which paths a prefix lookup must return. The
// expectations are written out rather than computed with the same helper the query
// uses, so a mistake in the range cannot make the test agree with itself.
var subtreeCases = []struct {
	prefix   string
	expected []string
}{
	{"/some", []string{"/some/file.txt", "/some/place", "/some/place/file.txt", "/some/place/deep/file.txt"}},
	{"/some/place", []string{"/some/place", "/some/place/file.txt", "/some/place/deep/file.txt"}},
	{"/some/", []string{"/some/file.txt", "/some/place", "/some/place/file.txt", "/some/place/deep/file.txt"}},
	{"/somewhere", []string{"/somewhere/file.txt"}},
	{"/some_thing", []string{"/some_thing/file.txt"}},
	{"/100%", []string{"/100%/report.txt"}},
	{"/100%file", []string{"/100%file"}},
	{"/a_b", []string{"/a_b/file.txt"}},
	{"/x.txt", []string{"/x.txt"}},
	{"/", []string{
		"/", "/100%/report.txt", "/100%file", "/a_b/file.txt", "/plain%dir", "/some-else/file.txt",
		"/some/file.txt", "/some/place", "/some/place/deep/file.txt", "/some/place/file.txt",
		"/some_thing/file.txt", "/somewhere/file.txt", "/sometext", "/under_score", "/x.txt",
	}},
}

// TestSubtreeLookupRespectsPathBoundaries qualifies the lookup half of gate 7. A
// subtree query that used a raw string prefix would return /somewhere for /some;
// the indexed key range must return exactly the prefix and its descendants.
func TestSubtreeLookupRespectsPathBoundaries(t *testing.T) {
	db, _ := newSpikeDB(t, "subtree")
	seedPathFixtures(t, db)

	receipt := newReport("Gate 7a: subtree lookup is boundary aware")
	receipt.add("fixtures recorded", len(pathFixtures))
	receipt.add("lookup", "path_key >= PathKey(prefix) AND path_key < SubtreeRange(prefix) upper bound")

	for _, tc := range subtreeCases {
		low, high := SubtreeRange(tc.prefix)
		got := queryStrings(t, db,
			`SELECT path FROM path_index WHERE path_key >= ? AND path_key < ? ORDER BY path`, low, high)
		want := sortedCopy(tc.expected)

		if !equalStrings(sortedCopy(got), want) {
			t.Errorf("%s: got %v, want %v", tc.prefix, got, want)
		}
		receipt.addf("%-14s range [%q, %q)", tc.prefix, low, high)
		receipt.addf("%-14s %d paths %v", tc.prefix, len(got), got)
	}

	// The siblings that only share characters with the prefix must never appear.
	for _, prefix := range []string{"/some", "/a_b", "/100%"} {
		low, high := SubtreeRange(prefix)
		got := queryStrings(t, db,
			`SELECT path FROM path_index WHERE path_key >= ? AND path_key < ?`, low, high)
		for _, path := range got {
			if strings.HasPrefix(path, prefix) && !strings.HasPrefix(path, prefix+"/") && path != prefix {
				t.Errorf("%s: subtree of %s returned the sibling %s", prefix, prefix, path)
			}
		}
	}
	receipt.add("sibling paths excluded", "/somewhere, /some-else, /sometext, /some_thing, /100%file, /under_score")
	receipt.write(t, "gate7-subtree-boundary")
}

// TestSubtreeLookupAgreesWithTheEscapedLikeVariant compares the key range with the
// LIKE form an implementation could reasonably have chosen. Agreeing on every
// fixture is the evidence that the key range is not a subtly different query.
func TestSubtreeLookupAgreesWithTheEscapedLikeVariant(t *testing.T) {
	db, _ := newSpikeDB(t, "like-equivalence")
	seedPathFixtures(t, db)

	receipt := newReport("Gate 7b: the key range agrees with escaped LIKE")
	agreed, differed := 0, 0
	for _, tc := range subtreeCases {
		low, high := SubtreeRange(tc.prefix)
		ranged := sortedCopy(queryStrings(t, db,
			`SELECT path FROM path_index WHERE path_key >= ? AND path_key < ?`, low, high))
		liked := sortedCopy(queryStrings(t, db,
			`SELECT path FROM path_index WHERE path_key LIKE ? ESCAPE '\'`, EscapeLikePrefix(PathKey(tc.prefix))+"%"))

		if equalStrings(ranged, liked) {
			agreed++
		} else {
			differed++
			t.Errorf("%s: key range %v but LIKE %v", tc.prefix, ranged, liked)
		}
	}
	receipt.add("prefixes compared", len(subtreeCases))
	receipt.add("identical result sets", agreed)
	receipt.add("differing result sets", differed)
	receipt.add("LIKE form", "path_key LIKE EscapeLikePrefix(PathKey(prefix)) || '%' ESCAPE '\\'")
	receipt.add("wildcard prefix escaped", "/100% -> "+EscapeLikePrefix(PathKey("/100%")))
	receipt.add("underscore prefix escaped", "/a_b -> "+EscapeLikePrefix(PathKey("/a_b")))
	receipt.write(t, "gate7-like-equivalence")
}

// TestLookupIsCaseSensitive qualifies the case behaviour that the driver consumes
// itself rather than reporting: PRAGMA case_sensitive_like cannot be read back from
// modernc.org/sqlite, so the claim has to be made behaviourally. A Unix path is
// case-sensitive, so /SOME must not match /some.
func TestLookupIsCaseSensitive(t *testing.T) {
	db, _ := newSpikeDB(t, "case")
	seedPathFixtures(t, db)

	receipt := newReport("Gate 7c: lookups are case sensitive")

	low, high := SubtreeRange("/some")
	matching := queryInt(t, db, `SELECT COUNT(*) FROM path_index WHERE path_key >= ? AND path_key < ?`, low, high)
	upper := queryInt(t, db, `SELECT COUNT(*) FROM path_index WHERE path_key >= ? AND path_key < ?`,
		strings.ToUpper(low), strings.ToUpper(high))
	like := queryInt(t, db, `SELECT COUNT(*) FROM path_index WHERE path_key LIKE '/SOME/%'`)

	receipt.add("paths under /some", matching)
	receipt.add("paths under /SOME", upper)
	receipt.add("paths matching LIKE '/SOME/%'", like)
	receipt.add("exact /sometext rows", queryInt(t, db, `SELECT COUNT(*) FROM path_index WHERE path_key = '/some/text/'`))

	if matching == 0 {
		t.Fatal("the fixture recorded no paths under /some")
	}
	if upper != 0 {
		t.Errorf("an uppercase key range matched %d paths, want 0", upper)
	}
	if like != 0 {
		t.Errorf("LIKE '/SOME/%%' matched %d paths, want 0", like)
	}

	// Exact command lookup is case sensitive for the same reason.
	receipt.add("exact 'cat /x.txt'", queryInt(t, db, `SELECT COUNT(*) FROM command_index WHERE command = 'cat /x.txt'`))
	receipt.add("exact 'CAT /x.txt'", queryInt(t, db, `SELECT COUNT(*) FROM command_index WHERE command = 'CAT /x.txt'`))
	if rows := queryInt(t, db, `SELECT COUNT(*) FROM command_index WHERE command = 'CAT /x.txt'`); rows != 0 {
		t.Errorf("an uppercased command matched %d rows, want 0", rows)
	}
	receipt.add("conclusion", "case_sensitive_like is effective even though the driver reports no value for it")
	receipt.write(t, "gate7-case-sensitivity")
}

// TestNormalizedCommandLookupGroupsSpacingOnly qualifies the normalized command
// index: spacing variants share a row, but case variants stay separate because
// Unix command names are case sensitive.
func TestNormalizedCommandLookupGroupsSpacingOnly(t *testing.T) {
	db, _ := newSpikeDB(t, "normalized")

	receipt := newReport("Gate 7d: normalized command lookup")
	spacings := []string{"cat /some/file.txt", "cat  /some/file.txt", "  cat /some/file.txt\t", "cat /some/file.txt "}
	seedCommands(t, db, "session-spacing", spacings, "/some/file.txt")

	grouped := queryInt(t, db, `SELECT COUNT(*) FROM command_index WHERE normalized_command = ?`,
		NormalizeCommand("cat /some/file.txt"))
	receipt.add("spacing variants recorded", len(spacings))
	receipt.add("rows under the normalized key", grouped)
	if grouped != int64(len(spacings)) {
		t.Errorf("%d rows under the normalized key, want %d", grouped, len(spacings))
	}
	receipt.add("normalized form", fmt.Sprintf("%q -> %q", "  cat  /some/file.txt ", NormalizeCommand("  cat  /some/file.txt ")))

	// The same path under a differently cased command is a different command.
	seedCommands(t, db, "session-case", []string{"CAT /some/file.txt"}, "/some/file.txt")
	upper := queryInt(t, db, `SELECT COUNT(*) FROM command_index WHERE normalized_command = ?`,
		NormalizeCommand("CAT /some/file.txt"))
	receipt.add("rows under 'CAT /some/file.txt'", upper)
	if upper != 1 {
		t.Errorf("%d rows under the cased key, want 1: case must not be folded", upper)
	}

	// Grouping by cwd as well as command is what lets one normalized row be found
	// per working directory.
	receipt.add("rows for (normalized, cwd)", queryInt(t, db,
		`SELECT COUNT(*) FROM command_index WHERE normalized_command = ? AND cwd = ?`,
		NormalizeCommand("cat /some/file.txt"), "/some"))
	receipt.write(t, "gate7-normalized-command")
}

// TestLookupsAreIndexBacked records the query plans, because a correct query that
// scans is not a usable answer at the scale PLAN 10.2 describes. The design choice
// this gate qualifies is an index-backed key range rather than a LIKE prefix.
func TestLookupsAreIndexBacked(t *testing.T) {
	db, _ := newSpikeDB(t, "plans")
	seedPathFixtures(t, db)
	low, high := SubtreeRange("/some")

	receipt := newReport("Gate 7e: query plans")
	cases := []struct {
		label string
		query string
		args  []any
		// index names the index SQLite must use, or "" when the query has no
		// index to use and a scan is the honest answer.
		index string
	}{
		{"subtree range", `SELECT path FROM path_index WHERE path_key >= ? AND path_key < ?`, []any{low, high}, "path_index_key"},
		{"exact path", `SELECT event_id FROM path_index WHERE path = ?`, []any{"/some/file.txt"}, "path_index_path"},
		{"prefix LIKE", `SELECT path FROM path_index WHERE path_key LIKE ?`, []any{"/some/%"}, "path_index_key"},
		{"substring LIKE", `SELECT path FROM path_index WHERE path LIKE ?`, []any{"%file.txt%"}, ""},
		{"exact command", `SELECT event_id FROM command_index WHERE command = ?`, []any{"cat /x.txt"}, "command_index_exact"},
		{"normalized command", `SELECT event_id FROM command_index WHERE normalized_command = ?`, []any{"cat /x.txt"}, "command_index_normalized"},
		{"session transcript", `SELECT id FROM events WHERE session_id = ? ORDER BY occurred_at`, []any{"s"}, "events_user_time"},
		{"full-text search", `SELECT rowid FROM transcript_fts WHERE transcript_fts MATCH ?`, []any{"cat"}, ""},
	}

	for _, tc := range cases {
		plan := strings.Join(explain(t, db, tc.query, tc.args...), " | ")
		receipt.addf("%-20s %s", tc.label, plan)

		// SQLite prints SEARCH for an index lookup and SCAN for a full pass over a
		// table, a covering index, or a virtual table.
		searched := strings.Contains(plan, "SEARCH ")
		if tc.index == "" {
			continue
		}
		if !searched {
			t.Errorf("%s does not use %s: %s", tc.label, tc.index, plan)
			continue
		}
		if !strings.Contains(plan, tc.index) {
			t.Errorf("%s uses the wrong index, want %s: %s", tc.label, tc.index, plan)
		}
	}

	receipt.add("subtree range plan", strings.Join(explain(t, db,
		`SELECT path FROM path_index WHERE path_key >= ? AND path_key < ?`, low, high), " | "))
	receipt.add("substring plan", strings.Join(explain(t, db,
		`SELECT path FROM path_index WHERE path LIKE ?`, "%file.txt%"), " | "))
	receipt.add("conclusion", "prefix, exact, and session lookups are index searches; a substring LIKE falls back to a scan of the covering index, which is why the key range was chosen")
	receipt.write(t, "gate7-query-plans")
}

// seedPathFixtures records one event per path fixture, with the lookup projections
// and the search projection the transcript gate expects.
func seedPathFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	applySearchSchema(t, db)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin path fixtures: %v", err)
	}
	defer tx.Rollback()

	for i, fixture := range pathFixtures {
		recordEvent(t, tx, fmt.Sprintf("ev-path-%02d", i), "session-paths", i, "accepted",
			fixture.command, fixture.path, nil)
		if err := IndexEventForSearch(context.Background(), tx, fmt.Sprintf("ev-path-%02d", i),
			fixture.command, fixture.path); err != nil {
			t.Fatalf("index path fixture %d for search: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit path fixtures: %v", err)
	}
}

// seedCommands records commands against one path, so the normalized index has
// spacing variants of the same invocation to group. Each call uses its own session
// because the events table keeps one sequence per session.
func seedCommands(t *testing.T, db *sql.DB, sessionID string, commands []string, path string) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin command fixtures: %v", err)
	}
	defer tx.Rollback()

	for i, command := range commands {
		event := AcceptedEvent{
			ID:         fmt.Sprintf("ev-cmd-%s-%02d", sessionID, i),
			SessionID:  sessionID,
			Sequence:   i + 1,
			Kind:       "accepted",
			OccurredAt: fixedTimestamp(i + 1),
			Command:    command,
			Path:       path,
		}
		if err := appendAcceptedEvent(context.Background(), tx, event); err != nil {
			t.Fatalf("record command %q: %v", command, err)
		}
		// The recorded helper fixes the working directory at the namespace root;
		// the fixture sets the directory the command actually ran in, so the
		// (normalized_command, cwd) index has something to group on.
		if _, err := tx.ExecContext(context.Background(),
			`UPDATE command_index SET cwd = ? WHERE event_id = ?`, parentDirectory(path), event.ID); err != nil {
			t.Fatalf("set cwd for %q: %v", command, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit command fixtures: %v", err)
	}
}

// parentDirectory returns the directory a path belongs in, which is the working
// directory a command touching that path ran in.
func parentDirectory(path string) string {
	if index := strings.LastIndex(path, "/"); index > 0 {
		return path[:index]
	}
	return "/"
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
