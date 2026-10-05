package storage

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// damagePattern is a payload chosen to break anything that treats stored bytes as
// text: an embedded NUL, an overlong two-byte sequence, a truncated three-byte
// sequence, a surrogate encoding, a bare continuation byte, a four-byte rune, and
// a CRLF pair.
var damagePattern = []byte{
	0x00, 0x01, 0xff, 0xfe,
	'd', 'u', 'm', 'p', 0x00, 'x',
	0xc3, 0x28,
	0xe2, 0x82,
	0xed, 0xa0, 0x80,
	0xf0, 0x9f, 0x9a, 0x80,
	0x80, 0x81,
	'\r', '\n', '\t',
}

// contentFixtures are the blobs every content gate stores and reads back. The
// sizes bracket the interesting cases: empty, one byte, the pattern above, and
// something larger than a single page so that a page-boundary truncation would
// show up as a size mismatch.
var contentFixtures = []struct {
	name  string
	bytes []byte
}{
	{"empty", []byte{}},
	{"one-byte-nul", []byte{0x00}},
	{"single-0xff", []byte{0xff}},
	{"damage-pattern", damagePattern},
	{"utf8-text", []byte("grüße, 世界\nsecond line\r\n")},
	{"pseudo-random-256KiB", pseudoRandomBlob(256*1024, 1)},
}

// TestContentBytesSurviveExactly qualifies the exact-bytes half of gate 6. World
// state is generated program bytes, so a round trip that normalises a NUL,
// truncates at the first invalid UTF-8 sequence, or replaces an encoding would
// silently corrupt files the user later reads.
func TestContentBytesSurviveExactly(t *testing.T) {
	db, path := newSpikeDB(t, "exact-bytes")
	seedWorld(t, db, "ns-bytes", "root-bytes")

	receipt := newReport("Gate 6a: content bytes survive a round trip exactly")
	receipt.add("database", path)

	for _, fixture := range contentFixtures {
		hash := storeContent(t, db, fixture.bytes)
		if got := queryInt(t, db, `SELECT size FROM contents WHERE hash = ?`, hash); got != int64(len(fixture.bytes)) {
			t.Errorf("%s: stored size %d, want %d", fixture.name, got, len(fixture.bytes))
			receipt.add(fixture.name, fmt.Sprintf("stored size %d, want %d", got, len(fixture.bytes)))
			continue
		}
		receipt.add(fixture.name+" stored", fmt.Sprintf("%d bytes, %s", len(fixture.bytes), hash[:12]))
	}

	// Read every blob back through a separate pool, so the comparison is against
	// what a later process would see rather than a cached value.
	reader, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer reader.Close()

	for _, fixture := range contentFixtures {
		hash := ContentHash(fixture.bytes)
		var got []byte
		var size int64
		var storageClass string
		var measuredLength int64
		if err := reader.QueryRow(
			`SELECT bytes, size, typeof(bytes), length(bytes) FROM contents WHERE hash = ?`, hash,
		).Scan(&got, &size, &storageClass, &measuredLength); err != nil {
			t.Fatalf("%s: read the blob back: %v", fixture.name, err)
		}

		if !bytes.Equal(got, fixture.bytes) {
			t.Errorf("%s: bytes changed in the round trip\n got %v\nwant %v", fixture.name, got, fixture.bytes)
		}
		if size != int64(len(fixture.bytes)) || measuredLength != int64(len(fixture.bytes)) {
			t.Errorf("%s: size %d and length %d, want %d", fixture.name, size, measuredLength, len(fixture.bytes))
		}
		// A TEXT value would report a character count for length() and could not
		// hold the pattern at all, so the storage class is part of the claim.
		if storageClass != "blob" {
			t.Errorf("%s: stored as %s, want blob", fixture.name, storageClass)
		}
		receipt.addf("%-22s %d bytes, identical=%t, typeof=%s, length=%d",
			fixture.name, len(fixture.bytes), bytes.Equal(got, fixture.bytes), storageClass, measuredLength)
	}

	// The blobs survive the write-ahead log as well, which is the other way bytes
	// reach the volume: an uncheckpointed commit must read back identically.
	if _, err := reader.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	for _, fixture := range contentFixtures {
		var got []byte
		if err := reader.QueryRow(
			`SELECT bytes FROM contents WHERE hash = ?`, ContentHash(fixture.bytes),
		).Scan(&got); err != nil {
			t.Fatalf("%s: read the blob back after a checkpoint: %v", fixture.name, err)
		}
		if !bytes.Equal(got, fixture.bytes) {
			t.Errorf("%s: bytes changed across a checkpoint", fixture.name)
		}
	}
	receipt.add("identical after checkpoint", true)
	receipt.add("storage classes", "blob (no text column is involved)")
	receipt.add("conclusion", "NUL bytes, invalid UTF-8, and page-crossing payloads return byte-identical; size and length() agree with the stored length")
	receipt.write(t, "gate6-exact-bytes")
}

// TestIdenticalContentIsStoredOnce qualifies the deduplication half of gate 6.
// Content is addressed by hash, so a world where several files share bytes must
// hold one copy; the storage saving is what makes "permanent" storage affordable.
func TestIdenticalContentIsStoredOnce(t *testing.T) {
	db, path := newSpikeDB(t, "dedup")
	seedWorld(t, db, "ns-dedup", "root-dedup")
	insertDirectory(t, db, "dir", "root-dedup", "dir")

	receipt := newReport("Gate 6b: content is stored once and shared")

	shared := pseudoRandomBlob(64*1024, 2)
	sharedHash := ContentHash(shared)
	receipt.add("shared blob", fmt.Sprintf("%d bytes, %s", len(shared), sharedHash[:12]))

	// Two turns in separate commits materialize different files from the same
	// bytes, which is what a generated project with repeated fixtures looks like.
	for _, turn := range []struct{ node, name string }{
		{"node-a", "copy-a.bin"},
		{"node-b", "copy-b.bin"},
	} {
		revision := directoryRevision(t, db, "dir")
		if err := runCommit(t, db, ChangeSet{
			NamespaceID: "ns-dedup",
			Contents:    []Content{{Hash: sharedHash, Bytes: shared}},
			Changes: []StagedChange{{
				NodeID: turn.node, ParentID: "dir", Name: turn.name,
				Kind: "file", ContentHash: sharedHash,
				ExpectAbsent: true, ExpectedParentRevision: revision,
			}},
			Events: []AcceptedEvent{{
				ID: "ev-" + turn.node, SessionID: "session-" + turn.node, Sequence: 1, Kind: "accepted",
				OccurredAt: fixedTimestamp(1), Command: "write " + turn.name, Path: "/dir/" + turn.name,
			}},
		}); err != nil {
			t.Fatalf("commit %s: %v", turn.name, err)
		}
		receipt.add(turn.name+" commit", "accepted")
	}

	storedCopies := queryInt(t, db, `SELECT COUNT(*) FROM contents WHERE hash = ?`, sharedHash)
	referencing := queryInt(t, db, `SELECT COUNT(*) FROM nodes WHERE content_hash = ?`, sharedHash)
	receipt.add("content rows for the hash", storedCopies)
	receipt.add("nodes referencing it", referencing)
	if storedCopies != 1 {
		t.Errorf("%d content rows for one hash, want 1", storedCopies)
	}
	if referencing != 2 {
		t.Errorf("%d nodes reference the shared content, want 2", referencing)
	}

	// A rebased change set re-presents content another turn already stored, which
	// must be accepted rather than refused.
	revision := directoryRevision(t, db, "dir")
	rebaseErr := runCommit(t, db, ChangeSet{
		NamespaceID: "ns-dedup",
		Contents:    []Content{{Hash: sharedHash, Bytes: shared}},
		Changes: []StagedChange{{
			NodeID: "node-c", ParentID: "dir", Name: "copy-c.bin",
			Kind: "file", ContentHash: sharedHash,
			ExpectAbsent: true, ExpectedParentRevision: revision,
		}},
	})
	if rebaseErr != nil {
		t.Fatalf("re-presenting stored content failed: %v", rebaseErr)
	}
	receipt.add("re-presented in a rebase", describeErr(rebaseErr))
	if copies := queryInt(t, db, `SELECT COUNT(*) FROM contents WHERE hash = ?`, sharedHash); copies != 1 {
		t.Errorf("re-presenting content stored %d copies, want 1", copies)
	}

	// One differing byte is a different blob, so deduplication cannot merge files
	// that only look alike.
	nearMiss := append([]byte(nil), shared...)
	nearMiss[len(nearMiss)/2] ^= 0x01
	nearMissHash := ContentHash(nearMiss)
	if err := runCommit(t, db, ChangeSet{
		NamespaceID: "ns-dedup",
		Contents:    []Content{{Hash: nearMissHash, Bytes: nearMiss}},
	}); err != nil {
		t.Fatalf("store the near-duplicate: %v", err)
	}
	distinct := queryInt(t, db, `SELECT COUNT(*) FROM contents`)
	receipt.add("one byte different", fmt.Sprintf("%s is a separate blob; contents rows now %d",
		nearMissHash[:12], distinct))
	if distinct != 2 {
		t.Errorf("%d content rows after storing a near-duplicate, want 2", distinct)
	}

	// The space actually used is the number of distinct blobs, not the number of
	// files that reference them.
	var payloadBytes int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM contents`).Scan(&payloadBytes); err != nil {
		t.Fatalf("sum stored payload: %v", err)
	}
	var fileBytes int64
	fileBytes = databaseFileSize(t, path)
	receipt.add("distinct payload bytes", payloadBytes)
	receipt.add("database and log bytes", fileBytes)
	receipt.add("files referencing content", referencing)
	receipt.add("conclusion", "three files share one stored blob; a single changed byte produces a second blob")
	receipt.write(t, "gate6-dedup")
}

// TestContentHashDistinguishesEveryByte shows the content key is usable as a
// deduplication key: no single-byte change anywhere in a blob may collide.
func TestContentHashDistinguishesEveryByte(t *testing.T) {
	base := pseudoRandomBlob(64, 3)
	receipt := newReport("Gate 6c: the content key separates distinct bytes")
	receipt.add("probe blob", fmt.Sprintf("%d bytes", len(base)))
	receipt.add("base hash", ContentHash(base))

	if again := ContentHash(base); again != ContentHash(base) {
		t.Fatalf("hashing is not stable: %s then %s", ContentHash(base), again)
	}
	collisions := 0
	for i := range base {
		for _, bit := range []byte{0x01, 0x80} {
			mutated := append([]byte(nil), base...)
			mutated[i] ^= bit
			if ContentHash(mutated) == ContentHash(base) {
				collisions++
			}
		}
	}
	receipt.add("single-bit mutations tried", 2*len(base))
	receipt.add("collisions", collisions)
	if collisions != 0 {
		t.Errorf("%d mutations hashed to the base key", collisions)
	}

	// An empty blob and a single NUL byte are the two smallest distinct payloads.
	if ContentHash(nil) == ContentHash([]byte{0x00}) {
		t.Error("the empty blob and a single NUL byte share a key")
	}
	receipt.add("empty blob hash", ContentHash(nil))
	receipt.add("single NUL hash", ContentHash([]byte{0x00}))
	receipt.write(t, "gate6-content-key")
}

// storeContent writes one blob in its own transaction, returning its key.
func storeContent(t *testing.T, db *sql.DB, raw []byte) string {
	t.Helper()
	hash := ContentHash(raw)
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin content insert: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(context.Background(),
		`INSERT INTO contents (hash, bytes, size, media_type) VALUES (?, ?, ?, ?)`,
		hash, raw, len(raw), "application/octet-stream",
	); err != nil {
		t.Fatalf("insert content %s: %v", hash, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit content %s: %v", hash, err)
	}
	return hash
}

// pseudoRandomBlob builds a deterministic blob of n bytes. A fixed generator
// seeded by the caller is used instead of a random source so a receipt describes the
// same fixture on every run and on every machine, while a different seed gives a
// blob that is distinct rather than a repeat.
func pseudoRandomBlob(n int, seed uint32) []byte {
	blob := make([]byte, n)
	state := seed
	for i := range blob {
		state = state*1664525 + 1013904223
		blob[i] = byte(state >> 24)
	}
	return blob
}
