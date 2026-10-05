package auth

import (
	"errors"
	"strings"
	"testing"
)

// FuzzParseEncoded checks that the parser never panics, only reports the
// documented error sentinels, and that every accepted value is in bounds and
// canonically re-encodable (encode(parse(encode(x))) is stable).
func FuzzParseEncoded(f *testing.F) {
	valid := validEncodedForFuzz()
	f.Add("")
	f.Add(valid)
	f.Add("$")
	f.Add("$argon2id$v=19$m=8192,t=1,p=1")
	f.Add("$argon2id$v=19$m=8192,t=1,p=1$c2FsdHNhbHQ$c2hhc2hoYXNoaGFzaA")
	f.Add("$argon2id$v=19$m=8192,t=1,p=1$c2FsdHNhbHQ$c2hhc2hoYXNoaGFzaA$")
	f.Add("$argon2id$v=19$m=99999999999999999999,t=1,p=1$c2FsdHNhbHQ$c2hhc2hoYXNoaGFzaA")
	f.Add("$argon2id$v=19$m=8192,t=1,p=1,p=2$c2FsdHNhbHQ$c2hhc2hoYXNoaGFzaA")
	f.Add(valid + strings.Repeat("A", 1000))

	f.Fuzz(func(t *testing.T, encoded string) {
		parsed, err := ParseEncoded(encoded)
		if err != nil {
			if !errors.Is(err, ErrMalformedHash) && !errors.Is(err, ErrParamsOutOfRange) {
				t.Fatalf("ParseEncoded(%q) error %v is not a documented sentinel", encoded, err)
			}
			return
		}
		if verr := parsed.Params.Validate(); verr != nil {
			t.Fatalf("accepted out-of-bounds params %+v: %v", parsed.Params, verr)
		}
		if len(parsed.Salt) < MinSaltBytes || len(parsed.Salt) > MaxSaltBytes {
			t.Fatalf("accepted salt of length %d", len(parsed.Salt))
		}
		if len(parsed.Hash) < MinHashBytes || len(parsed.Hash) > MaxHashBytes {
			t.Fatalf("accepted hash of length %d", len(parsed.Hash))
		}
		roundTrip := parsed.String()
		reparsed, err := ParseEncoded(roundTrip)
		if err != nil {
			t.Fatalf("re-encoding %q is not parseable: %v", roundTrip, err)
		}
		if reparsed.String() != roundTrip {
			t.Fatalf("re-encoding is not stable: %q then %q", roundTrip, reparsed.String())
		}
	})
}

// validEncodedForFuzz builds a known-good encoded hash for the fuzz seed
// corpus.
func validEncodedForFuzz() string {
	encoded, err := NewEncodedHash([]byte(fakePassword), fastTestParams, testSalt)
	if err != nil {
		panic(err)
	}
	return encoded.String()
}
