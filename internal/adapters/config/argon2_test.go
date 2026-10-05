package config

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func fastParams() Params {
	return Params{Time: 1, MemoryKiB: 8 * 1024, Threads: 1, SaltLen: 8, KeyLen: 16}
}

func TestArgon2Roundtrip(t *testing.T) {
	if err := DefaultParams.Validate(); err != nil {
		t.Fatalf("default params invalid: %v", err)
	}
	if DefaultParams.MemoryKiB != 131072 || DefaultParams.Time != 3 || DefaultParams.Threads != 4 {
		t.Fatalf("default params changed: %+v", DefaultParams)
	}
	hash, err := GenerateHash([]byte("correct horse battery staple"), fastParams())
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyHash(hash, []byte("correct horse battery staple"))
	if err != nil || !ok {
		t.Fatalf("verify: %v %v", ok, err)
	}
	ok, err = VerifyHash(hash, []byte("wrong"))
	if err != nil || ok {
		t.Fatalf("wrong password accepted: %v %v", ok, err)
	}
}

func TestArgon2DecodeBounds(t *testing.T) {
	salt := base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef"))
	key := base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	valid := fmt.Sprintf("$argon2id$v=19$m=8192,t=1,p=1$%s$%s", salt, key)
	if _, _, _, err := decodePHC(valid); err != nil {
		t.Fatalf("valid encoding rejected: %v", err)
	}
	for _, bad := range []string{
		"", // empty
		"$argon2i$v=19$m=8192,t=1,p=1$" + salt + "$" + key,                       // argon2i rejected
		"$argon2d$v=19$m=8192,t=1,p=1$" + salt + "$" + key,                       // argon2d rejected
		"$argon2id$v=16$m=8192,t=1,p=1$" + salt + "$" + key,                      // wrong version
		fmt.Sprintf("$argon2id$v=19$m=4,t=1,p=1$%s$%s", salt, key),               // below min memory
		fmt.Sprintf("$argon2id$v=19$m=%d,t=1,p=1$%s$%s", 2*1024*1024, salt, key), // above max memory
		fmt.Sprintf("$argon2id$v=19$m=8192,t=0,p=1$%s$%s", salt, key),            // zero time
		fmt.Sprintf("$argon2id$v=19$m=8192,t=99,p=1$%s$%s", salt, key),           // time too large
		fmt.Sprintf("$argon2id$v=19$m=8192,t=1,p=0$%s$%s", salt, key),            // zero lanes
		fmt.Sprintf("$argon2id$v=19$m=8192,t=1,p=9$%s$%s", salt, key),            // lanes above bound
		fmt.Sprintf("$argon2id$v=19$m=-8192,t=1,p=1$%s$%s", salt, key),           // negative
		"$argon2id$v=19$m=abc,t=1,p=1$" + salt + "$" + key,                       // not a number
		"$argon2id$v=19$m=8192,t=1$" + salt + "$" + key,                          // missing lane
		"$argon2id$v=19$m=8192,t=1,p=1$$" + key,                                  // empty salt
		"$argon2id$v=19$m=8192,t=1,p=1$" + salt + "$not!base64!",                 // bad base64
	} {
		if _, _, _, err := decodePHC(bad); err == nil {
			t.Fatalf("decodePHC accepted %q", bad)
		}
	}
}

func TestParamsValidateBounds(t *testing.T) {
	if err := fastParams().Validate(); err != nil {
		t.Fatalf("fast params: %v", err)
	}
	p := fastParams()
	p.MemoryKiB = 8192 * 1024
	if err := p.Validate(); err == nil {
		t.Fatal("oversized memory accepted")
	}
	p = fastParams()
	p.SaltLen = 4
	if err := p.Validate(); err == nil {
		t.Fatal("short salt accepted")
	}
	if _, err := GenerateHash(nil, fastParams()); err == nil {
		t.Fatal("empty password hashed")
	}
}

func TestPasswordStoreVerifyDummy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "passwords.json")
	s, err := CreatePasswordStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rnd := &fixedRandom{}
	id, err := s.AddUser(rnd, "alice", []byte("s3cret"), fastParams())
	if err != nil {
		t.Fatal(err)
	}
	if id.String() == "" {
		t.Fatal("empty identity")
	}
	user, ok, err := s.Verify("alice", []byte("s3cret"))
	if err != nil || !ok || user.Username != "alice" {
		t.Fatalf("verify: %v %v %+v", user, ok, err)
	}
	if _, ok, _ := s.Verify("alice", []byte("nope")); ok {
		t.Fatal("wrong password accepted")
	}
	// Unknown and disabled users must still perform a full verification:
	// the store must not error or short-circuit.
	if _, ok, err := s.Verify("mallory", []byte("s3cret")); err != nil || ok {
		t.Fatalf("unknown user: %v %v", ok, err)
	}
	if err := s.SetEnabled("alice", false); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Verify("alice", []byte("s3cret")); err != nil || ok {
		t.Fatalf("disabled user: %v %v", ok, err)
	}
}

func TestReadPasswordBounds(t *testing.T) {
	pw, err := ReadPassword(strings.NewReader("hunter2\n"))
	if err != nil || string(pw) != "hunter2" {
		t.Fatalf("read: %q %v", pw, err)
	}
	if _, err := ReadPassword(strings.NewReader("\n")); err == nil {
		t.Fatal("empty password accepted")
	}
	if _, err := ReadPassword(strings.NewReader(strings.Repeat("a", 2000) + "\n")); err == nil {
		t.Fatal("oversized password accepted")
	}
	if _, err := GenerateHashFromReader(strings.NewReader("hunter2\n"), fastParams()); err != nil {
		t.Fatal(err)
	}
}
