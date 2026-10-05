package auth

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// Fixture values are obviously fake; nothing here is a real credential.
const fakePassword = "correct-horse-battery-staple-fake"

var (
	testSalt       = []byte("0123456789abcdef")
	fastTestParams = Params{MemoryKiB: MinMemoryKiB, Iterations: MinIterations, Parallelism: 1}
)

func b64(s string) string {
	return base64.RawStdEncoding.EncodeToString([]byte(s))
}

func mustEncode(t *testing.T, password string, params Params, salt []byte) EncodedHash {
	t.Helper()
	encoded, err := NewEncodedHash([]byte(password), params, salt)
	if err != nil {
		t.Fatalf("NewEncodedHash: %v", err)
	}
	return encoded
}

// nonCanonicalSalt returns a base64 string that decodes to the same bytes as
// canonical but re-encodes differently, by flipping a discarded trailing bit.
func nonCanonicalSalt(t *testing.T) string {
	t.Helper()
	canonical := b64("saltsalt")
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	last := canonical[len(canonical)-1]
	idx := strings.IndexByte(alphabet, last)
	if idx < 0 {
		t.Fatalf("last character %q not in base64 alphabet", last)
	}
	flipped := alphabet[idx^1]
	if flipped == last {
		t.Fatalf("could not flip trailing bits of %q", canonical)
	}
	mutated := canonical[:len(canonical)-1] + string(flipped)
	original, err := base64.RawStdEncoding.DecodeString(canonical)
	if err != nil {
		t.Fatalf("decode canonical: %v", err)
	}
	decoded, err := base64.RawStdEncoding.DecodeString(mutated)
	if err != nil {
		// The decoder itself already rejects the mutation; that still
		// exercises the malformed path.
		return mutated
	}
	if string(decoded) != string(original) {
		t.Fatalf("mutated salt decodes to different bytes")
	}
	if base64.RawStdEncoding.EncodeToString(decoded) == mutated {
		t.Fatalf("mutation did not break canonicality")
	}
	return mutated
}

func TestEncodedHashRoundTrip(t *testing.T) {
	original := mustEncode(t, fakePassword, fastTestParams, testSalt)

	encoded := original.String()
	if strings.Count(encoded, "$") != 5 {
		t.Fatalf("String() = %q, want five $ separators", encoded)
	}

	parsed, err := ParseEncoded(encoded)
	if err != nil {
		t.Fatalf("ParseEncoded(%q): %v", encoded, err)
	}
	if parsed.Params != original.Params {
		t.Errorf("Params = %+v, want %+v", parsed.Params, original.Params)
	}
	if string(parsed.Salt) != string(original.Salt) {
		t.Errorf("Salt = %q, want %q", parsed.Salt, original.Salt)
	}
	if string(parsed.Hash) != string(original.Hash) {
		t.Errorf("Hash bytes differ after round trip")
	}
	if parsed.String() != encoded {
		t.Errorf("re-encoding = %q, want %q", parsed.String(), encoded)
	}
}

func TestParseEncodedRejectsMalformed(t *testing.T) {
	salt8 := b64("saltsalt")               // 8 bytes, the minimum accepted salt
	hash16 := b64(strings.Repeat("h", 16)) // minimum accepted hash
	hash65 := b64(strings.Repeat("h", 65)) // one byte over the maximum
	valid := "$argon2id$v=19$m=8192,t=1,p=1$" + salt8 + "$" + hash16

	cases := []struct {
		name    string
		encoded string
		wantErr error
	}{
		{"empty", "", ErrMalformedHash},
		{"no leading dollar", "argon2id$v=19$m=8192,t=1,p=1$" + salt8 + "$" + hash16, ErrMalformedHash},
		{"missing fields", "$argon2id$v=19$m=8192,t=1,p=1$" + salt8, ErrMalformedHash},
		{"trailing field", valid + "$extra", ErrMalformedHash},
		{"trailing garbage", valid + "!", ErrMalformedHash},
		{"wrong algorithm", "$argon2i$v=19$m=8192,t=1,p=1$" + salt8 + "$" + hash16, ErrMalformedHash},
		{"wrong algorithm d", "$argon2d$v=19$m=8192,t=1,p=1$" + salt8 + "$" + hash16, ErrMalformedHash},
		{"wrong version", "$argon2id$v=16$m=8192,t=1,p=1$" + salt8 + "$" + hash16, ErrMalformedHash},
		{"version without v=", "$argon2id$19$m=8192,t=1,p=1$" + salt8 + "$" + hash16, ErrMalformedHash},
		{"missing m prefix", "$argon2id$v=19$t=1,m=8192,p=1$" + salt8 + "$" + hash16, ErrMalformedHash},
		{"reordered params", "$argon2id$v=19$p=1,t=1,m=8192$" + salt8 + "$" + hash16, ErrMalformedHash},
		{"missing param", "$argon2id$v=19$m=8192,t=1$" + salt8 + "$" + hash16, ErrMalformedHash},
		{"extra param", "$argon2id$v=19$m=8192,t=1,p=1,s=2$" + salt8 + "$" + hash16, ErrMalformedHash},
		{"non-digit memory", "$argon2id$v=19$m=8k19,t=1,p=1$" + salt8 + "$" + hash16, ErrMalformedHash},
		{"negative memory", "$argon2id$v=19$m=-8192,t=1,p=1$" + salt8 + "$" + hash16, ErrMalformedHash},
		{"empty value", "$argon2id$v=19$m=,t=1,p=1$" + salt8 + "$" + hash16, ErrMalformedHash},
		{"memory below minimum", "$argon2id$v=19$m=7168,t=1,p=1$" + salt8 + "$" + hash16, ErrParamsOutOfRange},
		{"memory zero", "$argon2id$v=19$m=0,t=1,p=1$" + salt8 + "$" + hash16, ErrParamsOutOfRange},
		{"memory above maximum", "$argon2id$v=19$m=524289,t=1,p=1$" + salt8 + "$" + hash16, ErrParamsOutOfRange},
		{"memory does not fit", "$argon2id$v=19$m=4294967296,t=1,p=1$" + salt8 + "$" + hash16, ErrMalformedHash},
		{"iterations zero", "$argon2id$v=19$m=8192,t=0,p=1$" + salt8 + "$" + hash16, ErrParamsOutOfRange},
		{"iterations above maximum", "$argon2id$v=19$m=8192,t=9,p=1$" + salt8 + "$" + hash16, ErrParamsOutOfRange},
		{"parallelism zero", "$argon2id$v=19$m=8192,t=1,p=0$" + salt8 + "$" + hash16, ErrParamsOutOfRange},
		{"parallelism above maximum", "$argon2id$v=19$m=8192,t=1,p=17$" + salt8 + "$" + hash16, ErrParamsOutOfRange},
		{"parallelism does not fit", "$argon2id$v=19$m=8192,t=1,p=300$" + salt8 + "$" + hash16, ErrMalformedHash},
		{"salt too short", "$argon2id$v=19$m=8192,t=1,p=1$" + b64("short") + "$" + hash16, ErrParamsOutOfRange},
		{"salt too long", "$argon2id$v=19$m=8192,t=1,p=1$" + b64(strings.Repeat("s", 65)) + "$" + hash16, ErrParamsOutOfRange},
		{"empty salt", "$argon2id$v=19$m=8192,t=1,p=1$$" + hash16, ErrMalformedHash},
		{"hash too short", "$argon2id$v=19$m=8192,t=1,p=1$" + salt8 + "$" + b64(strings.Repeat("h", 15)), ErrParamsOutOfRange},
		{"hash too long", "$argon2id$v=19$m=8192,t=1,p=1$" + salt8 + "$" + hash65, ErrParamsOutOfRange},
		{"empty hash", "$argon2id$v=19$m=8192,t=1,p=1$" + salt8 + "$", ErrMalformedHash},
		{"padded base64 salt", "$argon2id$v=19$m=8192,t=1,p=1$" + salt8 + "=$" + hash16, ErrMalformedHash},
		{"non-canonical salt", "$argon2id$v=19$m=8192,t=1,p=1$" + nonCanonicalSalt(t) + "$" + hash16, ErrMalformedHash},
		{"invalid base64 character", "$argon2id$v=19$m=8192,t=1,p=1$" + salt8 + "*$" + hash16, ErrMalformedHash},
		{"whitespace inside", "$argon2id$v=19$m=8192,t=1,p=1$salt salt$" + hash16, ErrMalformedHash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseEncoded(tc.encoded)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseEncoded(%q) error = %v, want %v", tc.encoded, err, tc.wantErr)
			}
		})
	}
}

func TestParseEncodedAcceptsValid(t *testing.T) {
	encoded := "$argon2id$v=19$m=65536,t=3,p=4$" + b64("saltsaltsaltsalt") + "$" + b64(strings.Repeat("h", 32))
	parsed, err := ParseEncoded(encoded)
	if err != nil {
		t.Fatalf("ParseEncoded: %v", err)
	}
	if parsed.Params != (Params{MemoryKiB: 65536, Iterations: 3, Parallelism: 4}) {
		t.Errorf("Params = %+v", parsed.Params)
	}
	if len(parsed.Salt) != 16 || len(parsed.Hash) != 32 {
		t.Errorf("salt/hash lengths = %d/%d, want 16/32", len(parsed.Salt), len(parsed.Hash))
	}
	if parsed.String() != encoded {
		t.Errorf("re-encoding = %q, want %q", parsed.String(), encoded)
	}
}

func TestParamsValidateBounds(t *testing.T) {
	cases := []struct {
		name    string
		params  Params
		wantErr error
	}{
		{"minimums ok", Params{MemoryKiB: MinMemoryKiB, Iterations: MinIterations, Parallelism: MinParallelism}, nil},
		{"maximums ok", Params{MemoryKiB: MaxMemoryKiB, Iterations: MaxIterations, Parallelism: MaxParallelism}, nil},
		{"memory one below minimum", Params{MemoryKiB: MinMemoryKiB - 1, Iterations: 1, Parallelism: 1}, ErrParamsOutOfRange},
		{"memory one above maximum", Params{MemoryKiB: MaxMemoryKiB + 1, Iterations: 1, Parallelism: 1}, ErrParamsOutOfRange},
		{"iterations one above maximum", Params{MemoryKiB: MinMemoryKiB, Iterations: MaxIterations + 1, Parallelism: 1}, ErrParamsOutOfRange},
		{"parallelism one above maximum", Params{MemoryKiB: MinMemoryKiB, Iterations: 1, Parallelism: MaxParallelism + 1}, ErrParamsOutOfRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.params.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewEncodedHashRejectsOutOfRangeInput(t *testing.T) {
	if _, err := NewEncodedHash([]byte(fakePassword), Params{MemoryKiB: 1, Iterations: 1, Parallelism: 1}, testSalt); !errors.Is(err, ErrParamsOutOfRange) {
		t.Errorf("params error = %v, want ErrParamsOutOfRange", err)
	}
	if _, err := NewEncodedHash([]byte(fakePassword), fastTestParams, []byte("short")); !errors.Is(err, ErrParamsOutOfRange) {
		t.Errorf("short salt error = %v, want ErrParamsOutOfRange", err)
	}
	longSalt := make([]byte, MaxSaltBytes+1)
	if _, err := NewEncodedHash([]byte(fakePassword), fastTestParams, longSalt); !errors.Is(err, ErrParamsOutOfRange) {
		t.Errorf("long salt error = %v, want ErrParamsOutOfRange", err)
	}
}
