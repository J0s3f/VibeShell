package auth

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// fixtureEntry builds an obviously-fake account. The hash is derived from a
// fake password; no real credential appears anywhere in this package.
func fixtureEntry(t *testing.T, username string, enabled bool) UserEntry {
	t.Helper()
	hash := mustEncode(t, fakePassword, fastTestParams, testSalt).String()
	return UserEntry{
		Username:    username,
		Hash:        hash,
		Enabled:     enabled,
		IdentityRef: "sec-identity-" + username,
	}
}

func fixtureFile(t *testing.T, users ...UserEntry) PasswordFile {
	t.Helper()
	return PasswordFile{
		Schema:  PasswordFileSchema,
		Version: PasswordFileVersion,
		Users:   users,
	}
}

func TestPasswordFileRoundTrip(t *testing.T) {
	file := fixtureFile(t,
		fixtureEntry(t, "alice", true),
		fixtureEntry(t, "bob", false),
		fixtureEntry(t, "café-user", true),
	)

	encoded, err := file.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	parsed, err := ParsePasswordFile(encoded)
	if err != nil {
		t.Fatalf("ParsePasswordFile: %v", err)
	}
	if parsed.Schema != PasswordFileSchema || parsed.Version != PasswordFileVersion {
		t.Errorf("schema/version = %q/%d", parsed.Schema, parsed.Version)
	}
	if len(parsed.Users) != len(file.Users) {
		t.Fatalf("user count = %d, want %d", len(parsed.Users), len(file.Users))
	}
	for i, want := range file.Users {
		if parsed.Users[i] != want {
			t.Errorf("user %d = %+v, want %+v", i, parsed.Users[i], want)
		}
	}

	entry, found := parsed.Find("bob")
	if !found {
		t.Fatal("Find(bob) not found")
	}
	if entry.Enabled {
		t.Error("bob should be disabled")
	}
	if entry.IdentityRef != "sec-identity-bob" {
		t.Errorf("IdentityRef = %q", entry.IdentityRef)
	}
	if _, found := parsed.Find("mallory"); found {
		t.Error("Find(mallory) unexpectedly found")
	}
	// Exact byte matching: no case folding or normalization.
	if _, found := parsed.Find("Alice"); found {
		t.Error("Find(Alice) must not match alice")
	}
}

func TestPasswordFileRejectsDuplicateUsername(t *testing.T) {
	duplicate := fixtureFile(t, fixtureEntry(t, "alice", true), fixtureEntry(t, "alice", false))
	data, err := duplicate.Encode()
	if !errors.Is(err, ErrDuplicateUsername) {
		t.Fatalf("Encode duplicate error = %v, want ErrDuplicateUsername", err)
	}
	if data != nil {
		t.Errorf("Encode returned data for an invalid file: %q", data)
	}
}

func TestPasswordFileKeepsDistinctUsernamesDistinct(t *testing.T) {
	file := fixtureFile(t, fixtureEntry(t, "Alice", true), fixtureEntry(t, "alice", true))
	if _, err := file.Encode(); err != nil {
		t.Fatalf("Encode: %v", err)
	}
}

func TestPasswordFileRejectsWrongSchema(t *testing.T) {
	file := fixtureFile(t, fixtureEntry(t, "alice", true))
	file.Schema = "other/password-file"
	if _, err := file.Encode(); !errors.Is(err, ErrSchemaMismatch) {
		t.Errorf("schema error = %v, want ErrSchemaMismatch", err)
	}

	file = fixtureFile(t, fixtureEntry(t, "alice", true))
	file.Version = 2
	if _, err := file.Encode(); !errors.Is(err, ErrSchemaMismatch) {
		t.Errorf("version error = %v, want ErrSchemaMismatch", err)
	}
}

func TestPasswordFileParseRejectsInvalidJSONShape(t *testing.T) {
	valid, err := fixtureFile(t, fixtureEntry(t, "alice", true)).Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	cases := []struct {
		name string
		data string
	}{
		{"not json", "definitely not json"},
		{"unknown field", `{"schema":"vibeshell/password-file","version":1,"users":[],"extra":true}`},
		{"trailing data", string(valid) + `{}`},
		{"trailing garbage", string(valid) + "junk"},
		{"missing schema", `{"version":1,"users":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePasswordFile([]byte(tc.data)); err == nil {
				t.Fatalf("ParsePasswordFile(%q) succeeded, want error", tc.data)
			}
		})
	}
}

func TestPasswordFileRejectsInvalidEntries(t *testing.T) {
	hash := mustEncode(t, fakePassword, fastTestParams, testSalt).String()

	cases := []struct {
		name    string
		entry   UserEntry
		wantErr error
	}{
		{"empty username", UserEntry{Hash: hash, Enabled: true, IdentityRef: "ref"}, ErrInvalidEntry},
		{"control character in username", UserEntry{Username: "al\nice", Hash: hash, IdentityRef: "ref"}, ErrInvalidEntry},
		{"username too long", UserEntry{Username: strings.Repeat("u", MaxUsernameBytes+1), Hash: hash, IdentityRef: "ref"}, ErrInvalidEntry},
		{"empty identity ref", UserEntry{Username: "alice", Hash: hash, IdentityRef: ""}, ErrInvalidEntry},
		{"control character in identity ref", UserEntry{Username: "alice", Hash: hash, IdentityRef: "ref\x01"}, ErrInvalidEntry},
		{"malformed hash", UserEntry{Username: "alice", Hash: "not-a-hash", IdentityRef: "ref"}, ErrInvalidEntry},
		{
			"hash parameters out of bounds",
			UserEntry{Username: "alice", Hash: "$argon2id$v=19$m=1073741824,t=8,p=16$c2FstHNhbHQ$c2hhc2hoYXNoaGFzaA", IdentityRef: "ref"},
			ErrParamsOutOfRange,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := fixtureFile(t, tc.entry)
			_, err := file.Encode()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Encode error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestPasswordFileNeverStoresPlaintext(t *testing.T) {
	file := fixtureFile(t, fixtureEntry(t, "alice", true))
	data, err := file.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if bytes.Contains(data, []byte(fakePassword)) {
		t.Error("encoded file contains the fixture password")
	}
	if bytes.Contains(bytes.ToLower(data), []byte(`"password"`)) {
		t.Error("encoded file contains a password field")
	}
}

func TestFileStoreKeepsPreviousSnapshotOnFailedLoad(t *testing.T) {
	var store FileStore
	if _, loaded := store.Current(); loaded {
		t.Fatal("empty store reported loaded")
	}

	first := fixtureFile(t, fixtureEntry(t, "alice", true))
	if err := store.Load(mustEncodeFile(t, first)); err != nil {
		t.Fatalf("Load first: %v", err)
	}
	current, loaded := store.Current()
	if !loaded || len(current.Users) != 1 || current.Users[0].Username != "alice" {
		t.Fatalf("current snapshot = %+v loaded=%v", current, loaded)
	}

	broken := []byte(`{"schema":"vibeshell/password-file","version":1,"users":[` +
		`{"username":"mallory","hash":"nope","enabled":true,"identityRef":"ref"}]}`)
	if err := store.Load(broken); err == nil {
		t.Fatal("Load broken file succeeded")
	}
	current, loaded = store.Current()
	if !loaded || len(current.Users) != 1 || current.Users[0].Username != "alice" {
		t.Fatalf("failed load replaced the snapshot: %+v", current)
	}

	second := fixtureFile(t, fixtureEntry(t, "alice", false), fixtureEntry(t, "bob", true))
	if err := store.Load(mustEncodeFile(t, second)); err != nil {
		t.Fatalf("Load second: %v", err)
	}
	current, _ = store.Current()
	if len(current.Users) != 2 {
		t.Fatalf("current users = %d, want 2", len(current.Users))
	}
}

func mustEncodeFile(t *testing.T, file PasswordFile) []byte {
	t.Helper()
	data, err := file.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return data
}
